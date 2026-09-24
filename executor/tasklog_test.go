package executor

import (
	"strings"
	"testing"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// События таски читаются словами: этап, статус, агент, инструмент; шум
// (результаты инструментов, расход) в журнал не идёт.
func TestDescribeEvent(t *testing.T) {
	cases := []struct {
		ev   protocol.Event
		want string
	}{
		{protocol.Event{Type: "task_status", Payload: map[string]any{"status": "running"}}, "Таска: выполняется"},
		{protocol.Event{Type: "stage_status", Payload: map[string]any{"key": "analyze", "round": 2, "status": "done"}}, "Этап «Анализ задачи» (раунд 2) — готово"},
		{protocol.Event{Type: "stage_status", Payload: map[string]any{"key": "analyze", "status": "running", "usage": 1}}, ""},
		{protocol.Event{Type: "agent_text", Stage: "execute", Payload: map[string]any{"text": "Сделал коммит.\nПодробности…"}}, "Выполнение: Агент: Сделал коммит. …"},
		{protocol.Event{Type: "tool_use", Stage: "analyze", Payload: map[string]any{"name": "Read", "summary": "web/src/game.ts"}}, "Анализ задачи: Инструмент Read: web/src/game.ts"},
		{protocol.Event{Type: "tool_result", Payload: map[string]any{"summary": "ok"}}, ""},
		{protocol.Event{Type: "log", Payload: map[string]any{"text": "Ветка: task-3-x"}}, "Ветка: task-3-x"},
	}
	for _, c := range cases {
		if got := describeEvent(nil, &c.ev); got != c.want {
			t.Errorf("%s: %q, ожидалось %q", c.ev.Type, got, c.want)
		}
	}
	if p := TaskLogPrefix("snake", 3); !strings.HasPrefix(p, "Project snake · Task 3 · ") {
		t.Errorf("префикс: %q", p)
	}
}
