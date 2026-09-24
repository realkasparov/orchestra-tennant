package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/realkasparov/orchestra-tennant/dispatch"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Мини-оркестратор на другом конце трубы: настоящий Dispatcher, приём событий
// с отбрасыванием дублей по номеру, подтверждения. Ровно то, что делает
// настоящий координатор, без базы и браузера.
type fakeOrch struct {
	t    *testing.T
	disp *dispatch.Dispatcher

	mu     sync.Mutex
	conn   protocol.Conn
	events map[string][]*protocol.Event // jobID → принятые, без дублей
	seen   map[string]int64             // jobID → последний номер
	done   map[string]protocol.Done
	rej    map[string]protocol.Reject
	hello  *protocol.Hello
	device int64
}

func newFakeOrch(t *testing.T) *fakeOrch {
	return &fakeOrch{t: t, disp: dispatch.NewDispatcher(nil), device: 1,
		events: map[string][]*protocol.Event{}, seen: map[string]int64{},
		done: map[string]protocol.Done{}, rej: map[string]protocol.Reject{}}
}

// attach принимает соединение: hello → welcome, затем цикл чтения в фоне.
func (o *fakeOrch) attach(conn protocol.Conn) {
	env, err := conn.Recv()
	if err != nil {
		o.t.Errorf("hello: %v", err)
		return
	}
	if env.Type != protocol.MsgHello {
		o.t.Errorf("вместо hello пришло %q", env.Type)
		return
	}
	var h protocol.Hello
	if err := json.Unmarshal(env.Body, &h); err != nil {
		o.t.Errorf("hello: %v", err)
		return
	}
	rc := o.disp.AddExecutor(o.device, dispatch.Capabilities{Slots: h.Slots, Models: h.Models, Skills: h.Skills}, h.Running)
	w := protocol.Welcome{DeviceID: o.device, Schema: protocol.SchemaVersion,
		LeaseTTLSec: 90, RenewSec: 30, Continue: rc.Continue, Cancel: rc.Cancel}
	raw, _ := json.Marshal(w)
	if err := conn.Send(&protocol.Envelope{Type: protocol.MsgWelcome, Body: raw}); err != nil {
		o.t.Errorf("welcome: %v", err)
		return
	}
	o.mu.Lock()
	o.conn, o.hello = conn, &h
	o.mu.Unlock()
	go o.readLoop(conn)
}

func (o *fakeOrch) readLoop(conn protocol.Conn) {
	for {
		env, err := conn.Recv()
		if err != nil {
			o.mu.Lock()
			if o.conn == conn {
				o.conn = nil
			}
			o.mu.Unlock()
			o.disp.RemoveExecutor(o.device)
			return
		}
		switch env.Type {
		case protocol.MsgAccept:
			_ = o.disp.Accept(env.JobID, o.device)
		case protocol.MsgReject:
			var r protocol.Reject
			_ = json.Unmarshal(env.Body, &r)
			o.mu.Lock()
			o.rej[env.JobID] = r
			o.mu.Unlock()
			_ = o.disp.Reject(env.JobID, o.device, r.Retryable)
		case protocol.MsgRenew:
			_ = o.disp.Renew(env.JobID, o.device)
		case protocol.MsgEvent:
			var ev protocol.Event
			_ = json.Unmarshal(env.Body, &ev)
			o.mu.Lock()
			if ev.Seq > o.seen[env.JobID] {
				o.seen[env.JobID] = ev.Seq
				o.events[env.JobID] = append(o.events[env.JobID], &ev)
			}
			last := o.seen[env.JobID]
			o.mu.Unlock()
			ack, _ := json.Marshal(protocol.Ack{JobID: env.JobID, Seq: last})
			_ = conn.Send(&protocol.Envelope{Type: protocol.MsgAck, JobID: env.JobID, Body: ack})
		case protocol.MsgDone:
			var d protocol.Done
			_ = json.Unmarshal(env.Body, &d)
			o.mu.Lock()
			o.done[env.JobID] = d
			o.mu.Unlock()
			_ = o.disp.Finish(env.JobID, o.device)
		}
	}
}

