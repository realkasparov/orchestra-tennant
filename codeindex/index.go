package codeindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  path TEXT NOT NULL,
  module TEXT NOT NULL DEFAULT '',
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  symbols TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  file_hash TEXT NOT NULL,
  vec BLOB);
CREATE INDEX IF NOT EXISTS chunks_path ON chunks(path);
CREATE TABLE IF NOT EXISTS files(
  path TEXT PRIMARY KEY,
  file_hash TEXT NOT NULL,
  indexed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  body, symbols, path, content='chunks', content_rowid='id');
`

// Index — индекс кода одного проекта: SQLite-файл с чанками, их векторами и
// полнотекстовым индексом. Векторы лежат BLOB-ом и перебираются в Go: чистый
// Go-драйвер SQLite не грузит C-расширения вроде sqlite-vec (см. design D5).
type Index struct {
	db   *sql.DB
	emb  Embedding
	root string // корень репозитория, к которому относятся пути чанков

	mu sync.Mutex // сериализует запись: индексация идёт из фоновых горутин
}

// Embedding — источник векторов: Ollama в проде, детерминированная заглушка в
// тестах. Интерфейс держит индекс независимым от способа получения эмбеддингов.
type Embedding interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery считает вектор поискового запроса. Отдельный метод, потому
	// что модели поиска по коду асимметричны: запрос и документ кодируются
	// по-разному (см. queryInstruct).
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
}

// Open открывает (создавая при необходимости) индекс проекта.
// dbPath — файл индекса, root — корень рабочей копии.
func Open(dbPath, root string, emb Embedding) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("создание индекса: %w", err)
	}
	if emb == nil {
		emb = NewEmbedder("")
	}
	return &Index{db: db, emb: emb, root: root}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// Stats — сводка состояния индекса для UI.
type Stats struct {
	Files  int    `json:"files"`
	Chunks int    `json:"chunks"`
	Commit string `json:"indexed_commit"`
}

func (ix *Index) Stats() (Stats, error) {
	var s Stats
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&s.Files); err != nil {
		return s, err
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&s.Chunks); err != nil {
		return s, err
	}
	_ = ix.db.QueryRow(`SELECT value FROM meta WHERE key='indexed_commit'`).Scan(&s.Commit)
	return s, nil
}

// SetCommit запоминает коммит, на котором индекс актуален.
func (ix *Index) SetCommit(sha string) error {
	_, err := ix.db.Exec(`INSERT INTO meta(key, value) VALUES('indexed_commit', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sha)
	return err
}

// IndexFiles индексирует или переиндексирует перечисленные файлы (пути
// относительно корня). Файлы, чей хэш совпадает с сохранённым, пропускаются —
// поэтому повторный вызов после коммита, не менявшего файл, бесплатен.
// Удалённые с диска файлы вычищаются из индекса.
func (ix *Index) IndexFiles(ctx context.Context, rels []string) (indexed int, err error) {
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return indexed, err
		}
		data, rerr := os.ReadFile(filepath.Join(ix.root, rel))
		if rerr != nil {
			// Файл удалён или недоступен — его чанки больше не актуальны.
			if err := ix.deleteFile(rel); err != nil {
				return indexed, err
			}
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:8])

		var known string
		_ = ix.db.QueryRow(`SELECT file_hash FROM files WHERE path=?`, rel).Scan(&known)
		if known == hash {
			continue
		}
		chunks := ChunkFile(rel, data)
		if len(chunks) == 0 {
			if err := ix.deleteFile(rel); err != nil {
				return indexed, err
			}
			continue
		}
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text()
		}
		vecs, eerr := ix.emb.Embed(ctx, texts)
		if eerr != nil {
			return indexed, eerr
		}
		if err := ix.replaceFile(rel, hash, chunks, vecs); err != nil {
			return indexed, err
		}
		indexed++
	}
	return indexed, nil
}

