package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// e2eClaude — поддельный claude для сквозных прогонов Run: триаж по маркеру
// QQQ в тексте (вопрос) или правка; ответ на вопрос ждёт файла release.
func e2eClaude(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
trap 'exit 130' INT TERM
D="` + dir + `"
emit() {
  echo '{"type":"system","subtype":"init","session_id":"s1"}'
  printf '{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"%s"}],"usage":{"input_tokens":1}}}\n' "$1"
  printf '{"type":"result","subtype":"success","session_id":"s1","result":"%s","usage":{"input_tokens":1}}\n' "$1"
}
case "$*" in
*Классифицируй*)
  echo "triage" >> "$D/calls"
  case "$*" in *QQQ*) emit "TRIAGE: question";; *) emit "TRIAGE: change";; esac
  ;;
*"Пользователь задал вопрос"*)
  q=$(printf '%s' "$*" | grep -o 'QQQ[0-9]*' | head -1)
  echo "answer-start $q" >> "$D/calls"
  while [ ! -f "$D/release" ]; do sleep 0.05; done
  echo "answer-done $q" >> "$D/calls"
  emit "ответ"
  ;;
*)
  echo "other" >> "$D/calls"
  emit "готово"
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func readCalls(dir string) string { b, _ := os.ReadFile(filepath.Join(dir, "calls")); return string(b) }

func newJob(ex *Executor, id string, plan *protocol.Plan, st *TaskState) *Job {
	return &Job{ID: id, Plan: plan, ex: ex, State: st,
		answers: make(chan *protocol.Answer, 8), messages: make(chan *protocol.Message, 8),
		compacts: make(chan *protocol.Compact, 4), cont: make(chan protocol.Continue, 1), ended: make(chan struct{})}
}

func doneState(t *testing.T, plan *protocol.Plan, wt string) *TaskState {
	st := &TaskState{TaskDir: t.TempDir(), WorktreeDir: wt, BranchName: "task-7-x"}
	r, _ := testRun(t, plan)
	r.st = st
	st.addRound(r.stageKeys())
	for _, s := range st.Stages {
		s.Status = "done"
	}
	return st
}

// Готовая таска: вопрос в начале задания, пока на него отвечают — живая
// правка и живой вопрос, затем пауза. Отложенное — по одному со своими
// режимами; возобновление отвечает на оба вопроса по разу, а в правке и в
// user-feedback.md — только текст правки.
func TestE2EStartQuestionLiveChangeQuestionPause(t *testing.T) {
	cd := e2eClaude(t)
	ex, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	plan.StageTimeout = protocol.Seconds(60)
	plan.Message = &protocol.Message{Text: "QQQ1 как запустить?", Mode: "auto"}
	st := doneState(t, plan, wt)
	p := &Pipeline{DataDir: t.TempDir()}
	j := newJob(ex, "j1", plan, st)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 1)
	go func() { s, _ := p.Run(ctx, j); out <- s }()
	waitFor(t, "ответ на QQQ1", func() bool { return strings.Contains(readCalls(cd), "answer-start QQQ1") })
	j.deliverMessage(&protocol.Message{Text: "ZZZ поправь кнопку", Mode: "auto"})
	waitFor(t, "правка", func() bool {
		b, _ := os.ReadFile(filepath.Join(st.TaskDir, "user-feedback.md"))
		return strings.Contains(string(b), "ZZZ")
	})
	j.deliverMessage(&protocol.Message{Text: "QQQ2 а тесты?", Mode: "auto"})
	waitFor(t, "триаж вопроса", func() bool { return strings.Count(readCalls(cd), "triage") >= 3 })
	time.Sleep(300 * time.Millisecond)
	cancel()
	j.finish(<-out, "")
	if got := queueOf(st); got != "change:ZZZ поправь кнопку | auto:QQQ1 как запустить? | question:QQQ2 а тесты?" {
		t.Fatalf("отложенное после паузы: %s", got)
	}
	b, _ := json.Marshal(st) // через журнал
	var st2 TaskState
	if err := json.Unmarshal(b, &st2); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cd, "release"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(cd, "calls"), nil, 0o644)
	// Вопросы и правка разбираются в начале задания; дальше раунд правки
	// ждёт связи с оркестратором, которой в тесте нет, — до таймаута.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, _ = p.Run(ctx2, newJob(ex, "j1", plan, &st2))
	calls := readCalls(cd)
	if strings.Count(calls, "answer-done QQQ1") != 1 || strings.Count(calls, "answer-done QQQ2") != 1 {
		t.Fatalf("ответы на вопросы после возобновления:\n%s", calls)
	}
	if st2.Feedback != "ZZZ поправь кнопку" {
		t.Fatalf("правка: %q", st2.Feedback)
	}
	fb, _ := os.ReadFile(filepath.Join(st.TaskDir, "user-feedback.md"))
	if strings.Count(string(fb), "## Правка") != 1 || strings.Contains(string(fb), "QQQ") {
		t.Fatalf("user-feedback.md: %q", fb)
	}
}

// Исполнитель перезапустился, пока отвечал на вопрос начала задания: то же
// задание из журнала отвечает на него один раз, а не берёт ещё и из плана.
func TestE2ECrashDuringStart(t *testing.T) {
	cd := e2eClaude(t)
	ex, repo, wt := viewWorld(t)
	plan := fullPlan()
	plan.Project = protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"}
	plan.StageTimeout = protocol.Seconds(60)
	plan.Message = &protocol.Message{Text: "QQQ1 как запустить?", Mode: "auto"}
	st := doneState(t, plan, wt)
	st.MessageJob = "j0"
	p := &Pipeline{DataDir: t.TempDir()}
	j := newJob(ex, "j1", plan, st)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 1)
	go func() { s, _ := p.Run(ctx, j); out <- s }()
	waitFor(t, "ответ на QQQ1", func() bool { return strings.Contains(readCalls(cd), "answer-start QQQ1") })
	recs, _ := ex.journal.List() // что осталось бы после смерти процесса
	var saved *TaskState
	for _, r := range recs {
		if r.JobID == "j1" {
			saved = r.State
		}
	}
	cancel()
	<-out
	if saved == nil {
		t.Fatal("нет записи журнала")
	}
	_ = os.WriteFile(filepath.Join(cd, "release"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(cd, "calls"), nil, 0o644)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_, _ = p.Run(ctx2, newJob(ex, "j1", plan, saved))
	if calls := readCalls(cd); strings.Count(calls, "answer-start QQQ1") != 1 {
		t.Fatalf("вопрос начала задания после перезапуска:\n%s", calls)
	}
}