// offer ставит таску в очередь и предлагает её исполнителю.
func (o *fakeOrch) offer(plan *protocol.Plan, resume bool) string {
	o.t.Helper()
	if _, err := o.disp.Submit(plan.TaskID, plan, resume); err != nil && !errors.Is(err, dispatch.ErrTaskQueued) {
		o.t.Fatal(err)
	}
	off := o.disp.Next(o.device)
	if off == nil {
		o.t.Fatal("очередь ничего не выдала")
	}
	o.mu.Lock()
	off.LastSeq = o.seen[off.JobID]
	o.mu.Unlock()
	o.sendTo(protocol.MsgOffer, off.JobID, off)
	return off.JobID
}

func (o *fakeOrch) sendTo(typ, jobID string, body any) {
	o.t.Helper()
	o.mu.Lock()
	conn := o.conn
	o.mu.Unlock()
	if conn == nil {
		o.t.Fatal("оркестратор без соединения")
	}
	raw, _ := json.Marshal(body)
	if err := conn.Send(&protocol.Envelope{Type: typ, JobID: jobID, Body: raw}); err != nil {
		o.t.Fatal(err)
	}
}

func (o *fakeOrch) waitDone(jobID string, timeout time.Duration) (protocol.Done, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		d, ok := o.done[jobID]
		o.mu.Unlock()
		if ok {
			return d, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return protocol.Done{}, false
}

func (o *fakeOrch) waitRejected(jobID string, timeout time.Duration) (protocol.Reject, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		r, ok := o.rej[jobID]
		o.mu.Unlock()
		if ok {
			return r, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return protocol.Reject{}, false
}

// --- заглушка исполнения ---

// scriptRunner играет роль цикла этапов: шлёт заданное число событий по
// команде теста и ждёт, что скажут дальше.
type scriptRunner struct {
	mu      sync.Mutex
	started chan *Job
	steps   chan func(j *Job) // nil закрывает: завершиться done
}

func newScript() *scriptRunner {
	return &scriptRunner{started: make(chan *Job, 4), steps: make(chan func(*Job), 16)}
}

func (r *scriptRunner) Run(ctx context.Context, j *Job) (string, error) {
	r.started <- j
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case step, ok := <-r.steps:
			if !ok || step == nil {
				return "done", nil
			}
			step(j)
		}
	}
}

func (r *scriptRunner) emit(n int) {
	r.steps <- func(j *Job) {
		for i := 0; i < n; i++ {
			j.Emit("execute", "log", map[string]any{"text": "шаг"})
		}
	}
}

func (r *scriptRunner) finish() { r.steps <- nil }

// --- обвязка ---

