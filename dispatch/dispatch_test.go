package dispatch

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Часы под контролем теста: поведение лиза иначе пришлось бы проверять
// настоящими паузами, а полутораминутный тест не проверяет ничего.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func dispatchPlan(taskID int64) *protocol.Plan {
	return &protocol.Plan{
		SchemaVersion: 1, // схема 1 принимается на переходный релиз
		TaskID:        taskID,
		Title:         "таска",
		Project:       protocol.Project{ID: 1, Name: "demo", Path: "/repo", BaseBranch: "main"},
		Stages:        []protocol.Stage{{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "high"}},
		Continuity:    "per_stage",
		Workspace:     "worktree",
		StageTimeout:  protocol.Seconds(1800),
	}
}

func newTestDispatcher() (*Dispatcher, *fakeClock) {
	c := newClock()
	return NewDispatcher(c.now), c
}

func TestJobFlowsThroughLease(t *testing.T) {
	d, clock := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	if _, err := d.Submit(10, dispatchPlan(10), false); err != nil {
		t.Fatal(err)
	}

	offer := d.Next(1)
	if offer == nil || offer.Plan.TaskID != 10 {
		t.Fatalf("задание не выдано: %+v", offer)
	}
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	if st, dev, _ := d.State(10); st != JobRunning || dev != 1 {
		t.Errorf("состояние после приёма: %s / %d", st, dev)
	}

	// Продление держит лиз живым сколь угодно долго.
	for i := 0; i < 5; i++ {
		clock.advance(protocol.LeaseRenew)
		if err := d.Renew(offer.JobID, 1); err != nil {
			t.Fatalf("продление %d: %v", i, err)
		}
		if lost := d.Sweep(); len(lost) != 0 {
			t.Fatalf("продлённое задание отобрано: %v", lost)
		}
	}
	d.Finish(offer.JobID, 1)
	if _, _, ok := d.State(10); ok {
		t.Error("завершённое задание осталось в очереди")
	}
}

// Предложение без подтверждения не должно держать таску весь срок лиза:
// молчащая машина иначе задерживала бы её на полторы минуты.
func TestUnacceptedOfferExpiresQuickly(t *testing.T) {
	d, clock := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	if d.Next(1) == nil {
		t.Fatal("задание не выдано")
	}
	clock.advance(OfferTTL / 2)
	d.Sweep()
	if got := d.Waiting(); len(got) != 0 {
		t.Fatal("задание отобрано раньше срока предложения")
	}
	clock.advance(OfferTTL)
	d.Sweep()
	if got := d.Waiting(); len(got) != 1 || got[0] != 10 {
		t.Errorf("непринятое предложение не вернулось в очередь: %v", got)
	}
}

// Повторная доставка того же задания не создаёт второго исполнения.
func TestAcceptIsIdempotent(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 2}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Errorf("повторное подтверждение отвергнуто: %v", err)
	}
	// Второе подтверждение не должно занимать второе место: при двух местах
	// следующая таска обязана уехать на ту же машину.
	d.Submit(11, dispatchPlan(11), false) //nolint:errcheck
	if next := d.Next(1); next == nil || next.Plan.TaskID != 11 {
		t.Error("повторное подтверждение съело свободное место")
	}
	// Чужое устройство подтвердить не может: иначе перехват задания на лету.
	if err := d.Accept(offer.JobID, 2); !errors.Is(err, ErrOtherDevice) {
		t.Errorf("чужое подтверждение: %v", err)
	}
	if err := d.Renew(offer.JobID, 2); !errors.Is(err, ErrOtherDevice) {
		t.Errorf("чужое продление: %v", err)
	}
}

// Одна таска — одно задание: иначе она поехала бы на двух машинах разом.
func TestOneJobPerTask(t *testing.T) {
	d, _ := newTestDispatcher()
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	if _, err := d.Submit(10, dispatchPlan(10), false); !errors.Is(err, ErrTaskQueued) {
		t.Errorf("вторая постановка той же таски: %v", err)
	}
}

