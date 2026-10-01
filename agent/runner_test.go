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
	sessionLine(call("m1", 10, 9664, 22376), res, on)
	sessionLine(call("m1", 10, 9664, 22376), res, on)
	sessionLine(call("m2", 8, 2559, 32040), res, on)
	if res.Context != 34607 || len(events) != 2 {
		t.Fatalf("контекст %d, событий %d", res.Context, len(events))
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