type rig struct {
	t     *testing.T
	orch  *fakeOrch
	ex    *Executor
	run   *scriptRunner
	dir   string
	dials chan protocol.Conn
	stop  context.CancelFunc
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	if cfg.JournalDir == "" {
		cfg.JournalDir = t.TempDir()
	}
	cfg.Log = func(string, ...any) {}
	run := newScript()
	ex, err := New(cfg, run)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, orch: newFakeOrch(t), ex: ex, run: run, dir: cfg.JournalDir,
		dials: make(chan protocol.Conn, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	t.Cleanup(cancel)
	go ex.Serve(ctx, func() (protocol.Conn, error) { //nolint:errcheck
		c, ok := <-r.dials
		if !ok {
			return nil, errors.New("закрыто")
		}
		return c, nil
	})
	return r
}

// connect даёт исполнителю новую трубу и подключает к ней оркестратор.
func (r *rig) connect() protocol.Conn {
	a, b := protocol.Pipe()
	r.dials <- a
	r.orch.attach(b)
	return b
}

func (r *rig) waitConnected() {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.ex.Connected() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("исполнитель не подключился")
}

func plan(taskID int64) *protocol.Plan {
	return &protocol.Plan{
		// Схема 1: исполнитель переводит её в схему 2 при приёме.
		SchemaVersion: 1, TaskID: taskID, Title: "таска",
		Project:    protocol.Project{ID: 1, Name: "demo", Path: "/repo", BaseBranch: "main"},
		Stages:     []protocol.Stage{{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "high"}},
		Continuity: "per_stage", Workspace: "worktree", StageTimeout: protocol.Seconds(1800),
	}
}

// --- тесты ---

func TestJobRunsAndReportsDone(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	r.connect()
	r.waitConnected()

	job := r.orch.offer(plan(10), false)
	select {
	case <-r.run.started:
	case <-time.After(2 * time.Second):
		t.Fatal("задание не начато")
	}
	r.run.emit(5)
	r.run.finish()

	d, ok := r.orch.waitDone(job, 3*time.Second)
	if !ok || d.Status != "done" {
		t.Fatalf("итог: %+v %v", d, ok)
	}
	r.orch.mu.Lock()
	n := len(r.orch.events[job])
	r.orch.mu.Unlock()
	if n != 5 {
		t.Errorf("дошло %d событий из 5", n)
	}
	// Память таски остаётся (правка к готовой таске продолжит с той же
	// рабочей копии), но идущим задание не считается и в hello идёт как
	// отложенное, а не как ведущееся.
	if recs, _ := r.ex.journal.List(); len(recs) != 1 {
		t.Errorf("память завершённой таски должна остаться в журнале: %d", len(recs))
	}
	if r.ex.ActiveJobs() != 0 || len(r.ex.parkedJobs()) != 1 {
		t.Errorf("завершённое задание: идущих %d, отложенных %d", r.ex.ActiveJobs(), len(r.ex.parkedJobs()))
	}
}

// Разрыв посреди задания: события копятся и досылаются, ничего не теряется и
// не задваивается, задание завершается штатно.
func TestReconnectResendsWithoutDuplicates(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	first := r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.run.emit(3)
	time.Sleep(50 * time.Millisecond) // первые три дошли и подтверждены

	first.Close() // разрыв
	time.Sleep(50 * time.Millisecond)
	r.run.emit(4) // без связи: копятся
	if r.ex.Connected() {
		t.Fatal("исполнитель считает себя подключённым после разрыва")
	}

	r.connect()
	r.waitConnected()
	r.run.emit(2)
	r.run.finish()
	if _, ok := r.orch.waitDone(job, 3*time.Second); !ok {
		t.Fatal("задание не завершилось после переподключения")
	}
	r.orch.mu.Lock()
	evs := r.orch.events[job]
	r.orch.mu.Unlock()
	if len(evs) != 9 {
		t.Fatalf("дошло %d событий из 9", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("номера сбиты: позиция %d несёт %d", i, ev.Seq)
		}
	}
}

// Отмена прерывает работу, итог — пауза с причиной.
func TestCancelPausesJob(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.orch.sendTo(protocol.MsgCancel, job, protocol.Cancel{JobID: job, Reason: CancelPause})
	d, ok := r.orch.waitDone(job, 3*time.Second)
	if !ok || d.Status != "paused" || d.Reason != CancelPause {
		t.Fatalf("итог после отмены: %+v %v", d, ok)
	}
}

// Плана с моделью, которой нет на машине, исполнитель не принимает —
// отказывает без права повтора и с причиной.
func TestRejectsPlanBeyondCapabilities(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1, Models: []string{"claude-fable-5"}})
	r.connect()
	r.waitConnected()
	// Очередь с объявленными моделями сама не предложит план с opus; шлём
	// предложение мимо неё, как сделал бы оркестратор старее исполнителя.
	off := protocol.Offer{JobID: "j-bad", Plan: plan(10)}
	r.orch.sendTo(protocol.MsgOffer, off.JobID, off)
	rej, ok := r.orch.waitRejected(off.JobID, 2*time.Second)
	if !ok {
		t.Fatal("отказа не было")
	}
	if rej.Retryable || rej.Reason == "" {
		t.Errorf("отказ должен быть без повтора и с причиной: %+v", rej)
	}
}