// Отказ «занят» возвращает задание всем, отказ «не понял план» — только этой
// машине: другая может справиться, и отменять таску целиком не за что.
func TestRejectKinds(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck

	offer := d.Next(1)
	if err := d.Reject(offer.JobID, 1, false); err != nil { // не понял план
		t.Fatal(err)
	}
	if again := d.Next(1); again != nil {
		t.Error("задание снова предложено машине, которая его не поняла")
	}
	other := d.Next(2)
	if other == nil {
		t.Fatal("другой машине задание не предложено")
	}
	if err := d.Reject(other.JobID, 2, true); err != nil { // просто занят
		t.Fatal(err)
	}
	if back := d.Next(2); back == nil {
		t.Error("после отказа «занят» задание больше не предлагается")
	}
}

// Разрыв не возвращает принятое задание в очередь: рабочая копия на той
// машине, и другая возобновить не сможет. Предложенное, но не принятое —
// возвращается: на той стороне ещё ничего нет.
func TestDisconnectPinsAcceptedJob(t *testing.T) {
	d, clock := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 2}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	d.Submit(11, dispatchPlan(11), false) //nolint:errcheck
	accepted, offeredOnly := d.Next(1), d.Next(1)
	if accepted == nil || offeredOnly == nil {
		t.Fatal("задания не выданы")
	}
	if err := d.Accept(accepted.JobID, 1); err != nil {
		t.Fatal(err)
	}

	d.RemoveExecutor(1)
	// Непринятое возвращается в очередь по истечении своего короткого срока,
	// а не сразу: короткое окно нужно, чтобы вернувшаяся машина могла
	// назвать его в hello, если подтверждение потерялось.
	clock.advance(OfferTTL + time.Second)
	d.Sweep()
	if got := d.Waiting(); len(got) != 1 || got[0] != 11 {
		t.Errorf("в общей очереди должна быть только непринятая таска: %v", got)
	}
	if st, dev, _ := d.State(10); st != JobDetached || dev != 1 {
		t.Errorf("принятое задание не закреплено: %s / %d", st, dev)
	}
	// Другой машине закреплённое задание не достаётся.
	if other := d.Next(2); other != nil && other.Plan.TaskID == 10 {
		t.Error("закреплённое задание уехало на другую машину")
	}

	// Машина не вернулась за таймаут этапа — таска ждёт устройство, но всё
	// ещё никому другому не предлагается.
	clock.advance(dispatchPlan(10).StageTimeout.Duration() + time.Second)
	if waiting := d.Sweep(); len(waiting) != 1 || waiting[0] != 10 {
		t.Errorf("таска не перешла в ожидание устройства: %v", waiting)
	}
	if st, _, _ := d.State(10); st != JobWaiting {
		t.Errorf("состояние после таймаута: %s", st)
	}
	if got := d.Waiting(); len(got) != 1 {
		t.Errorf("закреплённое задание попало в общую очередь: %v", got)
	}
}

// Машина вернулась и назвала задание — работа продолжается без повторного
// предложения. Не назвала (упала и потеряла) — задание предлагается ей же с
// возобновлением. Назвала чужое — ей велят бросить.
func TestReconnectReconciles(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 3}, nil)
	for _, id := range []int64{10, 11} {
		d.Submit(id, dispatchPlan(id), false) //nolint:errcheck
	}
	kept, lost := d.Next(1), d.Next(1)
	for _, o := range []*protocol.Offer{kept, lost} {
		if err := d.Accept(o.JobID, 1); err != nil {
			t.Fatal(err)
		}
	}
	d.RemoveExecutor(1)

	rc := d.AddExecutor(1, Capabilities{Slots: 3}, []string{kept.JobID, "stale-job"})
	if len(rc.Continue) != 1 || rc.Continue[0] != kept.JobID {
		t.Errorf("названное задание не продолжено: %+v", rc)
	}
	if len(rc.Cancel) != 1 || rc.Cancel[0] != "stale-job" {
		t.Errorf("чужое задание не отменено: %+v", rc)
	}
	if len(rc.Resume) != 1 || rc.Resume[0] != 11 {
		t.Errorf("потерянное задание не помечено к возобновлению: %+v", rc)
	}
	if st, _, _ := d.State(10); st != JobRunning {
		t.Errorf("продолженное задание не в работе: %s", st)
	}
	// Потерянное предлагается той же машине с возобновлением.
	again := d.Next(1)
	if again == nil || again.JobID != lost.JobID || !again.Resume {
		t.Fatalf("потерянное задание не предложено заново: %+v", again)
	}
	// Отказ от возобновления оставляет задание за машиной, а не в общей очереди.
	if err := d.Reject(again.JobID, 1, true); err != nil {
		t.Fatal(err)
	}
	if st, dev, _ := d.State(11); st != JobWaiting || dev != 1 {
		t.Errorf("после отказа от возобновления: %s / %d", st, dev)
	}
	if got := d.Waiting(); len(got) != 0 {
		t.Errorf("закреплённое задание попало в общую очередь: %v", got)
	}
}

