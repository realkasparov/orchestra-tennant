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
//
// Состояние шагов (StageState) меняет только цикл этапов. Горутина сообщений
// видит сессии шагов через снимки в sessionState и оставляет там просьбы и
// итоги сжатия — цикл этапов забирает их, когда берёт шаг.

// sessionInfo — снимок сессии шага для горутины сообщений.
type sessionInfo struct {
	ID, CWD, Model string
	Context        int64
	Window         int64
}

// sessionState — то, что прогон помнит о сессиях шагов между вызовами.
type sessionState struct {
	mu      sync.Mutex
	locks   map[sref]*sync.Mutex
	lastPct map[sref]int
	// info — снимки сессий шагов раундов; pending — просьбы сжать, пока
	// шаг занят; compacted — размер после сжатия стоящего шага, который цикл
	// этапов ещё не перенёс в StageState; busy — идёт сжатие стоящего шага.
	info      map[sref]sessionInfo
	pending   map[sref]string
	compacted map[sref]int64
	busy      map[sref]bool
	// closed — прогон заканчивается: новых сжатий в фоне не начинать.
	closed bool
	wg     sync.WaitGroup
}

// sref — шаг раунда.
type sref struct {
	key   string
	round int
}

func sessKey(key string, round int) sref { return sref{key, round} }

// stageLock — замок сессии шага раунда: прогон держит его, пока идёт
// агент; сжатие стоящего шага берёт его же.
func (r *run) stageLock(key string, round int) *sync.Mutex {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if r.sess.locks == nil {
		r.sess.locks = map[sref]*sync.Mutex{}
	}
	k := sessKey(key, round)
	l := r.sess.locks[k]
	if l == nil {
		l = &sync.Mutex{}
		r.sess.locks[k] = l
	}
	return l
}

// knownWindow — окно модели, которое она уже сообщила на этой машине; 0 —
// ещё не сообщала.
func (r *run) knownWindow(model string) int64 {
	if r.job == nil || r.job.ex == nil {
		return 0
	}
	return r.job.ex.knownWindow(model)
}

func (r *run) noteWindow(model string, w int64) {
	if r.job != nil && r.job.ex != nil {
		r.job.ex.noteWindow(model, w)
	}
}

// shownWindow — окно для показа процента: известное или по умолчанию.
// Автосжатие по умолчанию не срабатывает — только по известному окну.
func shownWindow(st *StageState) int64 {
	if st.Window > 0 {
		return st.Window
	}
	return defaultWindow
}

// syncSession кладёт снимок сессии шага для горутины сообщений. Зовёт
// только цикл этапов.
func (r *run) syncSession(st *StageState) {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if r.sess.info == nil {
		r.sess.info = map[sref]sessionInfo{}
	}
	r.sess.info[sessKey(st.Key, st.Round)] = sessionInfo{
		ID: st.SessionID, CWD: st.SessionCWD, Model: st.SessionModel, Context: st.Context, Window: st.Window,
	}
}

// seedSessions — снимки сессий шагов из журнала (задание продолжено).
func (r *run) seedSessions() {
	for _, st := range r.st.Stages {
		if st.SessionID != "" {
			r.syncSession(st)
		}
	}
}

// sessionSnapshot — снимок сессии шага key раунда round; round 0 —
// последний раунд, где у шага есть сессия.
func (r *run) sessionSnapshot(key string, round int) (sessionInfo, int, bool) {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if round > 0 {
		in, ok := r.sess.info[sessKey(key, round)]
		return in, round, ok && in.ID != ""
	}
	best, bestRound := sessionInfo{}, 0
	for k, in := range r.sess.info {
		if k.key == key && in.ID != "" && k.round > bestRound {
			best, bestRound = in, k.round
		}
	}
	return best, bestRound, bestRound > 0
}

// setPending — сжать сессию шага, когда цикл этапов её возьмёт.
func (r *run) setPending(key string, round int, trigger string) {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if r.sess.pending == nil {
		r.sess.pending = map[sref]string{}
	}
	if r.sess.pending[sessKey(key, round)] == "" {
		r.sess.pending[sessKey(key, round)] = trigger
	}
}

