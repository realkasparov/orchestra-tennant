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
	if r.st.HumanBase != human {
		t.Fatal("коммиты человека не замечены")
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

// Агент закоммитил в отсоединённую рабочую копию (ветку забрали в папку во
// время старта задания): возврат ветки отказывает, а не выбрасывает коммит.
func TestReclaimRefusesCommitsOffBranch(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	if res := ex.view(&protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}); !res.OK {
		t.Fatalf("открытие: %+v", res)
	}
	_ = os.WriteFile(filepath.Join(wt, "late.txt"), []byte("x"), 0o644)
	gitc(t, wt, "add", "-A")
	gitc(t, wt, "commit", "-q", "-m", "поздний коммит агента")
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	r.st.WorktreeDir, r.st.BranchName = wt, "task-7-x"
	if err := r.reclaimBranch(); err == nil || !strings.Contains(err.Error(), "нет в ветке") {
		t.Fatalf("коммит вне ветки: %v", err)
	}
	if cur, _ := gitops.CurrentBranch(repo); cur != "task-7-x" {
		t.Fatalf("отказ сдвинул папку: %q", cur)
	}
	// Совет из отказа выполним: влили коммит в ветку — возврат проходит.
	late := gitc(t, wt, "rev-parse", "HEAD")
	gitc(t, repo, "merge", "-q", "--ff-only", late)
	if err := r.reclaimBranch(); err != nil {
		t.Fatalf("после влития: %v", err)
	}
}

// База раунда правки: после оборванного раунда — прежняя (его рабочие
// коммиты свернёт и проверит новый раунд); после законченного или поверх
// коммитов человека — HEAD.
func TestChangeRoundBase(t *testing.T) {
	_, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	base := gitc(t, repo, "rev-parse", "main")
	head := gitc(t, wt, "rev-parse", "HEAD")
	r.st.WorktreeDir, r.st.BranchName, r.st.BaseCommit, r.st.RoundBase = wt, "task-7-x", base, base
	for _, k := range r.reworkKeys() {
		r.st.Stages = append(r.st.Stages, &StageState{Key: k, Round: 1, Status: "done"})
	}
	r.st.stage("execute").Status = "paused"
	if err := r.startChangeRound(); err != nil {
		t.Fatal(err)
	}
	if r.st.RoundBase != base {
		t.Fatalf("после оборванного раунда база %s, ждали прежнюю", short(r.st.RoundBase))
	}
	for _, st := range r.st.Stages {
		st.Status = "done"
	}
	if err := r.startChangeRound(); err != nil {
		t.Fatal(err)
	}
	if r.st.RoundBase != head {
		t.Fatalf("после законченного раунда база %s, ждали HEAD", short(r.st.RoundBase))
	}
}

func TestMergeMessages(t *testing.T) {
	p := &protocol.Message{Text: "правка", Mode: "change"}
	n := &protocol.Message{Text: "вопрос", Mode: "question"}
	if m := mergeMessages(p, n); m.Text != "правка\n\nвопрос" || m.Mode != "change" {
		t.Fatalf("слияние: %+v", m)
	}
	if m := mergeMessages(&protocol.Message{Text: "а", Mode: "auto"}, n); m.Mode != "auto" {
		t.Fatalf("неразобранное с вопросом: %+v", m)
	}
	if mergeMessages(nil, n) != n || mergeMessages(p, nil) != p {
		t.Fatal("одно из двух")
	}
}

