package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// fakeClaude кладёт в PATH скрипт `claude`: на «/compact» отвечает границей
// сжатия, на остальное — одним обращением к модели с расходом и итогом с
// окном модели. Вызовы пишутся в calls.
func fakeClaude(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> "` + calls + `"; echo >> "` + calls + `"
case "$*" in
*/compact*)
  echo '{"type":"system","subtype":"init","session_id":"s1"}'
  echo '{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"manual","pre_tokens":160000,"post_tokens":12000}}'
  echo '{"type":"result","subtype":"success","session_id":"s1","usage":{}}'
  ;;
*)
  echo '{"type":"system","subtype":"init","session_id":"s1"}'
  echo '{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"готово"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":19000}}}'
  echo '{"type":"result","subtype":"success","session_id":"s1","result":"готово","usage":{"input_tokens":10},"modelUsage":{"claude-sonnet-5":{"contextWindow":200000}}}'
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// Продолжение сессии, заполненной на 80%, начинается со сжатия; после
// прогона — заполненность по последнему обращению и окно из итога.
func TestResumeCompactsFullSession(t *testing.T) {
	calls := fakeClaude(t)
	plan := fullPlan()
	plan.StageTimeout = protocol.Seconds(60)
	r, _ := testRun(t, plan)
	st := &StageState{Key: "execute", Round: 1, SessionID: "s1", Context: 160000, Window: 200000}
	r.st.Stages = append(r.st.Stages, st)
	sp := agentSpec{key: "execute", skill: "execute-plan", model: "sonnet", cwd: t.TempDir(), resume: true}
	if _, err := r.runAgentSession(context.Background(), st, sp, "почини тесты", "s1", 1); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "/compact") || !strings.Contains(lines[1], "Сессия сжата") {
		t.Fatalf("вызовы claude: %q", lines)
	}
	if st.Resumes != 1 || st.Context != 20010 || st.Window != 200000 || st.CompactPending != "" {
		t.Fatalf("сессия шага: %+v", st)
	}
	var compacted, usage bool
	for _, e := range pending(r.job) {
		switch e.Type {
		case protocol.EventSessionCompacted:
			compacted = e.Payload["trigger"] == "auto"
		case protocol.EventSessionUsage:
			usage = true
		}
	}
	if !compacted || !usage {
		t.Fatalf("события: сжатие %v, заполненность %v", compacted, usage)
	}
}

// Сжатие стоящего шага по кнопке: сразу, ответ — «готово» с размерами;
// размер после сжатия цикл этапов переносит в шаг, когда берёт его.
func TestManualCompactIdleStage(t *testing.T) {
	fakeClaude(t)
	r, _ := testRun(t, fullPlan())
	st := &StageState{Key: "analyze", Round: 1, SessionID: "s1", SessionCWD: t.TempDir(), SessionModel: "claude-sonnet-5", Context: 130000, Window: 200000}
	r.st.Stages = append(r.st.Stages, st)
	r.seedSessions()
	r.compactRequest(context.Background(), &protocol.Compact{ReqID: "q1", Key: "analyze", Round: 1})
	r.closeSessions()
	if st.Context != 130000 {
		t.Fatalf("горутина сообщений тронула шаг: %d", st.Context)
	}
	if trig := r.takeSession(st); trig != "" || st.Context != 12000 {
		t.Fatalf("после сжатия: просьба %q, контекст %d", trig, st.Context)
	}
	var compacted bool
	for _, e := range pending(r.job) {
		compacted = compacted || (e.Type == protocol.EventSessionCompacted && e.Payload["round"] == 1)
	}
	if !compacted {
		t.Fatal("нет события о сжатии")
	}
}

// Шаг занят (ждёт ответов): просьба откладывается до следующего обращения к
// агенту; у шага без сессии — ошибка, а не сжатие чужого раунда.
func TestManualCompactBusyStage(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	st := &StageState{Key: "analyze", Round: 2, SessionID: "s2"}
	r.st.Stages = append(r.st.Stages, st)
	r.seedSessions()
	lock := r.stageLock("analyze")
	lock.Lock()
	r.compactRequest(context.Background(), &protocol.Compact{ReqID: "q1", Key: "analyze", Round: 2})
	lock.Unlock()
	if trig := r.takeSession(st); trig != "manual" {
		t.Fatalf("отложенная просьба: %q", trig)
	}
	if trig := r.takeSession(st); trig != "" {
		t.Fatalf("просьба не снята: %q", trig)
	}
	if _, _, ok := r.sessionSnapshot("analyze", 1); ok {
		t.Fatal("у раунда 1 сессии нет")
	}
	if _, round, ok := r.sessionSnapshot("analyze", 0); !ok || round != 2 {
		t.Fatalf("последний раунд с сессией: %d %v", round, ok)
	}
}

// Окно модели ещё неизвестно (исполнитель только запущен): процент
// показывается по окну по умолчанию, но автосжатия нет.
func TestNoAutoCompactWithoutKnownWindow(t *testing.T) {
	calls := fakeClaude(t)
	plan := fullPlan()
	plan.StageTimeout = protocol.Seconds(60)
	r, _ := testRun(t, plan)
	st := &StageState{Key: "execute", Round: 1, SessionID: "s1", Context: 180000}
	r.st.Stages = append(r.st.Stages, st)
	sp := agentSpec{key: "execute", skill: "execute-plan", model: "opus", cwd: t.TempDir(), resume: true}
	if _, err := r.runAgentSession(context.Background(), st, sp, "почини тесты", "s1", 1); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(calls)
	if strings.Contains(string(raw), "/compact") {
		t.Fatalf("сжато по окну по умолчанию: %s", raw)
	}
}
