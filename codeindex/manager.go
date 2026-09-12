package codeindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/realkasparov/orchestra-tennant/repomap"
)

// autoThresholdBytes — граница режима «авто»: репозитории меньше этого объёма
// кода индекс не получают. На такой базе grep мгновенен, а агент удерживает
// картину целиком — индекс не окупает даже своего существования (design D4).
// ~1.5 МБ кода ≈ 50 тысяч строк.
const autoThresholdBytes = 1_500_000

// Режимы индексации проекта.
const (
	ModeAuto   = "auto"
	ModeAlways = "always"
	ModeNever  = "never"
)

// Status — состояние индекса проекта для UI и для решения «давать ли агенту
// инструмент code_search».
type Status struct {
	Mode     string `json:"mode"`
	Enabled  bool   `json:"enabled"`  // индекс есть и им можно пользоваться
	Building bool   `json:"building"` // идёт индексация
	Queued   int    `json:"queued"`   // файлов в очереди
	Files    int    `json:"files"`
	Chunks   int    `json:"chunks"`
	Model    string `json:"model"`
	Reason   string `json:"reason,omitempty"` // почему индекса нет
	Error    string `json:"error,omitempty"`
}

// Manager владеет индексами всех проектов: открывает базы, решает по порогу,
// нужен ли индекс, и обслуживает очередь (пере)индексации в фоне.
type Manager struct {
	dataDir string
	newEmb  func() Embedding // подменяется в тестах
	// ThresholdBytes — порог режима «авто»; 0 означает autoThresholdBytes.
	ThresholdBytes int64

	mu       sync.Mutex
	projects map[int64]*projectIndex
	// Log — необязательный приёмник сообщений о ходе индексации.
	Log func(projectID int64, text string)
}

type projectIndex struct {
	ix       *Index
	root     string
	mode     string
	excludes []string

	mu       sync.Mutex
	queue    []string
	queued   map[string]bool
	building bool
	enabled  bool
	closed   bool
	reason   string
	lastErr  string
	wake     chan struct{}
}

// wakeLocked будит воркер. Отправка идёт под p.mu вместе с проверкой closed —
// иначе Enqueue, успевший взять указатель до закрытия проекта, отправил бы в
// закрытый канал и уронил сервис.
func (p *projectIndex) wakeLocked() {
	if p.closed {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default: // воркер уже разбужен
	}
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		dataDir:  dataDir,
		newEmb:   func() Embedding { return NewEmbedder("") },
		projects: map[int64]*projectIndex{},
		Log:      func(int64, string) {},
	}
}

func (m *Manager) threshold() int64 {
	if m.ThresholdBytes > 0 {
		return m.ThresholdBytes
	}
	return autoThresholdBytes
}

func (m *Manager) logf(projectID int64, format string, args ...any) {
	if m.Log != nil {
		m.Log(projectID, fmt.Sprintf(format, args...))
	}
}

