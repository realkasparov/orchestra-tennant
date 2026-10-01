package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Заполненность и сжатие сессии шага (session-context): размер разговора
// после каждого обращения к модели, сжатие по кнопке человека и с порога.

// sessionState — то, что прогон помнит о сессиях шагов между вызовами.
type sessionState struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	lastPct map[string]int
}

// stageLock — замок сессии шага: прогон держит его, пока идёт агент;
// сжатие стоящего шага берёт его же.
func (r *run) stageLock(key string) *sync.Mutex {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if r.sess.locks == nil {
		r.sess.locks = map[string]*sync.Mutex{}
	}
	l := r.sess.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		r.sess.locks[key] = l
	}
	return l
}

func (r *run) windowFor(model string) int64 {
	if r.job == nil || r.job.ex == nil {
		return defaultWindow
	}
	return r.job.ex.windowFor(model)
}

func (r *run) noteWindow(model string, w int64) {
	if r.job != nil && r.job.ex != nil {
		r.job.ex.noteWindow(model, w)
	}
}

// emitSession сообщает оркестратору заполненность сессии шага. Без force —
// только когда процент сменился: событие идёт после каждого обращения.
func (r *run) emitSession(st *StageState, force bool) {
	if st.SessionID == "" && st.Context == 0 {
		return
	}
	pct := protocol.ContextPercent(st.Context, st.Window)
	r.sess.mu.Lock()
	if r.sess.lastPct == nil {
		r.sess.lastPct = map[string]int{}
	}
	k := fmt.Sprintf("%s#%d", st.Key, st.Round)
	last, seen := r.sess.lastPct[k]
	r.sess.lastPct[k] = pct
	r.sess.mu.Unlock()
	if !force && seen && last == pct {
		return
	}
	r.job.Emit(st.Key, protocol.EventSessionUsage, map[string]any{
		"key": st.Key, "round": st.Round, "session": st.SessionID, "cwd": st.SessionCWD, "model": st.SessionModel,
		"tokens": st.Context, "window": st.Window, "percent": pct, "resumes": st.Resumes,
	})
}

// sessionCompacted — сессия шага сжата: событие и запись в журнал шага.
func (r *run) sessionCompacted(st *StageState, pre, post int64, trigger string) {
	if post > 0 {
		st.Context = post
	}
	r.job.Emit(st.Key, protocol.EventSessionCompacted, map[string]any{
		"key": st.Key, "round": st.Round, "session": st.SessionID, "pre": pre, "post": post,
		"window": st.Window, "trigger": trigger,
	})
	who := map[string]string{"manual": "по кнопке", "auto": "автоматически", "claude": "самим Claude Code"}[trigger]
	r.log(st.Key, fmt.Sprintf("Сессия сжата %s: контекст %d%% → %d%%.", who,
		protocol.ContextPercent(pre, st.Window), protocol.ContextPercent(post, st.Window)))
	r.emitSession(st, true)
}

// compactNow сжимает сессию шага. Сбой не роняет шаг: сессия продолжается
// как есть, причина — в журнале.
func (r *run) compactNow(ctx context.Context, st *StageState, session, trigger string) (*agent.Compaction, error) {
	st.CompactPending = ""
	if session == "" {
		session = st.SessionID
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	comp, err := agent.Compact(cctx, session, st.SessionCWD, st.SessionModel)
	if err != nil {
		r.log(st.Key, "Сжать сессию не вышло: "+err.Error()+" — продолжаю без сжатия.")
		return nil, err
	}
	r.sessionCompacted(st, comp.Pre, comp.Post, trigger)
	r.job.SaveState()
	return comp, nil
}

// compactRequest — «Сжать сессию» от человека для шага этого задания.
func (r *run) compactRequest(ctx context.Context, c *protocol.Compact) {
	reply := func(res *protocol.CompactResult) {
		res.ReqID, res.TaskID, res.Key, res.Round = c.ReqID, c.TaskID, c.Key, c.Round
		if r.job != nil && r.job.ex != nil {
			if err := r.job.ex.send(protocol.MsgCompactResult, r.job.ID, res); err != nil {
				r.job.ex.logf("ответ на сжатие сессии %s: %v", c.ReqID, err)
			}
		}
	}
	st := r.st.stageRound(c.Key, c.Round)
	if st == nil || st.SessionID == "" {
		reply(&protocol.CompactResult{Status: "error", Error: "у шага нет сессии"})
		return
	}
	switch r.clar.requestCompact(c.Key, "manual") {
	case "interrupt":
		r.log(c.Key, "Сжимаю сессию по кнопке — шаг продолжит работу в той же сессии.")
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	case "pending":
		st.CompactPending = "manual"
		r.log(c.Key, "Сожму сессию, когда шаг закончит прогон: прервать его нельзя.")
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	}
	lock := r.stageLock(c.Key)
	if !lock.TryLock() {
		st.CompactPending = "manual"
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	}
	go func() {
		defer lock.Unlock()
		comp, err := r.compactNow(ctx, st, st.SessionID, "manual")
		if err != nil {
			reply(&protocol.CompactResult{Status: "error", Error: err.Error()})
			return
		}
		reply(&protocol.CompactResult{Status: "done", Pre: comp.Pre, Post: comp.Post, Window: st.Window})
	}()
}
