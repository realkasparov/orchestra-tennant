package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Runner ведёт задание по этапам. Настоящая реализация — перенесённый цикл
// этапов оркестратора; в тестах ядра — заглушка. Возвращает терминальный статус
// таски: done, error или paused.
type Runner interface {
	Run(ctx context.Context, job *Job) (status string, err error)
}

// Cleaner умеет убирать за удалённой таской: рабочая копия и папка задачи
// лежат на диске исполнителя, и оркестратору до них не дотянуться.
type Cleaner interface {
	Cleanup(job *Job)
}

// Config — то, что исполнитель объявляет о себе и где хранит журнал.
type Config struct {
	DeviceKey string
	Hostname  string
	OS        string
	Version   string
	Slots     int
	Models    []string
	Skills    []string
	// ProjectsDir — папка проектов машины; объявляется в hello.
	ProjectsDir string
	// JournalDir — папка журнала незавершённых заданий.
	JournalDir string
	// Log — куда писать; nil — стандартный лог.
	Log func(format string, args ...any)
	// Trace — писать в лог каждый отправленный и принятый пакет: тип и
	// задание, без тела. Тело hello содержит ключ, тела событий — код проекта;
	// ни тому, ни другому в журнале не место.
	Trace bool
}

// Причины отмены задания.
const (
	CancelPause  = "pause"
	CancelDelete = "delete"
	// cancelReassigned — оркестратор при сверке сказал, что задание уже не за
	// нами: отменено или переотдано, пока связи не было.
	cancelReassigned = "reassigned"
)

// Executor — исполняющая сторона протокола.
type Executor struct {
	cfg     Config
	runner  Runner
	journal *Journal
	logf    func(string, ...any)

	mu   sync.Mutex
	conn protocol.Conn // текущее соединение; nil, пока связи нет
	jobs map[string]*Job
	// connected закрывается и пересоздаётся при каждом подключении: задания
	// ждут на нём границу этапа.
	connected chan struct{}
}

// New создаёт исполнителя. Журнал читается сразу: незавершённые задания
// прошлого запуска пойдут в hello.
func New(cfg Config, runner Runner) (*Executor, error) {
	if cfg.Slots < 1 {
		cfg.Slots = 1
	}
	j, err := OpenJournal(cfg.JournalDir)
	if err != nil {
		return nil, err
	}
	logf := cfg.Log
	if logf == nil {
		logf = log.Printf
	}
	e := &Executor{cfg: cfg, runner: runner, journal: j, logf: logf,
		jobs: map[string]*Job{}, connected: make(chan struct{})}
	records, errs := j.List()
	for _, err := range errs {
		e.logf("журнал: %v", err)
	}
	for _, r := range records {
		// Задание из журнала: процесс перезапустился посреди него. Оно не
		// идёт — его нужно получить заново с resume, — но оркестратору о нём
		// нужно сказать, иначе он будет ждать нас впустую.
		ended := make(chan struct{})
		close(ended) // прогона нет — ждать нечего
		e.jobs[r.JobID] = &Job{ID: r.JobID, Plan: r.Plan, Resume: r.Resume,
			lastAck: r.LastAck, orphan: true, ex: e, State: r.State, ended: ended}
	}
	return e, nil
}

// Serve держит соединение с оркестратором, пока жив ctx. Разрыв — не конец
// работы: задания продолжаются, события копятся, а по возвращении связи
// досылаются с последнего подтверждённого.
func (e *Executor) Serve(ctx context.Context, dial func() (protocol.Conn, error)) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := dial()
		if err != nil {
			e.logf("подключение: %v — повтор через %s", err, backoff)
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = grow(backoff)
			continue
		}
		// Recv не смотрит на ctx: остановка процесса иначе висела бы, пока
		// оркестратор не закроет сокет со своей стороны. Сторож живёт ровно
		// столько, сколько сессия, — иначе по горутине на каждое
		// переподключение копилось бы до конца работы демона.
		ended := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-ended:
			}
		}()
		started := time.Now()
		err = e.session(ctx, conn)
		close(ended)
		conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Сессия, прожившая заметное время, сбрасывает отступ; мгновенно
		// падающая (несовместимая схема, отказ по ключу) — нет: долбить
		// оркестратор раз в секунду одним и тем же вопросом бессмысленно.
		if time.Since(started) > 10*time.Second {
			backoff = time.Second
		}
		e.logf("сессия завершилась: %v — переподключение через %s", err, backoff)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = grow(backoff)
	}
}