// Ensure подготавливает индекс проекта: открывает базу, проверяет порог и
// доступность модели и, если индекс нужен и пуст, запускает полную индексацию
// в фоне. Вызывать можно многократно — повторные вызовы дёшевы.
func (m *Manager) Ensure(ctx context.Context, projectID int64, root, mode string, excludes []string) (*Status, error) {
	if mode == "" {
		mode = ModeAuto
	}
	m.mu.Lock()
	p, ok := m.projects[projectID]
	if !ok {
		p = &projectIndex{root: root, mode: mode, queued: map[string]bool{}, wake: make(chan struct{}, 1)}
		m.projects[projectID] = p
	}
	m.mu.Unlock()

	p.mu.Lock()
	p.root, p.mode, p.excludes = root, mode, excludes
	alreadyOpen := p.ix != nil
	p.mu.Unlock()

	if mode == ModeNever {
		m.closeAndRemove(projectID)
		// Выключили осознанно — не оставляем висеть мегабайты векторов.
		_ = os.Remove(m.dbPath(projectID))
		return &Status{Mode: mode, Reason: "индексация выключена в настройках проекта"}, nil
	}
	if root == "" {
		return m.disabled(p, mode, "у проекта нет рабочей копии"), nil
	}

	files, err := m.candidates(root, excludes)
	if err != nil {
		return &Status{Mode: mode, Error: err.Error()}, err
	}
	if mode == ModeAuto && codeBytes(root, files) < m.threshold() {
		m.closeAndRemove(projectID)
		return &Status{Mode: mode, Reason: "проект меньше порога — на такой базе достаточно grep"}, nil
	}

	emb := m.newEmb()
	if e, isOllama := emb.(*Embedder); isOllama {
		if err := e.Available(ctx); err != nil {
			st := m.disabled(p, mode, err.Error())
			st.Model = e.Model
			return st, nil
		}
	}

	p.mu.Lock()
	p.reason = ""
	p.mu.Unlock()

	if !alreadyOpen {
		ix, err := Open(m.dbPath(projectID), root, emb)
		if err != nil {
			return &Status{Mode: mode, Error: err.Error()}, err
		}
		p.mu.Lock()
		p.ix, p.enabled = ix, true
		p.mu.Unlock()
		go p.worker(m, projectID)
	}

	st, _ := p.ix.Stats()
	if st.Files == 0 {
		m.logf(projectID, "Индексация кода: %d файлов, идёт в фоне.", len(files))
		m.Enqueue(projectID, files)
	}
	return m.Status(projectID), nil
}

// Enqueue ставит файлы в очередь (пере)индексации. Пути — относительно корня.
func (m *Manager) Enqueue(projectID int64, rels []string) {
	m.mu.Lock()
	p := m.projects[projectID]
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	for _, rel := range rels {
		if !p.queued[rel] {
			p.queued[rel] = true
			p.queue = append(p.queue, rel)
		}
	}
	p.wakeLocked()
	p.mu.Unlock()
}

// EnqueueChanged фильтрует список изменённых файлов (например, из git diff) по
// правилам индексации и ставит подходящие в очередь.
func (m *Manager) EnqueueChanged(projectID int64, rels []string) {
	m.mu.Lock()
	p := m.projects[projectID]
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	excludes := p.excludes
	p.mu.Unlock()

	var keep []string
	for _, rel := range rels {
		if repomap.Supported(rel) && !excluded(rel, excludes) {
			keep = append(keep, rel)
		}
	}
	if len(keep) > 0 {
		m.Enqueue(projectID, keep)
	}
}

// Search выполняет поиск по индексу проекта.
func (m *Manager) Search(ctx context.Context, projectID int64, query string, limit int, module string) ([]Result, error) {
	m.mu.Lock()
	p := m.projects[projectID]
	m.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("индекс проекта не открыт")
	}
	p.mu.Lock()
	ix := p.ix
	p.mu.Unlock()
	if ix == nil {
		return nil, fmt.Errorf("индекс проекта не построен")
	}
	return ix.Search(ctx, query, limit, module)
}

// Status возвращает текущее состояние индекса проекта.
func (m *Manager) Status(projectID int64) *Status {
	m.mu.Lock()
	p := m.projects[projectID]
	m.mu.Unlock()
	if p == nil {
		return &Status{Mode: ModeAuto, Reason: "индекс не открывался"}
	}
	p.mu.Lock()
	st := &Status{
		Mode: p.mode, Enabled: p.enabled, Building: p.building,
		Queued: len(p.queue), Error: p.lastErr, Reason: p.reason,
	}
	ix := p.ix
	p.mu.Unlock()
	if ix != nil {
		if s, err := ix.Stats(); err == nil {
			st.Files, st.Chunks = s.Files, s.Chunks
		}
	}
	return st
}