// Сверх числа мест — отказ с правом повтора.
func TestRejectsWhenSlotsFull(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	r.connect()
	r.waitConnected()
	r.orch.offer(plan(10), false)
	<-r.run.started
	off := protocol.Offer{JobID: "j-extra", Plan: plan(11)}
	r.orch.sendTo(protocol.MsgOffer, off.JobID, off)
	rej, ok := r.orch.waitRejected(off.JobID, 2*time.Second)
	if !ok || !rej.Retryable {
		t.Fatalf("ожидался отказ с правом повтора: %+v %v", rej, ok)
	}
}

// Повторная доставка принятого задания не запускает его второй раз.
func TestDuplicateOfferIsIdempotent(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 2})
	r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.orch.sendTo(protocol.MsgOffer, job, protocol.Offer{JobID: job, Plan: plan(10)})
	select {
	case <-r.run.started:
		t.Fatal("задание запущено второй раз")
	case <-time.After(200 * time.Millisecond):
	}
}

// Исполнитель упал посреди задания и перезапустился: журнал называет его в
// hello, оркестратор возвращает с resume, а нумерация событий продолжается —
// новые события не отбрасываются как виденные.
func TestRestartResumesFromJournal(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, Config{DeviceKey: "k", Slots: 1, JournalDir: dir})
	first := r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.run.emit(3)
	time.Sleep(50 * time.Millisecond)
	orch := r.orch
	// «Падение»: связь пропала, процесс исчез, ничего не отправив. Журнал на
	// диске остался. Старый исполнитель просто бросаем.
	first.Close()
	r.stop()
	time.Sleep(50 * time.Millisecond)

	// Новый процесс на том же журнале, тот же оркестратор.
	run2 := newScript()
	ex2, err := New(Config{DeviceKey: "k", Slots: 1, JournalDir: dir, Log: func(string, ...any) {}}, run2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := protocol.Pipe()
	go ex2.Serve(ctx, func() (protocol.Conn, error) { return a, nil }) //nolint:errcheck
	orch.attach(b)

	orch.mu.Lock()
	hello := orch.hello
	orch.mu.Unlock()
	// Задание из журнала не идёт, и называть его идущим нельзя: оркестратор
	// ждал бы от него событий до таймаута этапа. Не названное, оно сразу
	// становится ждущим и предлагается заново с recovery.
	if hello == nil || len(hello.Running) != 0 {
		t.Fatalf("hello назвал идущим задание, которое не идёт: %+v", hello)
	}
	off := orch.disp.Next(orch.device)
	if off == nil || off.JobID != job || !off.Resume {
		t.Fatalf("очередь не предложила задание заново с recovery: %+v", off)
	}
	orch.mu.Lock()
	off.LastSeq = orch.seen[job]
	orch.mu.Unlock()
	orch.sendTo(protocol.MsgOffer, off.JobID, off)
	j2 := <-run2.started
	if !j2.Resume {
		t.Error("возобновление не помечено")
	}
	run2.emit(2)
	run2.finish()
	if _, ok := orch.waitDone(job, 3*time.Second); !ok {
		t.Fatal("задание не завершилось после перезапуска")
	}
	orch.mu.Lock()
	evs := orch.events[job]
	orch.mu.Unlock()
	if len(evs) != 5 {
		t.Fatalf("после перезапуска дошло %d событий из 5 — новые отброшены как дубли?", len(evs))
	}
	if evs[4].Seq != 5 {
		t.Errorf("нумерация не продолжилась: последний номер %d", evs[4].Seq)
	}
}

