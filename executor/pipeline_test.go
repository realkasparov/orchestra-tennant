package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/realkasparov/orchestra-tennant/gitops"
	"testing"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// testRun — прогон над заглушкой исполнителя: события копятся в памяти.
func testRun(t *testing.T, plan *protocol.Plan) (*run, *[]protocol.Event) {
	t.Helper()
	ex, err := New(Config{DeviceKey: "k", JournalDir: t.TempDir(), Log: func(string, ...any) {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	j := &Job{ID: "j", Plan: plan, ex: ex, State: &TaskState{TaskDir: t.TempDir()},
		cont: make(chan protocol.Continue, 1), answers: make(chan *protocol.Answer, 8)}
	// Перехват отправки: соединения нет, и события остаются в pending; читаем
	// оттуда.
	r := &run{p: &Pipeline{DataDir: t.TempDir()}, job: j, plan: plan, st: j.State}
	return r, &events
}

func pending(j *Job) []*protocol.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]*protocol.Event(nil), j.pending...)
}

func fullPlan() *protocol.Plan {
	return &protocol.Plan{
		SchemaVersion: protocol.SchemaVersion, TaskID: 7, Title: "Змейка", Prompt: "сделай",
		Project: protocol.Project{ID: 1, Name: "demo", Path: "/repo", BaseBranch: "main"},
		Stages: []protocol.Stage{
			{Key: "import", Skill: "import-gitlab", Model: "claude-haiku-4-5-20251001", Effort: "low"},
			{Key: "analyze", Skill: "analyze-task", Model: "claude-fable-5", Effort: "high"},
			{Key: "decompose", Virtual: true},
			{Key: "err_work", Skill: "plan-review", Model: "claude-sonnet-5", Effort: "high", Passes: 2},
			{Key: "branch"},
			{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "high"},
			{Key: "review", Skill: "review-task", Model: "claude-sonnet-5", Effort: "high"},
		},
		Continuity: "per_stage", Workspace: "worktree", StageTimeout: protocol.Seconds(1800),
	}
}

// Бюджет: до лимита — тихо; на лимите — errBudget; после ack — снова тихо.
func TestCheckBudget(t *testing.T) {
	plan := fullPlan()
	plan.BudgetUSD = 1.0
	r, _ := testRun(t, plan)
	r.st.addRound(r.stageKeys())
	if err := r.checkBudget(); err != nil {
		t.Fatalf("без расхода: %v", err)
	}
	r.st.stage("analyze").Usage = protocol.Usage{CostUSD: 0.6}
	r.st.stage("execute").Usage = protocol.Usage{CostUSD: 0.5}
	if err := r.checkBudget(); !errors.Is(err, errBudget) {
		t.Fatalf("на лимите: %v", err)
	}
	r.st.BudgetAck = true
	if err := r.checkBudget(); err != nil {
		t.Fatalf("после ack: %v", err)
	}
	plan.BudgetUSD = 0
	r.st.BudgetAck = false
	if err := r.checkBudget(); err != nil {
		t.Fatalf("без лимита: %v", err)
	}
}

// Вердикт no-code пропускает execute/review последнего раунда; code возвращает.
func TestApplyPlanResult(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.addRound(r.stageKeys())
	r.applyPlanResult("no-code")
	if r.st.stage("execute").Status != "skipped" || r.st.stage("review").Status != "skipped" {
		t.Fatalf("no-code не пропустил этапы: %+v", r.st.Stages)
	}
	r.applyPlanResult("code")
	if r.st.stage("execute").Status != "pending" || r.st.stage("review").Status != "pending" {
		t.Fatalf("code не вернул этапы: %+v", r.st.Stages)
	}
	// Второй раунд: пропуск касается только последнего раунда.
	r.st.addRound([]string{"analyze", "execute", "review"})
	r.applyPlanResult("no-code")
	first := r.st.Stages[0:7]
	for _, st := range first {
		if st.Key == "execute" && st.Status == "skipped" {
			t.Fatal("пропуск задел прошлый раунд")
		}
	}
	if r.st.stage("execute").Round != 2 || r.st.stage("execute").Status != "skipped" {
		t.Fatalf("последний раунд не пропущен: %+v", r.st.stage("execute"))
	}
}

// База раунда: для старых задач — база задачи.
func TestRoundBase(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.BaseCommit = "base"
	if got := r.roundBase(); got != "base" {
		t.Errorf("без round_base: %q", got)
	}
	r.st.RoundBase = "round2"
	if got := r.roundBase(); got != "round2" {
		t.Errorf("с round_base: %q", got)
	}
}