// Пауза после «Выполнения», человек закоммитил поверх: база раунда — его
// коммит (его не свернут), а ревью смотрит работу агента от прежней базы и
// знает, какие коммиты — человека.
func TestHumanCommitMidRoundKeepsReview(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	base := gitc(t, repo, "rev-parse", "main")
	r.st.WorktreeDir, r.st.BranchName, r.st.BaseCommit, r.st.RoundBase = wt, "task-7-x", base, base
	if res := ex.view(&protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}); !res.OK {
		t.Fatalf("открытие: %+v", res)
	}
	_ = os.WriteFile(filepath.Join(repo, "human.txt"), []byte("человек"), 0o644)
	gitc(t, repo, "add", "-A")
	gitc(t, repo, "commit", "-q", "-m", "человек")
	human := gitc(t, repo, "rev-parse", "HEAD")
	if err := r.reclaimBranch(); err != nil {
		t.Fatal(err)
	}
	if r.st.RoundBase != human || r.st.ReviewBase != base {
		t.Fatalf("база раунда %s, база ревью %s", short(r.st.RoundBase), short(r.st.ReviewBase))
	}
	if got, _ := r.resolve("$task.round_base", r.manifest("review-task"), "BASE_COMMIT"); got != base {
		t.Fatalf("база ревью по привязке: %s", short(got))
	}
	if note := r.humanCommitsNote(); !strings.Contains(note, short(human)) {
		t.Fatalf("заметка о коммитах человека: %q", note)
	}
	// Сессия шага, продолженная после паузы, узнаёт новую базу свёртки.
	if note := r.humanCommitsResumeNote(); !strings.Contains(note, human) {
		t.Fatalf("заметка продолжению: %q", note)
	}
	// Следующее задание той же таски (новый run, то же состояние) тоже её
	// передаёт.
	r2, _ := testRun(t, plan)
	r2.st = r.st
	if note := r2.humanCommitsResumeNote(); !strings.Contains(note, human) {
		t.Fatalf("заметка в следующем задании: %q", note)
	}
	// Правка сообщением посреди оборванного раунда не теряет базу ревью:
	// работа агента до коммита человека ещё не проверена.
	for _, k := range r.reworkKeys() {
		r.st.Stages = append(r.st.Stages, &StageState{Key: k, Round: 1, Status: "done"})
	}
	r.st.stage("review").Status = "paused"
	if err := r.startChangeRound(); err != nil {
		t.Fatal(err)
	}
	if r.st.ReviewBase != base || r.st.RoundBase != human || r.st.HumanBase != "" {
		t.Fatalf("после правки: база раунда %s, ревью %s, human %s", short(r.st.RoundBase), short(r.st.ReviewBase), short(r.st.HumanBase))
	}
}

// Человек переписал ветку в папке (amend коммита агента): это не повод
// отказывать в возврате ветки.
func TestReclaimAfterHumanAmend(t *testing.T) {
	ex, repo, wt := viewWorld(t)
	if res := ex.view(&protocol.ProjectSpec{Dir: repo, BaseBranch: "main", Branch: "task-7-x"}); !res.OK {
		t.Fatalf("открытие: %+v", res)
	}
	_ = os.WriteFile(filepath.Join(repo, "agent.txt"), []byte("агент, поправлено"), 0o644)
	gitc(t, repo, "commit", "-q", "-a", "--amend", "-m", "агент (поправил человек)")
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	r.st.WorktreeDir, r.st.BranchName = wt, "task-7-x"
	if err := r.reclaimBranch(); err != nil {
		t.Fatalf("возврат после amend: %v", err)
	}
	if cur, _ := gitops.CurrentBranch(wt); cur != "task-7-x" {
		t.Fatalf("ветка не вернулась: %q", cur)
	}
	if gitops.DetachedAt(wt, "task-7-x") != "" {
		t.Fatal("отметка отсоединения не снята")
	}
	// Переписанный коммит агента — не «коммит человека» для ревью, но и не
	// сворачивается: вся ветка — граница.
	head := gitc(t, wt, "rev-parse", "HEAD")
	if len(r.st.HumanCommits) != 0 || r.st.RoundBase != head || r.st.HumanBase != head || r.st.ReviewBase != "" {
		t.Fatalf("после amend: human=%v round=%s humanBase=%s review=%s", r.st.HumanCommits, r.st.RoundBase, r.st.HumanBase, r.st.ReviewBase)
	}
}