// Остановка самого исполнителя посреди задания — не провал таски: итог не
// отправляется, журнал остаётся, задание вернётся с resume после старта.
func TestShutdownKeepsJobForResume(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, Config{DeviceKey: "k", Slots: 1, JournalDir: dir})
	r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.stop()
	time.Sleep(100 * time.Millisecond)
	if _, ok := r.orch.waitDone(job, 200*time.Millisecond); ok {
		t.Fatal("остановка исполнителя отчиталась итогом таски")
	}
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := j.List()
	if len(recs) != 1 || recs[0].JobID != job {
		t.Errorf("задание не осталось в журнале: %+v", recs)
	}
}

// Досылка и новые события идут из разных горутин, и порядок номеров обязан
// сохраниться: событие N+1 раньше N потеряло бы N на стороне оркестратора.
func TestFlushAndEmitKeepOrder(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	first := r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	first.Close()
	time.Sleep(30 * time.Millisecond)
	r.run.emit(200) // копятся без связи

	// Переподключение и поток новых событий одновременно с досылкой.
	r.connect()
	for i := 0; i < 20; i++ {
		r.run.emit(10)
	}
	r.run.finish()
	if _, ok := r.orch.waitDone(job, 5*time.Second); !ok {
		t.Fatal("задание не завершилось")
	}
	r.orch.mu.Lock()
	evs := r.orch.events[job]
	r.orch.mu.Unlock()
	if len(evs) != 400 {
		t.Fatalf("дошло %d событий из 400 — порядок нарушен и часть отброшена", len(evs))
	}
}

// Остановка процесса не должна ждать, пока оркестратор закроет сокет: Recv
// сам по себе ctx не видит.
func TestServeReturnsOnShutdownWhileConnected(t *testing.T) {
	cfg := Config{DeviceKey: "k", Slots: 1, JournalDir: t.TempDir(), Log: func(string, ...any) {}}
	ex, err := New(cfg, newScript())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, b := protocol.Pipe()
	done := make(chan error, 1)
	go func() { done <- ex.Serve(ctx, func() (protocol.Conn, error) { return a, nil }) }()
	orch := newFakeOrch(t)
	orch.attach(b)
	deadline := time.Now().Add(2 * time.Second)
	for !ex.Connected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // оркестратор ничего не закрывает
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve не вернулся после остановки: завис в Recv")
	}
}

// Отмена задания, которое исполнитель знает только по журналу, стирает его из
// журнала — иначе оно всплывало бы при каждом старте.
func TestCancelOrphanClearsJournal(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	if err := j.Put(&Record{JobID: "old", TaskID: 10, Plan: plan(10)}); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, Config{DeviceKey: "k", Slots: 1, JournalDir: dir})
	r.connect()
	r.waitConnected()
	r.orch.sendTo(protocol.MsgCancel, "old", protocol.Cancel{JobID: "old", Reason: CancelDelete})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := j.List(); len(recs) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("отменённое задание осталось в журнале")
}