// Раунды накапливаются: прошлый прогон остаётся историей, цикл работает с
// последним раундом.
func TestStageRounds(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.addRound(r.stageKeys())
	for i := range r.st.Stages {
		r.st.Stages[i].Status = "done"
	}
	round := r.st.addRound([]string{"analyze", "execute"})
	if round != 2 {
		t.Fatalf("раунд %d", round)
	}
	if st := r.st.stage("analyze"); st.Round != 2 || st.Status != "pending" {
		t.Errorf("текущий этап не из последнего раунда: %+v", st)
	}
	if st := r.st.Stages[1]; st.Key != "analyze" || st.Round != 1 || st.Status != "done" {
		t.Errorf("история первого раунда испорчена: %+v", st)
	}
	if r.st.round() != 2 {
		t.Errorf("номер раунда %d", r.st.round())
	}
}

// Референс проходит только доверенной формы: ведущий дефис превратил бы имя
// ветки в опцию git, слэши и точки — в выход из пути.
func TestRefValidation(t *testing.T) {
	ok := []string{"tn/core/tradernet#42546", "task-42", "a#1", "group/sub.project#7"}
	bad := []string{"", "-rf#1", " task-1", "task-x", "../../evil#1", "a b#1", "#1", "task-1;rm"}
	for _, s := range ok {
		if !refRe.MatchString(s) {
			t.Errorf("%q должен проходить", s)
		}
	}
	for _, s := range bad {
		if refRe.MatchString(s) {
			t.Errorf("%q не должен проходить", s)
		}
	}
}

// Этапы с недоверенным содержимым получают список без Bash в per_stage; в
// non_stop — без ограничений.
func TestAllowedToolsScoping(t *testing.T) {
	for _, key := range []string{"analyze", "err_work"} {
		tools := toolsFor("per_stage", key)
		if len(tools) == 0 {
			t.Errorf("%s: ожидался ограниченный список", key)
		}
		for _, tool := range tools {
			if tool == "Bash" {
				t.Errorf("%s: Bash в списке", key)
			}
		}
		if toolsFor("non_stop", key) != nil {
			t.Errorf("%s в non_stop: ожидалось без ограничений", key)
		}
	}
	for _, key := range []string{"import", "execute", "review"} {
		if toolsFor("per_stage", key) != nil {
			t.Errorf("%s: ожидалось без ограничений", key)
		}
	}
	if got := withTools([]string{"Read"}, []string{"mcp"}); len(got) != 2 {
		t.Errorf("withTools = %v", got)
	}
	if got := withTools(nil, []string{"mcp"}); got != nil {
		t.Errorf("к «без ограничений» добавили: %v", got)
	}
}

// Тест-гейт: зелёная команда проходит, красная возвращает ошибку с выводом.
func TestRunTestGate(t *testing.T) {
	ctx := context.Background()
	if out, err := runTestGate(ctx, "echo ok", t.TempDir()); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("зелёная: %v %q", err, out)
	}
	if _, err := runTestGate(ctx, "echo boom; exit 1", t.TempDir()); err == nil {
		t.Fatal("красная команда прошла")
	}
}

// Вопросы: номер выдаётся исполнителем, ответ сопоставляется по нему, а
// собранный промпт помечает вопросы доставленными.
func TestQuestionsRoundTrip(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.addRound(r.stageKeys())
	st := r.st.stage("analyze")
	q := r.st.addQuestion(QuestionState{Stage: "analyze", Type: "text", Question: "Какой фреймворк?"})
	if q.ID != 1 || q.Status != "open" {
		t.Fatalf("вопрос: %+v", q)
	}
	r.job.deliverAnswer(&protocol.Answer{QuestionID: 1, Text: "Pygame"})
	got, err := r.waitAndCollectAnswers(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Pygame") || !strings.Contains(got, "Какой фреймворк") {
		t.Errorf("промпт из ответов: %q", got)
	}
	if r.st.question(1).Status != "delivered" {
		t.Error("ответ не помечен доставленным")
	}
	if again, _ := r.waitAndCollectAnswers(context.Background(), st); again != "" {
		t.Errorf("доставленный ответ ушёл второй раз: %q", again)
	}
	// Статусы ожидания и возврата ушли событиями.
	var seen []string
	for _, ev := range pending(r.job) {
		if ev.Type == "task_status" {
			seen = append(seen, ev.Payload["status"].(string))
		}
	}
	if strings.Join(seen, ",") != "waiting_user,running" {
		t.Errorf("статусы таски: %v", seen)
	}
}