// Close закрывает все индексы (вызывается при остановке сервиса).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.projects {
		p.mu.Lock()
		if p.ix != nil {
			_ = p.ix.Close()
			p.ix = nil
		}
		if !p.closed {
			p.closed = true
			close(p.wake)
		}
		p.mu.Unlock()
		delete(m.projects, id)
	}
}

func (m *Manager) closeAndRemove(projectID int64) {
	m.mu.Lock()
	p := m.projects[projectID]
	delete(m.projects, projectID)
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.ix != nil {
		_ = p.ix.Close()
		p.ix = nil
	}
	p.enabled = false
	if !p.closed {
		p.closed = true
		close(p.wake) // воркер выходит из range
	}
	p.mu.Unlock()
}

// disabled запоминает причину отсутствия индекса, чтобы её видел и следующий
// вызов Status (например, из UI настроек проекта), а не только Ensure.
func (m *Manager) disabled(p *projectIndex, mode, reason string) *Status {
	p.mu.Lock()
	p.reason, p.enabled = reason, false
	p.mu.Unlock()
	return &Status{Mode: mode, Reason: reason}
}

func (m *Manager) dbPath(projectID int64) string {
	return filepath.Join(m.dataDir, "index", fmt.Sprintf("project-%d.db", projectID))
}

// candidates — файлы проекта, пригодные для индексации.
func (m *Manager) candidates(root string, excludes []string) ([]string, error) {
	all, err := repomap.ListCodeFiles(root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, rel := range all {
		if !excluded(rel, excludes) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// worker обслуживает очередь проекта: индексация идёт в фоне и никогда не
// блокирует пайплайн — устаревшие результаты поиска помечаются Stale.
func (p *projectIndex) worker(m *Manager, projectID int64) {
	for range p.wake {
		for {
			p.mu.Lock()
			if len(p.queue) == 0 || p.ix == nil {
				p.building = false
				p.mu.Unlock()
				break
			}
			// Батч, чтобы не держать блокировку на весь прогон.
			const batch = 20
			n := min(batch, len(p.queue))
			job := append([]string(nil), p.queue[:n]...)
			p.queue = p.queue[n:]
			for _, rel := range job {
				delete(p.queued, rel)
			}
			p.building = true
			ix := p.ix
			p.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			_, err := ix.IndexFiles(ctx, job)
			cancel()

			p.mu.Lock()
			if err != nil {
				p.lastErr = err.Error()
			} else {
				p.lastErr = ""
			}
			remaining := len(p.queue)
			p.mu.Unlock()
			if err != nil {
				m.logf(projectID, "Индексация кода: ошибка — %s", err)
				// Не крутим очередь вхолостую на повторяющейся ошибке.
				time.Sleep(2 * time.Second)
			}
			if remaining == 0 {
				if st, serr := ix.Stats(); serr == nil {
					m.logf(projectID, "Индекс кода готов: %d файлов, %d фрагментов.", st.Files, st.Chunks)
				}
			}
		}
	}
}

// excluded проверяет путь против глоб-исключений проекта.
func excluded(rel string, patterns []string) bool {
	rel = filepath.ToSlash(rel)
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
		// Глоб без слэша применяем и к имени файла, и к каждому сегменту пути:
		// «*.gen.go» и «migrations» должны работать интуитивно.
		if !strings.Contains(pat, "/") {
			if ok, _ := filepath.Match(pat, filepath.Base(rel)); ok {
				return true
			}
			for _, seg := range strings.Split(rel, "/") {
				if seg == pat {
					return true
				}
			}
		} else if strings.HasPrefix(rel, strings.TrimSuffix(pat, "/")+"/") {
			return true
		}
	}
	return false
}

// codeBytes считает объём кода — по нему режим «авто» решает, нужен ли индекс.
func codeBytes(root string, rels []string) int64 {
	var total int64
	for _, rel := range rels {
		if fi, err := os.Stat(filepath.Join(root, rel)); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// ExcludeList разбирает глобы исключений: по одному в строке (так их удобнее
// править в textarea), пустые строки игнорируются.
func ExcludeList(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
