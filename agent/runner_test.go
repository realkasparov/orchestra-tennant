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
	}, res, nil, func(StreamEvent) {})
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
	handleLine(map[string]any{"type": "result"}, res2, nil, func(StreamEvent) {})
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
		"fable":  "claude-fable-5-1",
		"":       "claude-fable-5-1",
	} {
		if got := ModelID(key); got != want {
			t.Errorf("ModelID(%q) = %q, want %q", key, got, want)
		}
	}
}