// takeSession переносит в шаг то, что оставила горутина сообщений: итог
// сжатия стоящего шага и просьбу сжать. Возвращает, чем сжимать (manual |
// auto | пусто). Зовёт только цикл этапов.
func (r *run) takeSession(st *StageState) string {
	k := sessKey(st.Key, st.Round)
	r.sess.mu.Lock()
	post, ok := r.sess.compacted[k]
	delete(r.sess.compacted, k)
	manual := r.sess.pending[k]
	delete(r.sess.pending, k)
	r.sess.mu.Unlock()
	if ok {
		// Уже сжата по кнопке: прежняя просьба (и автосжатие по старому
		// размеру) ни к чему — порог решит по новому.
		st.Context = post
		st.CompactPending = ""
	}
	trigger := st.CompactPending
	if manual != "" {
		trigger = manual
	}
	st.CompactPending = ""
	return trigger
}

// keepPending переносит в шаг просьбу сжать, оставленную горутиной
// сообщений, — шаг кончает прогон, и просьба уйдёт в журнал. Зовёт только
// цикл этапов.
func (r *run) keepPending(st *StageState) {
	k := sessKey(st.Key, st.Round)
	r.sess.mu.Lock()
	t := r.sess.pending[k]
	delete(r.sess.pending, k)
	r.sess.mu.Unlock()
	if t != "" {
		st.CompactPending = t
	}
}

// dropPending — шаг начинает новую сессию: просьбы сжать прежнюю ни к чему.
func (r *run) dropPending(st *StageState) {
	st.CompactPending = ""
	r.sess.mu.Lock()
	delete(r.sess.pending, sessKey(st.Key, st.Round))
	r.sess.mu.Unlock()
}

// closeSessions — прогон заканчивается: дождаться сжатий в фоне и перенести
// в шаги то, что цикл этапов не успел забрать, — итоги сжатий и просьбы
// останутся в журнале до продолжения. Зовёт цикл этапов.
func (r *run) closeSessions() {
	r.sess.mu.Lock()
	r.sess.closed = true
	r.sess.mu.Unlock()
	r.sess.wg.Wait()
	r.sess.mu.Lock()
	compacted, pending := r.sess.compacted, r.sess.pending
	r.sess.compacted, r.sess.pending = nil, nil
	r.sess.mu.Unlock()
	if len(compacted) == 0 && len(pending) == 0 {
		return
	}
	for k, post := range compacted {
		if st := r.st.stageRound(k.key, k.round); st != nil {
			st.Context, st.CompactPending = post, ""
		}
	}
	for k, t := range pending {
		if st := r.st.stageRound(k.key, k.round); st != nil {
			st.CompactPending = t
		}
	}
	r.job.SaveState()
}

// emitSession сообщает оркестратору заполненность сессии шага. Без force —
// только когда процент сменился: событие идёт после каждого обращения.
// Зовёт только цикл этапов.
func (r *run) emitSession(st *StageState, force bool) {
	r.syncSession(st)
	if st.SessionID == "" && st.Context == 0 {
		return
	}
	win := shownWindow(st)
	pct := protocol.ContextPercent(st.Context, win)
	r.sess.mu.Lock()
	if r.sess.lastPct == nil {
		r.sess.lastPct = map[sref]int{}
	}
	k := sessKey(st.Key, st.Round)
	last, seen := r.sess.lastPct[k]
	r.sess.lastPct[k] = pct
	r.sess.mu.Unlock()
	if !force && seen && last == pct {
		return
	}
	r.job.Emit(st.Key, protocol.EventSessionUsage, map[string]any{
		"key": st.Key, "round": st.Round, "session": st.SessionID, "cwd": st.SessionCWD, "model": st.SessionModel,
		"tokens": st.Context, "window": win, "percent": pct, "resumes": st.Resumes,
	})
}

// compactedEvent — событие и запись в журнал шага о сжатии сессии.
func (r *run) compactedEvent(key string, round int, session string, pre, post, window int64, trigger string) {
	r.job.Emit(key, protocol.EventSessionCompacted, map[string]any{
		"key": key, "round": round, "session": session, "pre": pre, "post": post,
		"window": window, "trigger": trigger,
	})
	who := map[string]string{"manual": "по кнопке", "auto": "автоматически", "claude": "самим Claude Code"}[trigger]
	r.log(key, fmt.Sprintf("Сессия сжата %s: контекст %d%% → %d%%.", who,
		protocol.ContextPercent(pre, window), protocol.ContextPercent(post, window)))
}

// sessionCompacted — сессия шага сжата: размер, событие, журнал.
func (r *run) sessionCompacted(st *StageState, pre, post int64, trigger string) {
	if post > 0 {
		st.Context = post
	}
	r.compactedEvent(st.Key, st.Round, st.SessionID, pre, post, shownWindow(st), trigger)
	r.emitSession(st, true)
}