func grow(d time.Duration) time.Duration {
	if d >= 30*time.Second {
		return 30 * time.Second
	}
	return d * 2
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// session — одно соединение от hello до разрыва.
func (e *Executor) session(ctx context.Context, conn protocol.Conn) error {
	hello := protocol.Hello{
		DeviceKey: e.cfg.DeviceKey, Hostname: e.cfg.Hostname, OS: e.cfg.OS,
		Version: e.cfg.Version, MinSchema: protocol.MinSchemaVersion,
		MaxSchema: protocol.SchemaVersion, Slots: e.cfg.Slots, ProjectsDir: e.cfg.ProjectsDir,
		Models: e.cfg.Models, Skills: e.cfg.Skills, Running: e.runningIDs(), Parked: e.parkedJobs(),
	}
	e.trace("→", protocol.MsgHello, "")
	if err := send(conn, protocol.MsgHello, "", hello); err != nil {
		return err
	}
	env, err := conn.Recv()
	if err != nil {
		return err
	}
	e.trace("←", env.Type, env.JobID)
	if env.Type != protocol.MsgWelcome {
		return fmt.Errorf("вместо welcome пришло %q", env.Type)
	}
	var welcome protocol.Welcome
	if err := json.Unmarshal(env.Body, &welcome); err != nil {
		return fmt.Errorf("welcome не разобрать: %w", err)
	}
	if welcome.Schema < protocol.MinSchemaVersion || welcome.Schema > protocol.SchemaVersion {
		return &protocol.ErrSchema{Got: welcome.Schema, Min: protocol.MinSchemaVersion, Max: protocol.SchemaVersion}
	}

	// Сверка: чего за нами больше нет — бросаем; что продолжается — досылаем.
	// Идущее задание останавливается, но память таски остаётся: после
	// перезапуска оркестратор ставит ту же таску новым заданием, и оно
	// продолжит с той же рабочей копией. Запись из журнала, о которой
	// оркестратор ничего не знает, — сирота прошлого запуска, её убираем.
	for _, id := range welcome.Cancel {
		if j, orphan := e.jobState(id); j != nil {
			if orphan {
				e.forget(id)
				continue
			}
			j.stop(cancelReassigned)
		}
	}
	// Откат отправленного — до того, как соединение станет видно отправке:
	// иначе новое событие успело бы уйти раньше досылки и оркестратор
	// отбросил бы досланное как устаревшее.
	for _, id := range welcome.Continue {
		if j := e.job(id); j != nil {
			j.rewind()
		}
	}
	e.mu.Lock()
	e.conn = conn
	close(e.connected)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.conn = nil
		e.connected = make(chan struct{})
		e.mu.Unlock()
	}()
	stop := make(chan struct{})
	defer close(stop)
	go e.renewLoop(ctx, stop)

	// Досылка — в отдельной горутине, а цикл чтения — сразу. Иначе на каждое
	// досланное событие приходит подтверждение, читать его некому, буфер
	// заполняется, и обе стороны встают: мы — на отправке, оркестратор — на
	// отправке подтверждения.
	for _, id := range welcome.Continue {
		if j := e.job(id); j != nil {
			go j.flush()
		}
	}

	for {
		env, err := conn.Recv()
		if err != nil {
			return err
		}
		e.trace("←", env.Type, env.JobID)
		e.handle(ctx, env)
	}
}

// trace пишет пакет в лог, если включена трассировка.
func (e *Executor) trace(dir, typ, jobID string) {
	// Подтверждения — по одному на событие; в журнале от них только шум.
	if !e.cfg.Trace || typ == protocol.MsgAck {
		return
	}
	if jobID == "" {
		e.logf("%s %s", dir, typ)
		return
	}
	e.logf("%s %s %s", dir, typ, jobID)
}