func (ix *Index) deleteFile(rel string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // откат после успешного Commit — no-op
	if err := deleteChunksTx(tx, rel); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE path=?`, rel); err != nil {
		return err
	}
	return tx.Commit()
}

func (ix *Index) replaceFile(rel, hash string, chunks []Chunk, vecs [][]float32) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // откат после успешного Commit — no-op

	if err := deleteChunksTx(tx, rel); err != nil {
		return err
	}
	module := moduleOf(rel)
	for i, c := range chunks {
		syms := strings.Join(c.Symbols, " ")
		res, err := tx.Exec(`INSERT INTO chunks(path, module, start_line, end_line, symbols, body, file_hash, vec)
			VALUES(?,?,?,?,?,?,?,?)`,
			c.Path, module, c.StartLine, c.EndLine, syms, c.Body, hash, encodeVec(vecs[i]))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		// content-таблица FTS5 требует явной синхронизации.
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid, body, symbols, path) VALUES(?,?,?,?)`,
			id, c.Body, syms, c.Path); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO files(path, file_hash, indexed_at) VALUES(?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(path) DO UPDATE SET file_hash=excluded.file_hash, indexed_at=CURRENT_TIMESTAMP`,
		rel, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteChunksTx(tx *sql.Tx, rel string) error {
	rows, err := tx.Query(`SELECT id FROM chunks WHERE path=?`, rel)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE rowid=?`, id); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM chunks WHERE path=?`, rel)
	return err
}

// moduleOf — верхний каталог пути: дешёвая метка «модуля» для фильтра поиска.
func moduleOf(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return ""
	}
	// internal/api/server.go → internal/api; web/src/x.ts → web/src
	if len(parts) > 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// Result — одна находка поиска.
type Result struct {
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Symbols   []string `json:"symbols"`
	Body      string   `json:"body"`
	Score     float64  `json:"score"`
	// Stale — файл на диске отличается от проиндексированного: содержимое
	// чанка устарело, читателю нужно перечитать файл напрямую.
	Stale bool `json:"stale"`
}

// Search выполняет гибридный поиск: плотные векторы + BM25 (FTS5), слияние
// рангов по RRF. module (если задан) ограничивает выдачу подкаталогом.
func (ix *Index) Search(ctx context.Context, query string, limit int, module string) ([]Result, error) {
	if limit <= 0 {
		limit = 8
	}
	const pool = 50 // сколько кандидатов берём с каждой стороны до слияния

	dense, err := ix.denseSearch(ctx, query, pool, module)
	if err != nil {
		return nil, err
	}
	lexical, err := ix.lexicalSearch(query, pool, module)
	if err != nil {
		return nil, err
	}

	// Reciprocal Rank Fusion: устойчиво сливает списки с несопоставимыми
	// шкалами (косинус и BM25) без подбора весов.
	const rrfK = 60.0
	scores := map[int64]float64{}
	for rank, id := range dense {
		scores[id] += 1.0 / (rrfK + float64(rank+1))
	}
	for rank, id := range lexical {
		scores[id] += 1.0 / (rrfK + float64(rank+1))
	}
	if len(scores) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // детерминированность при равных очках
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ix.hydrate(ids, scores)
}

// denseSearch перебирает векторы. Перебор, а не ANN-индекс: на десятках тысяч
// чанков это единицы миллисекунд — на порядки меньше одного вызова агента.
func (ix *Index) denseSearch(ctx context.Context, query string, limit int, module string) ([]int64, error) {
	q, err := ix.emb.EmbedQuery(ctx, query)
	if err != nil || len(q) == 0 {
		return nil, err
	}

	sqlText := `SELECT id, vec FROM chunks`
	var args []any
	if module != "" {
		sqlText += ` WHERE module LIKE ?`
		args = append(args, module+"%")
	}
	rows, err := ix.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		id    int64
		score float32
	}
	var all []scored
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		all = append(all, scored{id: id, score: dot(q, decodeVec(blob))})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]int64, len(all))
	for i, s := range all {
		out[i] = s.id
	}
	return out, nil
}

// lexicalSearch — BM25 через FTS5: он выигрывает там, где важно точное имя.
func (ix *Index) lexicalSearch(query string, limit int, module string) ([]int64, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	sqlText := `SELECT f.rowid FROM chunks_fts f JOIN chunks c ON c.id = f.rowid
		WHERE chunks_fts MATCH ?`
	args := []any{match}
	if module != "" {
		sqlText += ` AND c.module LIKE ?`
		args = append(args, module+"%")
	}
	sqlText += ` ORDER BY bm25(chunks_fts) LIMIT ?`
	args = append(args, limit)

	rows, err := ix.db.Query(sqlText, args...)
	if err != nil {
		// Неудачный синтаксис MATCH не должен ронять поиск: плотная половина
		// гибрида отработает и без лексической.
		return nil, nil //nolint:nilerr // деградация вместо отказа
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ftsQuery превращает произвольный текст в безопасный OR-запрос FTS5:
// пользовательский запрос содержит скобки, дефисы и кавычки, которые синтаксис
// MATCH трактует как операторы.
//
// Разбор — по unicode.IsLetter, а не по латинице: комментарии и запросы здесь
// по-русски, и ASCII-фильтр молча обнулял бы лексическую половину гибрида.
func ftsQuery(q string) string {
	var terms []string
	for _, f := range strings.FieldsFunc(q, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		if utf8.RuneCountInString(f) < 2 || len(terms) >= 16 {
			continue
		}
		terms = append(terms, `"`+f+`"`)
	}
	return strings.Join(terms, " OR ")
}

// hydrate достаёт тела найденных чанков и отмечает устаревшие.
func (ix *Index) hydrate(ids []int64, scores map[int64]float64) ([]Result, error) {
	out := make([]Result, 0, len(ids))
	// Хэш файла проверяем один раз на файл, а не на чанк.
	fresh := map[string]bool{}
	for _, id := range ids {
		var r Result
		var syms, hash string
		err := ix.db.QueryRow(`SELECT path, start_line, end_line, symbols, body, file_hash
			FROM chunks WHERE id=?`, id).Scan(&r.Path, &r.StartLine, &r.EndLine, &syms, &r.Body, &hash)
		if err != nil {
			continue
		}
		if syms != "" {
			r.Symbols = strings.Fields(syms)
		}
		r.Score = scores[id]
		ok, seen := fresh[r.Path]
		if !seen {
			ok = ix.fileMatches(r.Path, hash)
			fresh[r.Path] = ok
		}
		r.Stale = !ok
		out = append(out, r)
	}
	return out, nil
}

// fileMatches сверяет хэш файла на диске с проиндексированным.
func (ix *Index) fileMatches(rel, hash string) bool {
	data, err := os.ReadFile(filepath.Join(ix.root, rel))
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]) == hash
}
