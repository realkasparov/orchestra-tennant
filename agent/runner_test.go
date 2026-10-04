package agent

import "testing"

// Итог result-события несёт usage всего прогона — из него считается расход
// этапа; при отсутствии полей остаются нули.
func TestResultUsage(t *testing.T) {
	res := &Result{}
	handleLine(map[string]any{
		"type": "result", "session_id": "s1", "num_turns": float64(7),
		"total_cost_usd": 1.25,
		"usage": map[string]any{
			"input_tokens": float64(22), "output_tokens": float64(5198),
			"cache_creation_input_tokens": float64(18951),
			"cache_read_input_tokens":     float64(194459),
		},
	}, res, nil, func(StreamEvent) {}, nil)
	if !res.GotResult {
		t.Fatal("result event not seen")
	}
	u := res.Usage
	if u.InputTokens != 22 || u.OutputTokens != 5198 || u.CacheWrite != 18951 || u.CacheRead != 194459 {
		t.Fatalf("usage = %+v", u)
	}
	if u.CostUSD != 1.25 {
		t.Fatalf("cost = %v", u.CostUSD)
	}

	// Без usage (обрыв, старый CLI) — нули, а не паника.
	res2 := &Result{}
	handleLine(map[string]any{"type": "result"}, res2, nil, func(StreamEvent) {}, nil)
	if res2.Usage != (Usage{}) {
		t.Fatalf("expected zero usage, got %+v", res2.Usage)
	}
}

// Токены — из modelUsage: это суммы всей сессии с субагентами и всеми
// ходами, а usage — только последний ход основного агента. Последний итог
// процесса главнее предыдущих.
func TestResultUsageFromModelUsage(t *testing.T) {
	res := &Result{}
	turn := func(cost float64, cacheRead float64) map[string]any {
		return map[string]any{
			"type": "result", "session_id": "s1", "total_cost_usd": cost,
			"usage": map[string]any{"input_tokens": float64(1), "output_tokens": float64(2),
				"cache_creation_input_tokens": float64(3), "cache_read_input_tokens": float64(4)},
			"modelUsage": map[string]any{
				"claude-opus-5":             map[string]any{"inputTokens": float64(10), "outputTokens": float64(20), "cacheCreationInputTokens": float64(30), "cacheReadInputTokens": cacheRead, "contextWindow": float64(200000)},
				"claude-haiku-4-5-20251001": map[string]any{"inputTokens": float64(1), "outputTokens": float64(2), "cacheCreationInputTokens": float64(3), "cacheReadInputTokens": float64(4)},
			},
		}
	}
	handleLine(turn(0.5, 100), res, nil, func(StreamEvent) {}, nil)
	handleLine(turn(0.75, 300), res, nil, func(StreamEvent) {}, nil)
	u := res.Usage
	if u.InputTokens != 11 || u.OutputTokens != 22 || u.CacheWrite != 33 || u.CacheRead != 304 || u.CostUSD != 0.75 {
		t.Fatalf("usage = %+v", u)
	}
}

// Маппинг ключей моделей: opus — это Opus 5, неизвестный ключ — Fable.
func TestModelID(t *testing.T) {
	for key, want := range map[string]string{
		"opus":   "claude-opus-5",
		"sonnet": "claude-sonnet-5",
		"haiku":  "claude-haiku-4-5-20251001",
		"fable":  "claude-fable-5",
		"":       "claude-fable-5",
	} {
		if got := ModelID(key); got != want {
			t.Errorf("ModelID(%q) = %q, want %q", key, got, want)
		}
	}
}

// Размер разговора — по последнему обращению к модели (одно обращение —
// несколько сообщений с одним id), окно — из итога, сжатие — из
// compact_boundary.
func TestSessionLine(t *testing.T) {
	res := &Result{}
	var events []StreamEvent
	on := func(e StreamEvent) { events = append(events, e) }
	call := func(id string, in, write, read float64) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"id": id,
			"usage": map[string]any{"input_tokens": in, "cache_creation_input_tokens": write, "cache_read_input_tokens": read}}}
	}
	// Сессия известна с первой строки — до первого обращения к модели.
	sessionLine(map[string]any{"type": "system", "subtype": "init", "session_id": "s1"}, res, on)
	if len(events) != 1 || events[0].Type != "session" || events[0].Payload["id"] != "s1" {
		t.Fatalf("начало сессии: %+v", events)
	}
	events = nil
	sessionLine(call("m1", 10, 9664, 22376), res, on)
	sessionLine(call("m1", 10, 9664, 22376), res, on)
	sessionLine(call("m2", 8, 2559, 32040), res, on)
	if res.Context != 34607 || len(events) != 2 {
		t.Fatalf("контекст %d, событий %d", res.Context, len(events))
	}
	// Разговор субагента — не размер сессии шага.
	sub := call("m3", 10, 150000, 10000)
	sub["parent_tool_use_id"] = "toolu_1"
	sessionLine(sub, res, on)
	if res.Context != 34607 || len(events) != 2 {
		t.Fatalf("субагент: контекст %d, событий %d", res.Context, len(events))
	}
	sessionLine(map[string]any{"type": "system", "subtype": "compact_boundary",
		"compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 31964.0, "post_tokens": 3603.0}}, res, on)
	if res.Compacted == nil || res.Compacted.Pre != 31964 || res.Context != 3603 {
		t.Fatalf("сжатие: %+v, контекст %d", res.Compacted, res.Context)
	}
	sessionLine(map[string]any{"type": "result", "modelUsage": map[string]any{
		"claude-opus-5":             map[string]any{"contextWindow": 1000000.0},
		"claude-haiku-4-5-20251001": map[string]any{"contextWindow": 200000.0},
	}}, res, on)
	if res.Window("claude-haiku-4-5-20251001") != 200000 || res.Window("claude-sonnet-5") != 1000000 {
		t.Fatalf("окна: %+v", res.Windows)
	}
}