// Несостоявшаяся правка откладывается один раз, а не удваивается с каждым
// «Повторить».
func TestKeepChangeDoesNotDuplicate(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.PendingMessage = &protocol.Message{Text: "fix X", Mode: "change"}
	r.st.Feedback = "fix X" // разбор начала задания взял отложенное
	for i := 0; i < 3; i++ {
		if !r.keepChange() {
			t.Fatal("правка не отложена")
		}
	}
	if pm := r.st.PendingMessage; pm == nil || pm.Text != "fix X" {
		t.Fatalf("отложенное: %+v", pm)
	}
}

// Коммит человека между раундами — не работа следующего раунда: пустое
// «Выполнение» после правки — ошибка, а не «закоммитил человек».
func TestEmptyRoundAfterHumanCommitFails(t *testing.T) {
	_, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	head := gitc(t, wt, "rev-parse", "HEAD")
	r.st.WorktreeDir, r.st.BranchName, r.st.RoundBase = wt, "task-7-x", head
	r.st.HumanCommits = []string{head}
	def := r.plan.Step("execute")
	if def == nil {
		t.Fatal("нет шага execute")
	}
	_ = os.WriteFile(filepath.Join(r.st.TaskDir, "step04-execution.md"), []byte("отчёт"), 0o644)
	if err := r.runChecks(def, "", true); err == nil || !strings.Contains(err.Error(), "ни одного коммита") {
		t.Fatalf("пустой раунд прошёл: %v", err)
	}
	// Посреди раунда (HumanBase) — работу закоммитил человек, не ошибка.
	r.st.HumanBase = head
	if err := r.runChecks(def, "", true); err != nil {
		t.Fatalf("работа раунда человека: %v", err)
	}
}

// Ветку забрал в папку исполнитель 0.4.6 — без отметки отсоединения: её
// новые коммиты всё равно коммиты человека, их не сворачивают.
func TestReclaimLegacyDetachKeepsHumanCommits(t *testing.T) {
	_, repo, wt := viewWorld(t)
	if err := gitops.SwitchDetach(wt); err != nil {
		t.Fatal(err)
	}
	if err := gitops.Switch(repo, "task-7-x"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, "human.txt"), []byte("человек"), 0o644)
	gitc(t, repo, "add", "human.txt")
	gitc(t, repo, "commit", "-q", "-m", "человек")
	human := gitc(t, repo, "rev-parse", "HEAD")
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	base := gitc(t, wt, "rev-parse", "HEAD")
	r.st.WorktreeDir, r.st.BranchName, r.st.RoundBase = wt, "task-7-x", base
	if err := r.reclaimBranch(); err != nil {
		t.Fatalf("возврат: %v", err)
	}
	if r.st.HumanBase != human || len(r.st.HumanCommits) != 1 || r.st.RoundBase != human {
		t.Fatalf("коммит человека не учтён: base=%s human=%v round=%s", r.st.HumanBase, r.st.HumanCommits, r.st.RoundBase)
	}
}

// Сообщение, пришедшее к законченному заданию (и застрявшее в очереди к
// концу), сохраняется для «Повторить».
func TestLateMessageKept(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	j := r.job
	j.messages = make(chan *protocol.Message, 8)
	j.deliverMessage(&protocol.Message{Text: "раз", Mode: "change"})
	j.finish("error", "ветку не вернуть")
	j.deliverMessage(&protocol.Message{Text: "два", Mode: "auto"})
	pm := j.State.PendingMessage
	if pm == nil || pm.Text != "раз\n\nдва" || pm.Mode != "change" {
		t.Fatalf("сообщения потеряны: %+v", pm)
	}
}