// Отзыв устройства — единственное, что возвращает закреплённое задание в
// общую очередь: ждать эту машину больше нечего.
func TestRevokeReturnsPinnedJobs(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.RemoveExecutor(1)

	freed := d.RevokeDevice(1)
	if len(freed) != 1 || freed[0] != 10 {
		t.Fatalf("отзыв не вернул задание: %v", freed)
	}
	other := d.Next(2)
	if other == nil || other.Plan.TaskID != 10 || !other.Resume {
		t.Errorf("другая машина не получила задание с возобновлением: %+v", other)
	}
}

// Лиз перестал продлеваться при живом соединении — исполнитель завис. Для
// очереди это то же, что разрыв: задание закреплено и ждёт таймаут этапа.
func TestStaleLeaseDetachesNotRequeues(t *testing.T) {
	d, clock := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	clock.advance(LeaseTTL + time.Second)
	if waiting := d.Sweep(); len(waiting) != 0 {
		t.Errorf("протухший лиз сразу перевёл в ожидание устройства: %v", waiting)
	}
	if st, _, _ := d.State(10); st != JobDetached {
		t.Errorf("состояние после протухшего лиза: %s", st)
	}
	if got := d.Waiting(); len(got) != 0 {
		t.Errorf("задание попало в общую очередь: %v", got)
	}
}

// Свободных мест нет — задание остаётся в очереди, а не откладывается молча.
func TestSlotsLimitOffers(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	d.Submit(11, dispatchPlan(11), false) //nolint:errcheck

	first := d.Next(1)
	if first == nil {
		t.Fatal("первое задание не выдано")
	}
	if err := d.Accept(first.JobID, 1); err != nil {
		t.Fatal(err)
	}
	if second := d.Next(1); second != nil {
		t.Error("выдано задание сверх числа мест")
	}
	if got := d.Waiting(); len(got) != 1 || got[0] != 11 {
		t.Errorf("вторая таска не ждёт в очереди: %v", got)
	}
	d.Finish(first.JobID, 1)
	if next := d.Next(1); next == nil || next.Plan.TaskID != 11 {
		t.Error("после освобождения места следующая таска не подхвачена")
	}
}

// Пока исполнителей нет, задание просто ждёт — это обычное состояние.
func TestQueuedWithoutExecutors(t *testing.T) {
	d, _ := newTestDispatcher()
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	if d.Next(1) != nil {
		t.Error("задание выдано неизвестному исполнителю")
	}
	if got := d.Waiting(); len(got) != 1 {
		t.Errorf("таска не числится ожидающей: %v", got)
	}
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	if d.Next(1) == nil {
		t.Error("после подключения исполнителя задание не выдано")
	}
}

// Отмена снимает задание и называет машину, которой нужно об этом сказать.
func TestCancelReportsDevice(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	dev, jobID, ok := d.Cancel(10)
	if !ok || dev != 1 || jobID != offer.JobID {
		t.Fatalf("отмена: %v %d %q", ok, dev, jobID)
	}
	if _, _, ok := d.State(10); ok {
		t.Error("отменённое задание осталось")
	}
	if _, _, ok := d.Cancel(10); ok {
		t.Error("повторная отмена сообщила об успехе")
	}
}