// Артефакты уходят событиями при завершении этапа: имя и содержимое, только
// .md и только из папки задачи.
func TestArtifactsEmitted(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.addRound(r.stageKeys())
	_ = os.WriteFile(filepath.Join(r.st.TaskDir, "task.md"), []byte("# сводка"), 0o644)
	_ = os.WriteFile(filepath.Join(r.st.TaskDir, "secret.txt"), []byte("nope"), 0o644)
	_ = r.finishStage(r.st.stage("analyze"))
	names := map[string]string{}
	for _, ev := range pending(r.job) {
		if ev.Type == protocol.EventArtifact {
			names[ev.Payload["name"].(string)] = ev.Payload["content"].(string)
		}
	}
	if names["task.md"] != "# сводка" {
		t.Errorf("артефакт не ушёл: %v", names)
	}
	if _, ok := names["secret.txt"]; ok {
		t.Error("не-md файл ушёл артефактом")
	}
}

// Состояние переживает запись в журнал и чтение обратно: сессии, прогоны,
// вопросы — всё, без чего возобновление начинало бы этап заново.
func TestStateSurvivesJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := &TaskState{TaskDir: "/t", WorktreeDir: "/w", BranchName: "task/7-wip", BaseCommit: "abc"}
	st.addRound([]string{"analyze", "execute"})
	st.stage("analyze").SessionID, st.stage("analyze").CurrentPass, st.stage("analyze").Status = "sess-1", 1, "paused"
	st.addQuestion(QuestionState{Stage: "analyze", Question: "q?"})
	if err := j.Put(&Record{JobID: "j1", TaskID: 7, Plan: fullPlan(), State: st}); err != nil {
		t.Fatal(err)
	}
	recs, errs := j.List()
	if len(errs) != 0 || len(recs) != 1 || recs[0].State == nil {
		t.Fatalf("журнал: %v %+v", errs, recs)
	}
	back := recs[0].State
	if back.stage("analyze").SessionID != "sess-1" || back.stage("analyze").Status != "paused" {
		t.Errorf("сессия этапа потеряна: %+v", back.stage("analyze"))
	}
	if len(back.Questions) != 1 || back.Questions[0].Status != "open" || back.NextQuestion != 1 {
		t.Errorf("вопросы потеряны: %+v", back.Questions)
	}
}

// Новый раунд: артефакты прошлого уходят в round<N>/, а грязная рабочая
// копия — отказ до первого этапа, с именами файлов.
func TestChangeRoundArchivesAndRefusesDirty(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	r.st.addRound(r.stageKeys())
	for i := range r.st.Stages {
		r.st.Stages[i].Status = "done"
	}
	for _, name := range []string{"step02-analyze.md", "step03-refined-plan.md", "task.md"} {
		if err := os.WriteFile(filepath.Join(r.st.TaskDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo := t.TempDir()
	if err := gitops.InitRepo(repo, "main"); err != nil {
		t.Fatal(err)
	}
	r.st.WorktreeDir = repo
	if err := os.WriteFile(filepath.Join(repo, "snake"), []byte("bin"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.startChangeRound()
	if err == nil || !strings.Contains(err.Error(), "snake") {
		t.Fatalf("грязная копия не остановила раунд: %v", err)
	}
	if r.st.round() != 1 {
		t.Fatalf("раунд заведён несмотря на отказ: %d", r.st.round())
	}
	if _, serr := os.Stat(filepath.Join(r.st.TaskDir, "step02-analyze.md")); serr != nil {
		t.Fatal("артефакты перенесены до отказа")
	}
	_ = os.Remove(filepath.Join(repo, "snake"))
	if err := r.startChangeRound(); err != nil {
		t.Fatal(err)
	}
	if r.st.round() != 2 {
		t.Fatalf("раунд %d", r.st.round())
	}
	if _, serr := os.Stat(filepath.Join(r.st.TaskDir, "round1", "step03-refined-plan.md")); serr != nil {
		t.Fatal("план прошлого раунда не в round1/")
	}
	if _, serr := os.Stat(filepath.Join(r.st.TaskDir, "step03-refined-plan.md")); serr == nil {
		t.Fatal("план прошлого раунда остался наверху — execute взял бы его вместо нового")
	}
	if _, serr := os.Stat(filepath.Join(r.st.TaskDir, "task.md")); serr != nil {
		t.Fatal("task.md не должен переезжать")
	}
	if roundDir(r.st.TaskDir, 1) == "" || roundDir(r.st.TaskDir, 2) != "" {
		t.Fatal("roundDir")
	}
}