// Пауза не стирает память таски: рабочая копия и статусы этапов остаются, и
// возобновление — под тем же или под новым идентификатором задания —
// продолжает с них, а не заводит worktree поверх существующего.
func TestPauseKeepsStateForResume(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	first := <-r.run.started
	r.run.steps <- func(j *Job) {
		j.State = &TaskState{WorktreeDir: "/repo-agent-worktrees/10", Stages: []*StageState{{Key: "execute", Round: 1, Status: "running"}}}
		j.SaveState()
	}
	r.orch.sendTo(protocol.MsgCancel, job, protocol.Cancel{JobID: job, Reason: CancelPause})
	if d, ok := r.orch.waitDone(job, 3*time.Second); !ok || d.Status != "paused" {
		t.Fatalf("итог после паузы: %+v %v", d, ok)
	}
	if r.ex.ActiveJobs() != 0 {
		t.Fatal("остановленное задание не должно считаться идущим")
	}
	if recs, _ := r.ex.journal.List(); len(recs) != 1 || recs[0].State == nil || recs[0].State.WorktreeDir == "" {
		t.Fatalf("память таски должна остаться в журнале: %+v", recs)
	}

	// Возобновление тем же заданием.
	off := protocol.Offer{JobID: job, Plan: plan(10), Resume: true}
	r.orch.sendTo(protocol.MsgOffer, off.JobID, off)
	second := <-r.run.started
	if second == first || second.State == nil || second.State.WorktreeDir != "/repo-agent-worktrees/10" {
		t.Fatalf("возобновление потеряло память таски: %+v", second.State)
	}
	r.orch.sendTo(protocol.MsgCancel, job, protocol.Cancel{JobID: job, Reason: CancelPause})
	if _, ok := r.orch.waitDone(job, 3*time.Second); !ok {
		t.Fatal("итога после второй паузы не было")
	}

	// Возобновление новым заданием той же таски (оркестратор перезапустился).
	other := r.orch.offer(plan(10), true)
	third := <-r.run.started
	if third.ID != other || third.State == nil || third.State.WorktreeDir != "/repo-agent-worktrees/10" {
		t.Fatalf("новое задание той же таски не унаследовало память: %+v", third.State)
	}
	if recs, _ := r.ex.journal.List(); len(recs) != 1 || recs[0].JobID != other {
		t.Fatalf("в журнале должно остаться одно задание — новое: %+v", recs)
	}
	r.run.finish()
	if d, ok := r.orch.waitDone(other, 3*time.Second); !ok || d.Status != "done" {
		t.Fatalf("итог: %+v %v", d, ok)
	}
	// Удаление стирает память.
	r.orch.sendTo(protocol.MsgCancel, other, protocol.Cancel{JobID: other, Reason: CancelDelete})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := r.ex.journal.List(); len(recs) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("после удаления память таски осталась")
}

// Удаление остановленной таски убирает и её память.
func TestDeleteOfParkedJobForgets(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 1})
	r.connect()
	r.waitConnected()
	job := r.orch.offer(plan(10), false)
	<-r.run.started
	r.orch.sendTo(protocol.MsgCancel, job, protocol.Cancel{JobID: job, Reason: CancelPause})
	if _, ok := r.orch.waitDone(job, 3*time.Second); !ok {
		t.Fatal("итога после паузы не было")
	}
	r.orch.sendTo(protocol.MsgCancel, job, protocol.Cancel{JobID: job, Reason: CancelDelete})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := r.ex.journal.List(); len(recs) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("удалённое задание осталось в журнале")
}

// Оркестратор перезапустился и снял задание сверкой, а потом поставил ту
// же таску новым заданием: память таски переходит к нему, второго прогона
// поверх ещё останавливающегося не начинается.
func TestReassignedJobHandsStateToSuccessor(t *testing.T) {
	r := newRig(t, Config{DeviceKey: "k", Slots: 2})
	conn := r.connect()
	r.waitConnected()
	r.orch.offer(plan(10), false)
	first := <-r.run.started
	r.run.steps <- func(j *Job) {
		j.State = &TaskState{WorktreeDir: "/wt/10"}
		j.SaveState()
	}
	time.Sleep(50 * time.Millisecond)
	// Разрыв и второе соединение: оркестратор задание не знает — Cancel
	// при сверке.
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	r.orch.disp.Cancel(10) // оркестратор «забыл» задание
	r.connect()
	r.waitConnected()
	select {
	case <-first.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("снятое сверкой задание не остановилось")
	}
	other := r.orch.offer(plan(10), true)
	second := <-r.run.started
	if second.ID != other || second.State == nil || second.State.WorktreeDir != "/wt/10" {
		t.Fatalf("новое задание не унаследовало память: %+v", second.State)
	}
	if got := r.ex.ActiveJobs(); got != 1 {
		t.Fatalf("идущих заданий %d, ожидалось 1", got)
	}
	r.run.finish()
	if d, ok := r.orch.waitDone(other, 3*time.Second); !ok || d.Status != "done" {
		t.Fatalf("итог: %+v %v", d, ok)
	}
}