// Невалидный план не попадает в очередь: место, где его ловить, — постановка,
// а не исполнитель, которому его уже отдали.
func TestSubmitValidatesPlan(t *testing.T) {
	d, _ := newTestDispatcher()
	bad := dispatchPlan(10)
	bad.Stages = nil
	if _, err := d.Submit(10, bad, false); err == nil {
		t.Error("невалидный план поставлен в очередь")
	}
	if got := d.Waiting(); len(got) != 0 {
		t.Errorf("после отказа в очереди осталось: %v", got)
	}
}

// Вернувшееся задание встаёт в голову очереди: оно уже ждало, и наказывать
// таску за чужой сбой нельзя.
func TestRequeuedJobKeepsItsPlace(t *testing.T) {
	d, clock := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.Submit(11, dispatchPlan(11), false) //nolint:errcheck

	_ = clock
	d.RevokeDevice(1)
	if got := d.Waiting(); len(got) != 2 || got[0] != 10 {
		t.Errorf("вернувшееся задание не в голове очереди: %v", got)
	}
}

// Нехватка модели или скилла ловится при постановке, а исполнителю без них
// задание не предлагается. Пустые списки в hello ничего не запрещают.
func TestCapabilitiesGateOffers(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1, Models: []string{"claude-fable-5"}, Skills: []string{"analyze-task"}}, nil)
	// Этап без агента (ветка) не требует модели: машина с одной моделью
	// подходит плану, где у ветки модели нет.
	plain := dispatchPlan(12)
	plain.Stages = []protocol.Stage{{Key: "analyze", Skill: "analyze-task", Model: "claude-fable-5", Effort: "high"}, {Key: "branch"}}
	if got := d.MissingFor(1, plain); len(got) != 0 {
		t.Errorf("этап без модели посчитан нехваткой: %v", got)
	}
	d.AddExecutor(2, Capabilities{Slots: 1}, nil) // ничего не объявил

	plan := dispatchPlan(10)
	plan.Stages = []protocol.Stage{{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "high"}}

	missing := d.MissingFor(1, plan)
	if len(missing) != 2 {
		t.Errorf("ожидались две нехватки, получено: %v", missing)
	}
	if got := d.MissingFor(2, plan); len(got) != 0 {
		t.Errorf("исполнитель без объявлений получил нехватки: %v", got)
	}
	if got := d.MissingFor(99, plan); len(got) != 0 {
		t.Errorf("неподключённый исполнитель получил нехватки: %v", got)
	}

	d.Submit(10, plan, false) //nolint:errcheck
	if d.Next(1) != nil {
		t.Error("задание предложено машине без нужной модели и скилла")
	}
	if d.Next(2) == nil {
		t.Error("задание не предложено машине, которая ничего не объявляла")
	}
}

// Подтверждение потерялось в разрыве, а исполнитель уже работает: по hello
// задание считается принятым, а не отменяется — отмена убила бы настоящий
// прогон из-за потерянного пакета.
func TestLostAcceptRecoveredFromHello(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	// accept не дошёл — разрыв
	d.RemoveExecutor(1)
	rc := d.AddExecutor(1, Capabilities{Slots: 1}, []string{offer.JobID})
	if len(rc.Cancel) != 0 {
		t.Errorf("идущее задание велено бросить: %+v", rc)
	}
	if len(rc.Continue) != 1 || rc.Continue[0] != offer.JobID {
		t.Errorf("задание не продолжено: %+v", rc)
	}
	if st, _, _ := d.State(10); st != JobRunning {
		t.Errorf("состояние после сверки: %s", st)
	}
}

