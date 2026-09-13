package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

func projectPipeline(t *testing.T) (*Pipeline, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Pipeline{DataDir: t.TempDir(), ProjectsDir: root}, root
}

// Проверка не трогает диск и отвечает чек-листом; создание инициализирует
// репозиторий и возвращает абсолютный путь.
func TestProjectCheckThenCreate(t *testing.T) {
	p, root := projectPipeline(t)
	spec := protocol.ProjectSpec{Name: "shop", Dir: "shop", BaseBranch: "main"}
	res := p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCheck, Spec: spec})
	if !res.OK || res.Verdict != "ok" || len(res.Checks) != 3 {
		t.Fatalf("проверка: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "shop")); err == nil {
		t.Fatal("проверка создала папку")
	}
	res = p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCreate, Spec: spec})
	if !res.OK || !res.Initialized || res.Path != filepath.Join(root, "shop") {
		t.Fatalf("создание: %+v", res)
	}
	if fi, err := os.Stat(filepath.Join(root, "shop", ".git")); err != nil || !fi.IsDir() {
		t.Fatal("репозиторий не инициализирован")
	}
	// Повторное создание существующего репозитория — регистрация, не init.
	res = p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCreate, Spec: spec})
	if !res.OK || res.Initialized {
		t.Fatalf("регистрация: %+v", res)
	}
}

// Непустая папка без .git — ошибка и в проверке, и в создании; диск не тронут.
func TestProjectRefusesNonEmptyFolder(t *testing.T) {
	p, root := projectPipeline(t)
	_ = os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("x"), 0o644)
	spec := protocol.ProjectSpec{Name: "docs", Dir: "docs", BaseBranch: "main"}
	res := p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCheck, Spec: spec})
	if res.Verdict != "err" {
		t.Fatalf("проверка пропустила непустую папку: %+v", res)
	}
	res = p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCreate, Spec: spec})
	if res.OK || res.Error == "" {
		t.Fatalf("создание поверх файлов: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", ".git")); err == nil {
		t.Fatal("репозиторий создан поверх чужих файлов")
	}
}

// Папка не выходит за корень проектов; абсолютный путь — только к готовому
// репозиторию.
func TestProjectPathStaysInRoot(t *testing.T) {
	p, root := projectPipeline(t)
	for _, bad := range []string{"", "../escape", "..", "/etc"} {
		if _, err := p.projectPath(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
	if got, err := p.projectPath("a/b"); err != nil || got != filepath.Join(root, "a", "b") {
		t.Errorf("вложенная папка: %q %v", got, err)
	}
	repo := filepath.Join(t.TempDir(), "legacy")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	if got, err := p.projectPath(repo); err != nil || got != repo {
		t.Errorf("абсолютный путь к репозиторию: %q %v", got, err)
	}
}
