package executor

import (
	"testing"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

func result(session string, cost float64, cacheRead int64) *agent.Result {
	return &agent.Result{SessionID: session, GotResult: true,
		Usage: agent.Usage{InputTokens: 10, OutputTokens: 5, CacheWrite: 100, CacheRead: cacheRead, CostUSD: cost}}
}

// Продолжение сессии (ответы на вопросы, уточнение, пауза) приносит суммы
// всей сессии: в этап идёт только разница с прошлым вызовом.
func TestRunUsageResumeDelta(t *testing.T) {
	st := &StageState{Key: "solve"}
	if u := runUsage(st, result("s1", 1.0, 1000), ""); u.CostUSD != 1.0 || u.CacheRead != 1000 {
		t.Fatalf("первый вызов: %+v", u)
	}
	// Продолжение s1: суммы выросли до 1.5 и 1600.
	u := runUsage(st, &agent.Result{SessionID: "s1", GotResult: true,
		Usage: agent.Usage{InputTokens: 12, OutputTokens: 9, CacheWrite: 150, CacheRead: 1600, CostUSD: 1.5}}, "s1")
	want := protocol.Usage{TokIn: 2, TokOut: 4, CacheWrite: 50, CacheRead: 600, CostUSD: 0.5}
	if u != want {
		t.Fatalf("продолжение: %+v, ждали %+v", u, want)
	}
	if st.SessionTotal.CostUSD != 1.5 || st.SessionTotalID != "s1" {
		t.Fatalf("база: %+v %s", st.SessionTotal, st.SessionTotalID)
	}
	// Оборванный вызов без итога базу не трогает.
	if u := runUsage(st, &agent.Result{SessionID: "s1"}, "s1"); !u.Zero() {
		t.Fatalf("оборванный: %+v", u)
	}
	if st.SessionTotal.CostUSD != 1.5 {
		t.Fatalf("база после обрыва: %+v", st.SessionTotal)
	}
	// Новая сессия того же шага (ошибка → прогон заново) — итог как есть.
	if u := runUsage(st, result("s2", 0.25, 300), ""); u.CostUSD != 0.25 || u.CacheRead != 300 {
		t.Fatalf("новая сессия: %+v", u)
	}
}

// CLI, у которого суммы при продолжении начинаются заново, узнаётся по
// уменьшению: итог берётся как есть, а не обнуляется.
func TestRunUsageResumeWithoutCarry(t *testing.T) {
	st := &StageState{Key: "execute"}
	runUsage(st, result("s1", 1.0, 1000), "")
	u := runUsage(st, result("s1", 0.2, 200), "s1")
	if u.CostUSD != 0.2 || u.CacheRead != 200 {
		t.Fatalf("без переноса сумм: %+v", u)
	}
}

// Продолжение чужой сессии (база запомнена для другой) — итог как есть.
func TestRunUsageOtherSession(t *testing.T) {
	st := &StageState{Key: "execute", SessionTotal: protocol.Usage{CostUSD: 5}, SessionTotalID: "old"}
	if u := runUsage(st, result("s9", 1.0, 100), "s9"); u.CostUSD != 1.0 {
		t.Fatalf("%+v", u)
	}
}