// handle разбирает одно входящее сообщение. Неизвестный тип пропускается с
// записью в лог: оркестратор новее нас может слать то, чего мы не знаем, и
// это не повод рвать соединение.
func (e *Executor) handle(ctx context.Context, env *protocol.Envelope) {
	switch env.Type {
	case protocol.MsgOffer:
		var offer protocol.Offer
		if err := json.Unmarshal(env.Body, &offer); err != nil || offer.Plan == nil {
			e.reject(env.JobID, "предложение не разобрать", false)
			return
		}
		e.accept(ctx, &offer)
	case protocol.MsgCancel:
		var c protocol.Cancel
		_ = json.Unmarshal(env.Body, &c)
		if env.JobID == "" && c.TaskID != 0 && c.Reason == CancelDelete {
			// Таска удалена, а задания у оркестратора уже нет: убираем всё,
			// что помним о ней, — рабочую копию и папку задачи.
			for _, ref := range e.parkedJobs() {
				if ref.TaskID == c.TaskID {
					if j, _ := e.jobState(ref.JobID); j != nil {
						if cl, ok := e.runner.(Cleaner); ok {
							cl.Cleanup(j)
						}
						e.forget(ref.JobID)
					}
				}
			}
			return
		}
		if j, orphan := e.jobState(env.JobID); j != nil {
			if c.Reason == "" {
				c.Reason = CancelPause
			}
			if orphan {
				// Не идёт, а оркестратор его снял или остановил. Удалённому в
				// журнале делать нечего; остановленное остаётся ждать
				// «Возобновить» — память таски ещё понадобится.
				if c.Reason == CancelDelete || c.Reason == cancelReassigned {
					if cl, ok := e.runner.(Cleaner); ok && c.Reason == CancelDelete {
						cl.Cleanup(j)
					}
					e.forget(j.ID)
				}
				return
			}
			j.stop(c.Reason)
		}
	case protocol.MsgAnswer:
		var a protocol.Answer
		if err := json.Unmarshal(env.Body, &a); err == nil {
			if j := e.job(env.JobID); j != nil {
				j.deliverAnswer(&a)
			}
		}
	case protocol.MsgMessage:
		var m protocol.Message
		if err := json.Unmarshal(env.Body, &m); err == nil {
			if j := e.job(env.JobID); j != nil {
				j.deliverMessage(&m)
			}
		}
	case protocol.MsgContinue:
		var c protocol.Continue
		_ = json.Unmarshal(env.Body, &c)
		if j := e.job(env.JobID); j != nil {
			j.deliverContinue(c.BudgetAck)
		}
	case protocol.MsgProject:
		e.handleProject(ctx, env)
	case protocol.MsgAck:
		var a protocol.Ack
		if err := json.Unmarshal(env.Body, &a); err == nil {
			if j := e.job(env.JobID); j != nil {
				j.acked(a.Seq)
			}
		}
	default:
		e.logf("неизвестное сообщение %q — пропущено", env.Type)
	}
}