// 0.4.6 (без отметки) и amend человека в папке: не отказ с советом влить
// старый коммит обратно, а граница по переписанной ветке.
func TestReclaimLegacyAmend(t *testing.T) {
	_, repo, wt := viewWorld(t)
	if err := gitops.SwitchDetach(wt); err != nil {
		t.Fatal(err)
	}
	if err := gitops.Switch(repo, "task-7-x"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, "agent.txt"), []byte("поправил человек"), 0o644)
	gitc(t, repo, "commit", "-q", "-a", "--amend", "-m", "агент (поправил человек)")
	head := gitc(t, repo, "rev-parse", "HEAD")
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	r, _ := testRun(t, plan)
	r.st.WorktreeDir, r.st.BranchName, r.st.RoundBase = wt, "task-7-x", gitc(t, wt, "rev-parse", "HEAD~1")
	if err := r.reclaimBranch(); err != nil {
		t.Fatalf("возврат после amend (0.4.6): %v", err)
	}
	if r.st.RoundBase != head || r.st.HumanBase != head || len(r.st.HumanCommits) != 0 {
		t.Fatalf("граница: round=%s human=%s commits=%v", r.st.RoundBase, r.st.HumanBase, r.st.HumanCommits)
	}
}

// Правка, пришедшая между этапами, прерывает следующий этап сразу.
func TestChangeBetweenStagesCancelsNext(t *testing.T) {
	var c changeRequest
	c.arm(nil)
	c.request()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.arm(cancel)
	if ctx.Err() == nil {
		t.Fatal("этап начался, хотя правка уже ждёт")
	}
}

// После разбора начала задания отложенное (сообщения после паузы) — не часть
// правки: она встаёт после него, даже если текст короткий и входит в неё.
func TestKeepChangeAfterUnhandledPending(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.startDone = true
	r.st.PendingMessage = &protocol.Message{Text: "да", Mode: "auto"}
	r.st.Feedback = "когда будет готово — поправь"
	r.keepChange()
	if pm := r.st.PendingMessage; pm == nil || pm.Text != "да\n\nкогда будет готово — поправь" || pm.Mode != "change" {
		t.Fatalf("отложенное: %+v", pm)
	}
}

// До конца разбора начала задания: отложенное в состоянии — часть правки,
// отложенное этим заданием (после паузы) — дописывается.
func TestKeepChangeAtStartKeepsDeferred(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.PendingMessage = &protocol.Message{Text: "А", Mode: "change"}
	r.st.Feedback = "А"
	r.deferMessage("Б?", "auto")
	r.keepChange()
	if pm := r.st.PendingMessage; pm == nil || pm.Text != "А\n\nБ?" || pm.Mode != "change" {
		t.Fatalf("отложенное: %+v", pm)
	}
}

// После паузы живое сообщение не разбирается мёртвым триажем (он сделал бы
// из вопроса правку), а откладывается как есть; вопросы из очереди — тоже.
func TestPausedMessagesDeferred(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.questions <- question{text: "почему так?"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleMessage(ctx, "а что с тестами?", "auto", true)
	r.answerQueued(ctx)
	if r.change.take() {
		t.Fatal("сообщение после паузы стало правкой")
	}
	pm := r.st.PendingMessage
	if pm == nil || pm.Text != "а что с тестами?\n\nпочему так?" || pm.Mode != "auto" {
		t.Fatalf("отложенное: %+v", pm)
	}
}

// Две правки до начала раунда копятся.
func TestChangesAccumulate(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.requestChange("первая", protocol.Usage{})
	r.requestChange("вторая", protocol.Usage{})
	r.requestChange("вторая", protocol.Usage{}) // повтор разбора — не дубль
	r.requestChange("вто", protocol.Usage{})    // короткое — не теряется
	r.change.take()
	r.keepChange()
	if pm := r.st.PendingMessage; pm == nil || pm.Text != "первая\n\nвторая\n\nвто" {
		t.Fatalf("отложенное: %+v", pm)
	}
}

// Пауза посреди триажа сообщения начала задания: вопрос не становится
// правкой (сообщение сохранит Run с исходным режимом).
func TestStartMessageTriageCutByPause(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleMessage(ctx, "как это запустить?", "auto", false)
	if r.change.take() || r.st.Feedback != "" || r.startChange {
		t.Fatalf("вопрос стал правкой: feedback=%q", r.st.Feedback)
	}
}
