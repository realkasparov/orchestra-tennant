package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/realkasparov/orchestra-tennant/codeindex"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Задание на проект: проверить папку, клонировать или инициализировать
// репозиторий, зарегистрировать существующий, применить настройки. Раньше это
// делал оркестратор на своей машине — другой не было; теперь папка проектов
// на машине исполнителя, и делает это он.

// ProjectHandler исполняет задания на проект. Runner может его не уметь (в
// тестах ядра — заглушка), тогда исполнитель отвечает отказом.
type ProjectHandler interface {
	Project(ctx context.Context, req *protocol.ProjectRequest) *protocol.ProjectResult
}

// handleProject — одно задание на проект: отвечает в отдельной горутине,
// потому что клон может идти минуты, а цикл чтения ждать не должен.
func (e *Executor) handleProject(ctx context.Context, env *protocol.Envelope) {
	var req protocol.ProjectRequest
	if err := json.Unmarshal(env.Body, &req); err != nil {
		return
	}
	go func() {
		res := &protocol.ProjectResult{ReqID: req.ReqID}
		h, ok := e.runner.(ProjectHandler)
		switch {
		case !ok:
			res.Error = "исполнитель не умеет заводить проекты"
		case e.cfg.ProjectsDir == "":
			res.Error = "у машины не задана папка проектов"
		default:
			res = h.Project(ctx, &req)
			res.ReqID = req.ReqID
		}
		if err := e.send(protocol.MsgProjectResult, "", res); err != nil {
			e.logf("ответ на задание о проекте %s: %v", req.ReqID, err)
		}
	}()
}

// Project — реализация задания на проект у Pipeline.
func (p *Pipeline) Project(ctx context.Context, req *protocol.ProjectRequest) *protocol.ProjectResult {
	path, err := p.projectPath(req.Spec.Dir)
	if err != nil {
		return &protocol.ProjectResult{Error: err.Error()}
	}
	switch req.Action {
	case protocol.ProjectCheck:
		return checkProject(path, &req.Spec)
	case protocol.ProjectCreate:
		res := createProject(path, &req.Spec)
		if res.OK {
			p.ensureProjectIndex(ctx, req.Spec.ID, path, &req.Spec)
		}
		return res
	case protocol.ProjectUpdate:
		if _, err := os.Stat(path); err != nil {
			return &protocol.ProjectResult{Error: "папки проекта нет: " + path}
		}
		p.ensureProjectIndex(ctx, req.Spec.ID, path, &req.Spec)
		return &protocol.ProjectResult{OK: true, Path: path}
	}
	return &protocol.ProjectResult{Error: fmt.Sprintf("неизвестное действие %q", req.Action)}
}

// projectPath — абсолютный путь проекта: папка внутри папки проектов. Выход
// из неё («../», абсолютный путь) не допускается: оркестратор описывает
// проект относительно корня, а корень выбрал человек на этой машине. Абсолютный
// путь принимается только к уже существующему репозиторию — так заведены
// проекты до появления машин.
func (p *Pipeline) projectPath(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("не указана папка проекта")
	}
	if filepath.IsAbs(dir) {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("абсолютный путь допустим только к существующему репозиторию: %s", dir)
		}
		return filepath.Clean(dir), nil
	}
	if p.ProjectsDir == "" {
		return "", errors.New("у машины не задана папка проектов")
	}
	root, err := filepath.Abs(p.ProjectsDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, dir)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("папка %q выходит за папку проектов", dir)
	}
	return full, nil
}