// Своя же машина отказалась от закреплённого задания наотрез: ни ей по кругу,
// ни другим — снимается с явной ошибкой, а не крутится вечно.
func TestPinnedRefusalStopsInsteadOfLooping(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.RemoveExecutor(1)
	d.AddExecutor(1, Capabilities{Slots: 1}, nil) // вернулась, задание потеряла
	again := d.Next(1)
	if again == nil || !again.Resume {
		t.Fatal("возобновление не предложено")
	}
	err := d.Reject(again.JobID, 1, false)
	if !errors.Is(err, ErrPinnedRefused) {
		t.Fatalf("ожидался ErrPinnedRefused, получено: %v", err)
	}
	if _, _, ok := d.State(10); ok {
		t.Error("отвергнутое задание осталось")
	}
	for i := 0; i < 3; i++ {
		if d.Next(1) != nil {
			t.Fatal("отвергнутое задание предложено снова")
		}
	}
}

// Из нескольких закреплённых первой возобновляется та, что ждала дольше, и
// порядок не зависит от обхода карты.
func TestPinnedResumeOrderIsStable(t *testing.T) {
	for round := 0; round < 5; round++ {
		d, _ := newTestDispatcher()
		d.AddExecutor(1, Capabilities{Slots: 3}, nil)
		var ids []string
		for _, task := range []int64{30, 10, 20} {
			d.Submit(task, dispatchPlan(task), false) //nolint:errcheck
			o := d.Next(1)
			if err := d.Accept(o.JobID, 1); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, o.JobID)
		}
		d.RemoveExecutor(1)
		d.AddExecutor(1, Capabilities{Slots: 3}, nil)
		if first := d.Next(1); first == nil || first.Plan.TaskID != 10 {
			t.Fatalf("первой возобновлена не самая ранняя: %+v", first)
		}
	}
}

// Пауза сохраняет закрепление: «Возобновить» возвращает таску на ту же
// машину, где лежит рабочая копия, а не в общую очередь.
func TestPauseKeepsPin(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	dev, jobID, ok := d.Pause(10)
	if !ok || dev != 1 || jobID != offer.JobID {
		t.Fatalf("пауза: %v %d %q", ok, dev, jobID)
	}
	if st, _, _ := d.State(10); st != JobPaused {
		t.Errorf("состояние после паузы: %s", st)
	}
	if got := d.Waiting(); len(got) != 0 {
		t.Errorf("приостановленное задание в общей очереди: %v", got)
	}
	// Пока на паузе — ни своей машине, ни чужой.
	if d.Next(1) != nil || d.Next(2) != nil {
		t.Error("приостановленное задание предложено")
	}
	// Повторная постановка той же таски — отказ: она уже есть, её надо возобновить.
	if _, err := d.Submit(10, dispatchPlan(10), true); !errors.Is(err, ErrTaskQueued) {
		t.Errorf("приостановленную таску поставили второй раз: %v", err)
	}
	if !d.Resume(10) {
		t.Fatal("возобновление не удалось")
	}
	if d.Next(2) != nil {
		t.Error("возобновлённое задание ушло на другую машину")
	}
	again := d.Next(1)
	if again == nil || again.JobID != offer.JobID || !again.Resume {
		t.Errorf("возобновление не предложено своей машине: %+v", again)
	}
}

// Вторая сессия той же машины помечается: координатор обязан закрыть первую,
// иначе не названные во втором hello задания предложат заново, пока первая
// сессия их ещё ведёт.
func TestSecondSessionIsFlagged(t *testing.T) {
	d, _ := newTestDispatcher()
	if rc := d.AddExecutor(1, Capabilities{Slots: 1}, nil); rc.Replaced {
		t.Error("первое подключение помечено как повторное")
	}
	if rc := d.AddExecutor(1, Capabilities{Slots: 1}, nil); !rc.Replaced {
		t.Error("повторное подключение не помечено")
	}
	d.RemoveExecutor(1)
	if rc := d.AddExecutor(1, Capabilities{Slots: 1}, nil); rc.Replaced {
		t.Error("подключение после отключения помечено как повторное")
	}
}

// Человек остановил, а до машины «стой» не дошло: по hello ей велят
// остановиться, а не продолжать. Задание остаётся закреплённым на паузе.
func TestPausedJobReportedAsRunningIsCancelled(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.Pause(10)
	d.RemoveExecutor(1)
	rc := d.AddExecutor(1, Capabilities{Slots: 1}, []string{offer.JobID})
	if len(rc.Cancel) != 1 || rc.Cancel[0] != offer.JobID {
		t.Errorf("приостановленное задание не велено остановить: %+v", rc)
	}
	if st, dev, _ := d.State(10); st != JobPaused || dev != 1 {
		t.Errorf("после сверки: %s / %d", st, dev)
	}
}