// accept принимает или отвергает предложение. Отказ всегда с причиной и с
// признаком, стоит ли предлагать снова: «занят» — да, «не понимаю план» — нет.
func (e *Executor) accept(ctx context.Context, offer *protocol.Offer) {
	if err := offer.Plan.Validate(); err != nil {
		e.reject(offer.JobID, err.Error(), false)
		return
	}
	if missing := e.missing(offer.Plan); missing != "" {
		e.reject(offer.JobID, "на этой машине нет: "+missing, false)
		return
	}
	e.mu.Lock()
	existing := e.jobs[offer.JobID]
	// Память таски переживает и смену идентификатора задания: после ошибки
	// или перезапуска оркестратора «Возобновить» ставит новое задание той же
	// таски, а рабочая копия и сессии агента остались здесь под прежним.
	adopted := ""
	if existing == nil {
		for id, j := range e.jobs {
			if j.Plan.TaskID != offer.Plan.TaskID {
				continue
			}
			if j.orphan {
				existing, adopted = j, id
				break
			}
			if j.cancelReason() != "" {
				// Прежнее задание той же таски ещё останавливается: его
				// память возьмём, когда оно встанет, — иначе два прогона
				// писали бы одно состояние.
				e.mu.Unlock()
				go func() {
					select {
					case <-j.ended:
					case <-time.After(20 * time.Second):
					}
					e.accept(ctx, offer)
				}()
				return
			}
			// Таска уже идёт здесь другим заданием — второй прогон
			// исключён; оркестратор разберётся по сверке.
			e.mu.Unlock()
			e.reject(offer.JobID, "таска уже выполняется на этой машине", true)
			return
		}
	}
	if existing != nil && !existing.orphan {
		// Повторная доставка уже принятого: подтверждаем тем же
		// идентификатором, второго исполнения не начинаем.
		e.mu.Unlock()
		_ = e.send(protocol.MsgAccept, offer.JobID, nil)
		return
	}
	if e.activeLocked() >= e.cfg.Slots {
		e.mu.Unlock()
		e.reject(offer.JobID, "нет свободных мест", true)
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	j := &Job{ID: offer.JobID, Plan: offer.Plan, Resume: offer.Resume, Skip: offer.Done, ex: e,
		answers: make(chan *protocol.Answer, 8), messages: make(chan *protocol.Message, 8),
		cont: make(chan protocol.Continue, 1), cancel: cancel, ended: make(chan struct{})}
	if existing != nil {
		// Память прошлого запуска: сессии агента, прогоны, вопросы. Без неё
		// возобновление начинало бы этап заново, не зная, что можно продолжить.
		j.State = existing.State
	}
	if j.State == nil {
		// Состояние заводится до публикации задания: worktreeDir() читает
		// его из другой горутины.
		j.State = &TaskState{}
	}
	// Нумерация продолжается с того, что оркестратор уже видел, а не с
	// единицы: иначе новые события отбрасывались бы как дубли. Своя память
	// о подтверждениях (из журнала) — нижняя граница, оркестратор — верхняя.
	j.seq, j.lastAck = offer.LastSeq, offer.LastSeq
	if existing != nil && adopted == "" && existing.lastAck > j.seq {
		// Нумерация событий — на задание; от чужого идентификатора она не
		// наследуется.
		j.seq, j.lastAck = existing.lastAck, existing.lastAck
	}
	j.sent = j.lastAck
	e.jobs[j.ID] = j
	if adopted != "" {
		delete(e.jobs, adopted)
	}
	e.mu.Unlock()
	if adopted != "" {
		// Прежняя запись снимается с пометкой: запоздавшее подтверждение к
		// старому заданию не должно воскресить её рядом с новой.
		existing.retire()
	}

	if err := e.journal.Put(&Record{JobID: j.ID, TaskID: j.Plan.TaskID, Plan: j.Plan, Resume: j.Resume, State: j.State}); err != nil {
		// Без журнала задание не пережило бы перезапуск и пропало бы молча.
		// Честнее отказать с правом повтора — диск освободится, и его
		// предложат снова.
		e.mu.Lock()
		delete(e.jobs, j.ID)
		e.mu.Unlock()
		cancel()
		e.reject(j.ID, "журнал недоступен: "+err.Error(), true)
		return
	}
	if err := e.send(protocol.MsgAccept, j.ID, nil); err != nil {
		e.logf("accept %s: %v", j.ID, err)
	}
	var keys []string
	for _, s := range j.Plan.Stages {
		keys = append(keys, stageTitle(s.Key))
	}
	e.taskNote(j, "Задание принято: %s; рабочая область — %s; папка проекта %s", strings.Join(keys, " → "), j.Plan.Workspace, j.Plan.Project.Path)
	go e.run(jctx, j)
}

// run ведёт задание до конца и сообщает итог. Итог тоже может не дойти —
// тогда он уйдёт при следующем подключении вместе с досылкой событий.
func (e *Executor) run(ctx context.Context, j *Job) {
	defer close(j.ended)
	status, err := e.runner.Run(ctx, j)
	reason := ""
	if r := j.cancelReason(); r != "" {
		status, reason = "paused", r
		if r == CancelDelete {
			if c, ok := e.runner.(Cleaner); ok {
				c.Cleanup(j)
			}
		}
		if r == cancelReassigned {
			// Итога не будет: под этим идентификатором задание оркестратору
			// уже не нужно. Память таски — остаётся.
			e.park(j.ID)
			return
		}
	} else if ctx.Err() != nil {
		// Останавливается сам исполнитель (перезапуск демона, выключение), а
		// не таска. Отчитаться «ошибка» и стереть журнал значило бы объявить
		// таску проваленной из-за перезапуска. Задание остаётся в журнале и
		// вернётся с resume после старта.
		return
	} else if err != nil {
		if status == "" {
			status = "error"
		}
		reason = err.Error()
	}
	if status == "" {
		status = "done"
	}
	if reason != "" {
		e.taskNote(j, "Итог: %s — %s", statusTitle(status), reason)
	} else {
		e.taskNote(j, "Итог: %s", statusTitle(status))
	}
	j.finish(status, reason)
	j.flush()
}

// reject отвечает отказом на предложение.
func (e *Executor) reject(jobID, reason string, retryable bool) {
	if err := e.send(protocol.MsgReject, jobID, protocol.Reject{JobID: jobID, Reason: reason, Retryable: retryable}); err != nil {
		e.logf("reject %s: %v", jobID, err)
	}
}

// missing называет, чего нет на машине для плана.
func (e *Executor) missing(p *protocol.Plan) string {
	models := map[string]bool{}
	for _, m := range e.cfg.Models {
		models[m] = true
	}
	skills := map[string]bool{}
	for _, s := range e.cfg.Skills {
		skills[s] = true
	}
	for _, s := range p.Stages {
		if s.Model != "" && len(models) > 0 && !models[s.Model] {
			return "модели " + s.Model
		}
		if s.Skill != "" && len(skills) > 0 && !skills[s.Skill] {
			return "скилла " + s.Skill
		}
	}
	return ""
}

// renewLoop продлевает лизы идущих заданий, пока соединение живо.
func (e *Executor) renewLoop(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(protocol.LeaseRenew)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			for _, j := range e.activeJobs() {
				_ = e.send(protocol.MsgRenew, j.ID, nil)
			}
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// --- доступ к состоянию ---

func (e *Executor) job(id string) *Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.jobs[id]
}

// jobState — задание и признак, что оно не идёт (из журнала или после
// паузы). Признак читается под замком: park меняет его у живого задания.
func (e *Executor) jobState(id string) (*Job, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.jobs[id]
	if j == nil {
		return nil, false
	}
	return j, j.orphan
}

// park оставляет память задания после паузы или ошибки: оно больше не идёт
// и места не занимает, но рабочая копия, сессии агента и статусы этапов
// остаются в журнале до возобновления или удаления.
func (e *Executor) park(id string) {
	e.mu.Lock()
	j := e.jobs[id]
	if j != nil {
		j.orphan = true
	}
	e.mu.Unlock()
	if j != nil {
		j.SaveState()
	}
}

func (e *Executor) forget(id string) {
	e.mu.Lock()
	j := e.jobs[id]
	delete(e.jobs, id)
	e.mu.Unlock()
	if j != nil {
		j.retire()
	} else {
		_ = e.journal.Remove(id)
	}
}

// runningIDs — что исполнитель действительно ведёт. Задания из журнала
// после перезапуска сюда не попадают: назвать их идущими значило бы, что
// оркестратор ждёт от них событий, которых не будет, — и вернул бы их с
// resume только по истечении таймаута этапа. Не названные, они сразу
// становятся ждущими и предлагаются заново.
// ActiveJobs — сколько заданий исполнитель ведёт сейчас.
func (e *Executor) ActiveJobs() int { return len(e.runningIDs()) }

func (e *Executor) runningIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.jobs))
	for id, j := range e.jobs {
		if !j.orphan {
			out = append(out, id)
		}
	}
	return out
}