// checkProject — сухая проверка: чек-лист без изменений на диске. Та же
// логика, что у создания, чтобы «Валидировать» и «Создать» не расходились.
func checkProject(path string, spec *protocol.ProjectSpec) *protocol.ProjectResult {
	res := &protocol.ProjectResult{OK: true, Path: path, Repo: isRepo(path)}
	add := func(name, detail, level string) {
		res.Checks = append(res.Checks, protocol.ProjectCheckItem{Name: name, Detail: detail, Level: level})
	}
	if strings.TrimSpace(spec.Name) == "" {
		add("Название проекта", "Не заполнено — подставится имя папки", "warn")
	} else {
		add("Название проекта", "Заполнено", "ok")
	}
	// Репозиторий уже есть: его ветки — единственный источник правды о
	// базовой. Ветка по умолчанию подставляется, когда человек не выбрал.
	if res.Repo {
		res.Branches, _ = gitops.Branches(path)
		res.BaseBranch, _ = gitops.DefaultBranch(path)
	}
	branch := spec.BaseBranch
	if branch == "" {
		branch = res.BaseBranch
	}
	if branch == "" {
		branch = "develop"
	}
	switch {
	case spec.Repo != nil && spec.BaseBranch == "":
		add("Базовая ветка", "Не задана — возьмётся ветка по умолчанию репозитория после клона", "ok")
	case gitops.CheckRef(branch) != nil:
		add("Базовая ветка", "Недопустимое имя ветки: "+branch, "err")
	case res.Repo && spec.BaseBranch == "":
		add("Базовая ветка", "Не задана — возьмётся главная ветка репозитория «"+branch+"»", "ok")
	default:
		add("Базовая ветка", branch, "ok")
	}
	if spec.Repo != nil {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			if entries, _ := os.ReadDir(path); len(entries) > 0 {
				add("Папка", "Папка непуста — клонировать в неё нельзя: "+path, "err")
			} else {
				add("Папка", "Пуста — репозиторий будет клонирован сюда: "+path, "ok")
			}
		} else if err == nil {
			add("Папка", "Путь указывает на файл, а не папку", "err")
		} else {
			add("Папка", "Папки нет — репозиторий будет клонирован в "+path, "ok")
		}
	} else if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		add("Папка", "Путь указывает на файл, а не папку", "err")
	} else if err != nil {
		add("Папка", "Папки нет — будет создана в "+path+", в ней инициализируется новый git-репозиторий на ветке «"+branch+"»", "ok")
	} else if gi, gerr := os.Stat(filepath.Join(path, ".git")); gerr == nil && gi.IsDir() {
		if gitops.HasRef(path, branch) {
			add("Папка", "Git-репозиторий найден, ветка «"+branch+"» на месте", "ok")
		} else {
			add("Папка", "Git-репозиторий найден, но ветки «"+branch+"» нет ни локально, ни в origin", "err")
		}
	} else if entries, rerr := os.ReadDir(path); rerr == nil && len(entries) == 0 {
		add("Папка", "Папка пуста — в ней инициализируется новый git-репозиторий на ветке «"+branch+"»", "ok")
	} else {
		add("Папка", "Папка непуста и не является git-репозиторием — выберите другую или инициализируйте git вручную", "err")
	}
	res.Verdict = "ok"
	for _, c := range res.Checks {
		if c.Level == "err" {
			res.Verdict = "err"
			break
		}
		if c.Level == "warn" {
			res.Verdict = "warn"
		}
	}
	return res
}

// createProject заводит проект на диске: клон, init или регистрация
// существующего репозитория. Непустую папку без .git не трогает — опечатка в
// имени не должна молча заводить репозиторий поверх чужих файлов.
func createProject(path string, spec *protocol.ProjectSpec) *protocol.ProjectResult {
	res := &protocol.ProjectResult{Path: path, Repo: isRepo(path)}
	branch := spec.BaseBranch
	if branch == "" {
		branch = "develop"
	}
	if err := gitops.CheckRef(branch); err != nil {
		res.Error = "недопустимая базовая ветка: " + branch
		return res
	}
	if spec.Repo != nil {
		if err := gitops.CloneRepo(spec.Repo.HostURL, spec.Repo.Token, spec.Repo.RepoPath, path); err != nil {
			res.Error = "клонирование не удалось: " + err.Error()
			return res
		}
		if spec.BaseBranch == "" {
			if def, derr := gitops.DefaultBranch(path); derr == nil && def != "" {
				branch = def
			}
		} else if !gitops.HasRef(path, branch) {
			// Названной ветки на хосте нет: клон убирается, иначе непустая
			// папка помешала бы повторить с правильной веткой.
			_ = os.RemoveAll(path)
			res.Error = "в репозитории нет ветки «" + branch + "» — проверьте имя на хосте"
			return res
		}
		res.OK, res.Cloned, res.BaseBranch = true, true, branch
		return res
	}
	if res.Repo {
		// Регистрация существующего репозитория: без явной ветки — его
		// главная; названная должна существовать, иначе первая же таска
		// упадёт на checkout — лучше отказать сейчас.
		if spec.BaseBranch == "" {
			def, derr := gitops.DefaultBranch(path)
			if derr != nil {
				res.Error = "не удалось определить главную ветку репозитория: " + derr.Error()
				return res
			}
			branch = def
		}
		if !gitops.HasRef(path, branch) {
			res.Error = "в репозитории нет ветки «" + branch + "» ни локально, ни в origin"
			return res
		}
		res.OK, res.BaseBranch = true, branch
		return res
	}
	entries, rerr := os.ReadDir(path)
	dirMissing := rerr != nil && os.IsNotExist(rerr)
	if rerr != nil && !dirMissing {
		res.Error = "папка недоступна: " + path
		return res
	}
	if !dirMissing && len(entries) > 0 {
		res.Error = "папка не является git-репозиторием: " + path
		return res
	}
	if err := gitops.InitRepo(path, branch); err != nil {
		res.Error = "не удалось инициализировать репозиторий: " + err.Error()
		return res
	}
	res.Initialized = true
	res.OK, res.BaseBranch = true, branch
	return res
}

// isRepo — в папке есть git-репозиторий.
func isRepo(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && fi.IsDir()
}

// ensureProjectIndex применяет настройки индексации проекта.
func (p *Pipeline) ensureProjectIndex(ctx context.Context, id int64, path string, spec *protocol.ProjectSpec) {
	if p.Index == nil || id == 0 {
		return
	}
	if _, err := p.Index.Ensure(ctx, id, path, spec.IndexMode, codeindex.ExcludeList(spec.IndexExclude)); err != nil && p.Log != nil {
		p.Log("индекс проекта %d: %v", id, err)
	}
}