// Пауза во время повторного предложения своей машине сохраняет закрепление:
// рабочая копия там, удалять задание нельзя.
func TestPauseDuringReofferKeepsPin(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.RemoveExecutor(1)
	d.AddExecutor(1, Capabilities{Slots: 1}, nil) // потеряла задание
	if re := d.Next(1); re == nil || !re.Resume {
		t.Fatal("возобновление не предложено")
	}
	if _, _, ok := d.Pause(10); !ok {
		t.Fatal("пауза не удалась")
	}
	if st, dev, ok := d.State(10); !ok || st != JobPaused || dev != 1 {
		t.Errorf("после паузы: %s / %d / %v", st, dev, ok)
	}
}

// Запоздавший итог со старой машины не снимает задание, уже уехавшее на
// новую под тем же идентификатором.
func TestLateFinishFromOldDeviceIgnored(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	d.Submit(10, dispatchPlan(10), false) //nolint:errcheck
	offer := d.Next(1)
	if err := d.Accept(offer.JobID, 1); err != nil {
		t.Fatal(err)
	}
	d.RevokeDevice(1)
	moved := d.Next(2)
	if moved == nil || moved.JobID != offer.JobID {
		t.Fatalf("задание не уехало на вторую машину: %+v", moved)
	}
	if err := d.Accept(moved.JobID, 2); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(offer.JobID, 1); !errors.Is(err, ErrOtherDevice) {
		t.Errorf("итог со старой машины принят: %v", err)
	}
	if st, dev, ok := d.State(10); !ok || st != JobRunning || dev != 2 {
		t.Errorf("задание на новой машине пострадало: %s / %d / %v", st, dev, ok)
	}
	if err := d.Finish(offer.JobID, 2); err != nil {
		t.Errorf("итог с настоящей машины отвергнут: %v", err)
	}
}

// Пауза обогнала приём: подтверждение к остановленному заданию не делает
// его идущим — оркестратор должен сказать машине «стой».
func TestAcceptAfterPauseIsRefused(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	if _, err := d.Submit(10, dispatchPlan(10), false); err != nil {
		t.Fatal(err)
	}
	offer := d.Next(1)
	if dev, _, ok := d.Pause(10); !ok || dev != 1 {
		t.Fatalf("пауза предложенного: dev=%d ok=%v", dev, ok)
	}
	if err := d.Accept(offer.JobID, 1); !errors.Is(err, ErrPaused) {
		t.Fatalf("ожидался ErrPaused, получено %v", err)
	}
	if state, _, _ := d.State(10); state != JobPaused {
		t.Fatalf("после отказа в приёме задание должно остаться на паузе, а не %q", state)
	}
}

// Машина проекта отказалась наотрез: задание предназначено только ей, и
// в очереди ему делать нечего — снимается с объяснением, а не висит вечно.
func TestTargetRefusalStopsJob(t *testing.T) {
	d, _ := newTestDispatcher()
	d.AddExecutor(1, Capabilities{Slots: 1}, nil)
	d.AddExecutor(2, Capabilities{Slots: 1}, nil)
	if _, err := d.SubmitTo(10, dispatchPlan(10), false, 1); err != nil {
		t.Fatal(err)
	}
	offer := d.Next(1)
	if offer == nil {
		t.Fatal("машине проекта задание не предложено")
	}
	if err := d.Reject(offer.JobID, 1, false); !errors.Is(err, ErrPinnedRefused) {
		t.Fatalf("ожидался ErrPinnedRefused, получено %v", err)
	}
	if _, _, ok := d.State(10); ok {
		t.Fatal("задание должно быть снято")
	}
	if d.Next(2) != nil {
		t.Fatal("чужой машине задание проекта не предлагается")
	}
}
