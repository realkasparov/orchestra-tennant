package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/realkasparov/orchestra-tennant/gitops"
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

// Существующий репозиторий: проверка отдаёт его ветки и главную ветку, без
// явной ветки регистрация берёт главную, а несуществующая ветка — отказ.
func TestProjectExistingRepoBranches(t *testing.T) {
	p, root := projectPipeline(t)
	dir := filepath.Join(root, "lib")
	for _, args := range [][]string{
		{"init", "-q", "-b", "master", dir},
		{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", dir, "branch", "feature"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	check := p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCheck, Spec: protocol.ProjectSpec{Name: "lib", Dir: "lib"}})
	if !check.Repo || check.BaseBranch != "master" || check.Verdict != "ok" {
		t.Fatalf("проверка: %+v", check)
	}
	if got := strings.Join(check.Branches, ","); got != "feature,master" {
		t.Fatalf("ветки: %q", got)
	}
	res := p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCreate, Spec: protocol.ProjectSpec{Name: "lib", Dir: "lib"}})
	if !res.OK || res.BaseBranch != "master" || res.Initialized {
		t.Fatalf("регистрация без ветки: %+v", res)
	}
	res = p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCreate, Spec: protocol.ProjectSpec{Name: "lib", Dir: "lib", BaseBranch: "nope"}})
	if res.OK || !strings.Contains(res.Error, "nope") {
		t.Fatalf("регистрация с несуществующей веткой прошла: %+v", res)
	}
	check = p.Project(context.Background(), &protocol.ProjectRequest{Action: protocol.ProjectCheck, Spec: protocol.ProjectSpec{Name: "lib", Dir: "lib", BaseBranch: "nope"}})
	if check.Verdict != "err" {
		t.Fatalf("проверка пропустила несуществующую ветку: %+v", check)
	}
}