// compactNow сжимает сессию шага из цикла этапов. Сбой не роняет шаг:
// сессия продолжается как есть, причина — в журнале.
func (r *run) compactNow(ctx context.Context, st *StageState, session, trigger string) (*agent.Compaction, error) {
	if session == "" {
		session = st.SessionID
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	comp, err := agent.Compact(cctx, session, st.SessionCWD, st.SessionModel)
	if err != nil {
		if ctx.Err() != nil {
			// Пауза оборвала сжатие: просьба остаётся до продолжения.
			st.CompactPending = trigger
			return nil, err
		}
		st.CompactPending = ""
		r.log(st.Key, "Сжать сессию не вышло: "+err.Error()+" — продолжаю без сжатия.")
		return nil, err
	}
	st.CompactPending = ""
	r.sessionCompacted(st, comp.Pre, comp.Post, trigger)
	r.job.SaveState()
	return comp, nil
}

// compactRequest — «Сжать сессию» от человека для шага этого задания. Зовёт
// горутина сообщений: StageState не трогает, работает со снимком.
func (r *run) compactRequest(ctx context.Context, c *protocol.Compact) {
	reply := func(res *protocol.CompactResult) {
		res.ReqID, res.TaskID, res.Key, res.Round = c.ReqID, c.TaskID, c.Key, c.Round
		if r.job != nil && r.job.ex != nil {
			if err := r.job.ex.send(protocol.MsgCompactResult, r.job.ID, res); err != nil {
				r.job.ex.logf("ответ на сжатие сессии %s: %v", c.ReqID, err)
			}
		}
	}
	switch r.clar.requestCompact(c.Key, c.Round, "manual") {
	case "interrupt":
		r.log(c.Key, "Сжимаю сессию по кнопке — шаг продолжит работу в той же сессии.")
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	case "pending":
		r.setPending(c.Key, c.Round, "manual")
		r.log(c.Key, "Сожму сессию, когда шаг закончит прогон: прервать его нельзя.")
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	}
	in, round, ok := r.sessionSnapshot(c.Key, c.Round)
	if !ok {
		reply(&protocol.CompactResult{Status: "error", Error: "у шага нет сессии"})
		return
	}
	k := sessKey(c.Key, round)
	lock := r.stageLock(c.Key, round)
	r.sess.mu.Lock()
	if r.sess.closed || r.sess.busy[k] {
		busy := r.sess.busy[k]
		r.sess.mu.Unlock()
		msg := "задание заканчивается — сожмите после него"
		if busy {
			msg = "сессия этого шага уже сжимается"
		}
		reply(&protocol.CompactResult{Status: "error", Error: msg})
		return
	}
	if !lock.TryLock() {
		// Шаг занят (ждёт ответов на вопросы): сожмёт цикл этапов перед
		// следующим обращением к агенту.
		r.sess.mu.Unlock()
		r.setPending(c.Key, round, "manual")
		reply(&protocol.CompactResult{Status: "scheduled"})
		return
	}
	if r.sess.busy == nil {
		r.sess.busy = map[sref]bool{}
	}
	r.sess.busy[k] = true
	r.sess.wg.Add(1)
	r.sess.mu.Unlock()
	go func() {
		defer r.sess.wg.Done()
		defer lock.Unlock()
		defer func() {
			r.sess.mu.Lock()
			delete(r.sess.busy, k)
			r.sess.mu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		comp, err := agent.Compact(cctx, in.ID, in.CWD, in.Model)
		if err != nil {
			// Причину пишет в журнал таски сервис по этому ответу.
			reply(&protocol.CompactResult{Status: "error", Error: err.Error()})
			return
		}
		r.sess.mu.Lock()
		if comp.Post > 0 {
			if r.sess.compacted == nil {
				r.sess.compacted = map[sref]int64{}
			}
			r.sess.compacted[k] = comp.Post
			in.Context = comp.Post
			r.sess.info[k] = in
		}
		r.sess.mu.Unlock()
		win := in.Window
		if win == 0 {
			win = defaultWindow
		}
		r.compactedEvent(c.Key, round, in.ID, comp.Pre, comp.Post, win, "manual")
		reply(&protocol.CompactResult{Status: "done", Pre: comp.Pre, Post: comp.Post, Window: in.Window})
	}()
}