// parkedJobs — задания, которые не идут, но чья память хранится: из
// журнала после перезапуска и после паузы или ошибки.
func (e *Executor) parkedJobs() []protocol.JobRef {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []protocol.JobRef
	for id, j := range e.jobs {
		if j.orphan {
			out = append(out, protocol.JobRef{JobID: id, TaskID: j.Plan.TaskID})
		}
	}
	return out
}

func (e *Executor) activeJobs() []*Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*Job
	for _, j := range e.jobs {
		if !j.orphan && !j.isFinished() {
			out = append(out, j)
		}
	}
	return out
}

// activeLocked считает занятые места: идущие задания. Осиротевшие из журнала
// не идут и места не занимают; они получат своё место, когда вернутся с
// resume и пройдут через accept.
func (e *Executor) activeLocked() int {
	n := 0
	for _, j := range e.jobs {
		if !j.orphan && !j.isFinished() {
			n++
		}
	}
	return n
}

// Connected сообщает, есть ли сейчас связь.
func (e *Executor) Connected() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn != nil
}

// --- отправка ---

var errOffline = errors.New("связи с оркестратором нет")

func send(conn protocol.Conn, typ, jobID string, body any) error {
	env := &protocol.Envelope{Type: typ, JobID: jobID}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		env.Body = raw
	}
	return conn.Send(env)
}

func (e *Executor) send(typ, jobID string, body any) error {
	e.mu.Lock()
	conn := e.conn
	e.mu.Unlock()
	if conn == nil {
		return errOffline
	}
	e.trace("→", typ, jobID)
	return send(conn, typ, jobID, body)
}