// gitc — git с подписью коммитов (у тестовой машины может не быть своей).
func gitc(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

// viewWorld — папка проекта на main и worktree таски 7 на её ветке с
// коммитом агента.
func viewWorld(t *testing.T) (ex *Executor, repo, wt string) {
	t.Helper()
	ex, err := New(Config{DeviceKey: "k", JournalDir: t.TempDir(), Log: func(string, ...any) {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(t.TempDir(), "proj")
	if err := gitops.InitRepo(repo, "main"); err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(filepath.Dir(repo), "proj-agent-worktrees", "7")
	if _, err := gitops.AddWorktree(repo, wt, "task-7-x", "main"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(wt, "agent.txt"), []byte("агент"), 0o644)
	gitc(t, wt, "add", "-A")
	gitc(t, wt, "commit", "-q", "-m", "агент")
	return ex, repo, wt
}

// Открытие: ветка таски переезжает в папку проекта настоящей веткой,
// worktree отпускает её на том же коммите; базовая ветка выкладывается как
// есть; грязная папка и идущая в ней таска — отказ.
func TestProjectView(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	ex.jobs["j8"] = &Job{ID: "j8", Plan: &protocol.Plan{TaskID: 8, Workspace: "folder", Project: protocol.Project{Path: repo}}, ex: ex,
		State: &TaskState{WorktreeDir: repo, BranchName: "task-8-y"}}
	spec := protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}
	if res := ex.view(&spec); res.OK || !strings.Contains(res.Error, "#8") {
		t.Fatalf("занятая папка: %+v", res)
	}
	delete(ex.jobs, "j8")
	_ = os.WriteFile(filepath.Join(repo, "junk"), []byte("x"), 0o644)
	if res := ex.view(&spec); res.OK || !strings.Contains(res.Error, "junk") {
		t.Fatalf("грязная папка: %+v", res)
	}
	_ = os.Remove(filepath.Join(repo, "junk"))
	want, _ := gitops.HeadSHA(wt)
	if res := ex.view(&spec); !res.OK {
		t.Fatalf("открытие: %+v", res)
	}
	head, _ := gitops.HeadSHA(repo)
	if cur, _ := gitops.CurrentBranch(repo); cur != "task-7-x" || head != want {
		t.Fatalf("папка не на ветке таски: %s %s/%s", cur, head, want)
	}
	if cur, _ := gitops.CurrentBranch(wt); cur != "HEAD" {
		t.Fatalf("worktree не отпустил ветку: %q", cur)
	}
	if h, _ := gitops.HeadSHA(wt); h != want {
		t.Fatal("worktree сдвинулся с коммита")
	}
	// Повторное открытие — без действий.
	if res := ex.view(&spec); !res.OK {
		t.Fatalf("повторное открытие: %+v", res)
	}
	spec.Branch = "main"
	if res := ex.view(&spec); !res.OK {
		t.Fatalf("возврат на main: %+v", res)
	}
	if cur, _ := gitops.CurrentBranch(repo); cur != "main" {
		t.Fatalf("папка не на main: %q", cur)
	}
}

// Отказы открытия: агент работает над таской; в её worktree незакоммиченное.
func TestProjectViewRefusals(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	spec := protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}
	ex.jobs["j7"] = &Job{ID: "j7", Plan: &protocol.Plan{TaskID: 7, Workspace: "worktree", Project: protocol.Project{Path: repo}}, ex: ex,
		State: &TaskState{WorktreeDir: wt, BranchName: "task-7-x"}}
	if res := ex.view(&spec); res.OK || !strings.Contains(res.Error, "агент работает над таской #7") {
		t.Fatalf("идущая таска: %+v", res)
	}
	ex.jobs["j7"].orphan = true // пауза
	_ = os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("x"), 0o644)
	if res := ex.view(&spec); res.OK || !strings.Contains(res.Error, "wip.txt") {
		t.Fatalf("незакоммиченное агента: %+v", res)
	}
	if cur, _ := gitops.CurrentBranch(wt); cur != "task-7-x" {
		t.Fatalf("отказ сдвинул ветку: %q", cur)
	}
}

func TestCreateProjectAttachesExistingRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snake")
	if err := gitops.InitRepo(dir, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@gitlab.com:nov_aleks/snake-game.git").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	repo := &protocol.ProjectRepo{HostURL: "https://gitlab.com", RepoPath: "nov_aleks/snake-game"}
	// Проверка: тот же репозиторий — ok, клон не нужен.
	chk := checkProject(dir, &protocol.ProjectSpec{Name: "snake", Dir: dir, Repo: repo})
	if chk.Verdict != "ok" {
		t.Fatalf("check verdict = %s: %+v", chk.Verdict, chk.Checks)
	}
	res := createProject(dir, &protocol.ProjectSpec{Name: "snake", Dir: dir, Repo: repo})
	if !res.OK || !res.Attached || res.Cloned || res.BaseBranch != "main" {
		t.Fatalf("attach: %+v", res)
	}
	// Другой репозиторий того же хоста — отказ и в проверке, и в создании.
	other := &protocol.ProjectRepo{HostURL: "https://gitlab.com", RepoPath: "nov_aleks/other"}
	if chk := checkProject(dir, &protocol.ProjectSpec{Name: "snake", Dir: dir, Repo: other}); chk.Verdict != "err" {
		t.Fatalf("check other verdict = %s", chk.Verdict)
	}
	if res := createProject(dir, &protocol.ProjectSpec{Name: "snake", Dir: dir, Repo: other}); res.OK || !strings.Contains(res.Error, "другой репозиторий") {
		t.Fatalf("other: %+v", res)
	}
	// Несуществующая ветка у привязки — отказ.
	if res := createProject(dir, &protocol.ProjectSpec{Name: "snake", Dir: dir, Repo: repo, BaseBranch: "nope"}); res.OK {
		t.Fatalf("branch nope accepted: %+v", res)
	}
}

// Агент → человек → агент: человек открыл таску в папке и закоммитил свою
// правку; перед доработкой ветка возвращается в worktree со всеми
// коммитами, база раунда — коммит человека, и свёртка «Выполнения» от неё
// оставляет в истории три коммита.
func TestBranchHandoverRoundTrip(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	spec := protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}
	if res := ex.view(&spec); !res.OK {
		t.Fatalf("открытие: %+v", res)
	}
	_ = os.WriteFile(filepath.Join(repo, "human.txt"), []byte("человек"), 0o644)
	gitc(t, repo, "add", "-A")
	gitc(t, repo, "commit", "-q", "-m", "человек")
	human, _ := gitops.HeadSHA(repo)

	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	r.st.WorktreeDir, r.st.BranchName, r.st.BaseCommit = wt, "task-7-x", gitc(t, repo, "rev-parse", "main")

	// Незакоммиченное в папке на ветке таски — отказ, ничего не тронуто.
	_ = os.WriteFile(filepath.Join(repo, "draft.txt"), []byte("x"), 0o644)
	if err := r.reclaimBranch(); err == nil || !strings.Contains(err.Error(), "draft.txt") {
		t.Fatalf("незакоммиченное в папке: %v", err)
	}
	if cur, _ := gitops.CurrentBranch(repo); cur != "task-7-x" {
		t.Fatalf("отказ сдвинул папку: %q", cur)
	}
	_ = os.Remove(filepath.Join(repo, "draft.txt"))

	if err := r.reclaimBranch(); err != nil {
		t.Fatal(err)
	}
	if cur, _ := gitops.CurrentBranch(repo); cur != "main" {
		t.Fatalf("папка не на базовой ветке: %q", cur)
	}
	if cur, _ := gitops.CurrentBranch(wt); cur != "task-7-x" {
		t.Fatalf("ветка не вернулась в worktree: %q", cur)
	}
	if h, _ := gitops.HeadSHA(wt); h != human {
		t.Fatal("worktree без коммита человека")
	}
	// Повторный возврат — ничего не делает.
	if err := r.reclaimBranch(); err != nil {
		t.Fatal(err)
	}

	// Раунд правки: база раунда — HEAD worktree; BASE «Выполнения» — она,
	// даже по старой привязке к базе таски; ревью — по-прежнему база таски.
	r.st.RoundBase = human
	em := r.manifest("execute-plan")
	if got, _ := r.resolve("$task.base_commit", em, "BASE"); got != human {
		t.Fatalf("BASE выполнения: %s, ждали базу раунда", got)
	}
	// Ревью — работа раунда: коммиты человека не предмет его правок.
	if got, _ := r.resolve("$task.base_commit", r.manifest("review-task"), "BASE_COMMIT"); got != human {
		t.Fatalf("база ревью: %s, ждали базу раунда", got)
	}
	// Инструкции по проверке нужна вся ветка.
	if got, _ := r.resolve("$task.base_commit", r.manifest("handoff-notes"), "BASE_COMMIT"); got != r.st.BaseCommit {
		t.Fatalf("база инструкции: %s", got)
	}
	for _, f := range []string{"w1", "w2"} {
		_ = os.WriteFile(filepath.Join(wt, f), []byte(f), 0o644)
		gitc(t, wt, "add", "-A")
		gitc(t, wt, "commit", "-q", "-m", "WIP "+f)
	}
	gitc(t, wt, "reset", "--soft", human)
	gitc(t, wt, "commit", "-q", "-m", "агент, раунд 2")
	if log := gitc(t, wt, "log", "--format=%s", "main..task-7-x"); log != "агент, раунд 2\nчеловек\nагент" {
		t.Fatalf("история ветки:\n%s", log)
	}
}

// Имя ветки даётся один раз: в следующих раундах «Создание ветки» его не
// меняет, даже если анализ описал правку иначе.
func TestBranchNameStableAcrossRounds(t *testing.T) {
	_, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	r.st.WorktreeDir, r.st.BranchName, r.st.Reference, r.st.BranchSlug = wt, "task-7-x", "task-7", "done-output"
	st := &StageState{Key: "branch", Round: 2}
	r.st.Stages = append(r.st.Stages, &StageState{Key: "branch", Round: 1, Status: "done"}, st)
	if err := r.stepBranch(st); err != nil {
		t.Fatal(err)
	}
	if cur, _ := gitops.CurrentBranch(wt); cur != "task-7-x" || r.st.BranchName != "task-7-x" {
		t.Fatalf("ветка переименована во втором раунде: %s / %s", cur, r.st.BranchName)
	}
}
