// Package dispatch — очередь заданий оркестратора: предложение, лиз,
// закрепление за машиной, ожидание. Живёт в модуле тенанта рядом с
// протоколом, потому что описывает его же семантику с другой стороны, и
// тесты исполнителя гоняют его как настоящего собеседника.
package dispatch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Очередь заданий, лизы и закрепление за машиной.
//
// Здесь только состояние: кто чем занят, что кому предложено, у кого истёк
// срок. Ввода-вывода нет намеренно — так поведение очереди проверяется без
// сокетов, горутин и ожиданий, а времязависимые случаи становятся обычными
// табличными тестами.
//
// Принятое задание закреплено за машиной. Рабочая копия и артефакты лежат на
// её диске, и пока связи не было, наверх они не доехали — другая машина
// возобновила бы с прошлого этапа без них. Поэтому разрыв не возвращает
// задание в общую очередь: оно ждёт ту же машину, а по истечении таймаута
// этапа таска честно ждёт устройство. В общую очередь задание уходит только
// при отзыве устройства.

// Сроки жизни задания на разных стадиях.
const (
	// OfferTTL — сколько ждать подтверждения от исполнителя. Короткий: он либо
	// принимает сразу, либо отказывается, и держать за ним задание всё время
	// лиза значило бы задерживать таску из-за молчащей машины.
	OfferTTL = 10 * time.Second
	// LeaseTTL берётся из протокола: правила лиза — часть договорённости
	// сторон, а не внутреннее дело оркестратора.
	LeaseTTL = protocol.LeaseTTL
)

// Ошибки очереди.
var (
	ErrNoSuchJob   = errors.New("задание не найдено")
	ErrOtherDevice = errors.New("задание закреплено за другим устройством")
	ErrNotAssigned = errors.New("задание никому не выдано")
	ErrTaskQueued  = errors.New("таска уже в очереди")
	// ErrPinnedRefused — машина, за которой закреплено задание, отказалась от
	// него наотрез. Предлагать ей по кругу бессмысленно, переотдать некому:
	// рабочая копия у неё. Задание снимается, таске нужна явная причина.
	ErrPinnedRefused = errors.New("машина с рабочей копией не может продолжить задание")
)

// Состояние задания (State возвращает одно из них).
const (
	JobQueued   = "queued"   // в общей очереди, ни за кем не закреплено
	JobOffered  = "offered"  // предложено, подтверждения ещё нет
	JobRunning  = "running"  // принято и исполняется
	JobDetached = "detached" // связь с машиной потеряна, ждём её в пределах таймаута этапа
	JobWaiting  = "waiting"  // машина не вернулась: таска ждёт устройство
	JobPaused   = "paused"   // остановлено человеком; рабочая копия на машине, ждёт «Возобновить»
)

type job struct {
	ID     string
	TaskID int64
	Plan   *protocol.Plan
	Resume bool

	state    string
	deviceID int64
	// target — машина проекта: задание предлагается только ей. Ноль —
	// любой подходящей (так ставятся задания без машины у проекта).
	target  int64
	expires time.Time
	// reoffer — текущее предложение сделано машине, за которой задание уже
	// закреплено (возобновление). Отказ или молчание тогда возвращают задание
	// в ожидание этой же машины, а не в общую очередь: рабочая копия здесь.
	reoffer bool
	// refused — устройства, которые отказались наотрез (не «занят», а «не
	// понимаю план»). Предлагать им это же задание по кругу бессмысленно.
	refused map[int64]bool
}

// pinned сообщает, закреплено ли задание за машиной: у него там рабочая копия.
func (j *job) pinned() bool {
	return j.state == JobRunning || j.state == JobDetached || j.state == JobWaiting || j.state == JobPaused
}

type executor struct {
	deviceID    int64
	slots       int
	projectsDir string
	modelList   []string
	models      map[string]bool
	skills      map[string]bool
}

// Capabilities — что исполнитель объявил в hello.
type Capabilities struct {
	Slots       int
	Models      []string
	Skills      []string
	ProjectsDir string
}

// Dispatcher хранит очередь заданий и следит за лизами.
type Dispatcher struct {
	mu    sync.Mutex
	now   func() time.Time
	jobs  map[string]*job
	tasks map[int64]string // taskID → jobID: одна таска — одно задание
	queue []string         // порядок выдачи общей очереди
	execs map[int64]*executor
}

// NewDispatcher создаёт очередь. now подменяется в тестах: поведение лиза
// иначе пришлось бы проверять реальными паузами.
func NewDispatcher(now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{
		now:   now,
		jobs:  map[string]*job{},
		tasks: map[int64]string{},
		execs: map[int64]*executor{},
	}
}

// NewID — случайный идентификатор задания или запроса.
func NewID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// Идентификатор задания нужен только для сопоставления «предложено —
		// принято»; при отказе источника случайности время уникально не хуже.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf)
}

// Submit ставит таску в общую очередь. Повторная постановка той же таски —
// ошибка, а не второе задание: иначе одна таска поехала бы на двух машинах.
func (d *Dispatcher) Submit(taskID int64, plan *protocol.Plan, resume bool) (string, error) {
	return d.SubmitTo(taskID, plan, resume, 0)
}

// SubmitTo ставит таску в очередь на конкретную машину: проект лежит на ней,
// и никому другому задание не предлагается, даже если машина выключена.
func (d *Dispatcher) SubmitTo(taskID int64, plan *protocol.Plan, resume bool, target int64) (string, error) {
	if err := plan.Validate(); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.tasks[taskID]; ok {
		return "", ErrTaskQueued
	}
	j := &job{ID: NewID(), TaskID: taskID, Plan: plan, Resume: resume, target: target,
		state: JobQueued, refused: map[int64]bool{}}
	d.jobs[j.ID] = j
	d.tasks[taskID] = j.ID
	d.queue = append(d.queue, j.ID)
	return j.ID, nil
}

// Reconcile — итог сверки при подключении исполнителя.
type Reconcile struct {
	// Replaced — эта машина уже была подключена. Координатор обязан закрыть
	// прежнее соединение: два живых сеанса одной машины означали бы, что не
	// названные во втором hello задания предложат заново, пока первый сеанс
	// их ещё ведёт, — двойное исполнение на одной машине.
	Replaced bool
	// Continue — задания, которые исполнитель ведёт и должен продолжать.
	Continue []string
	// Cancel — задания, о которых исполнитель сообщил, но которых за ним нет:
	// отменены или переотданы. Ему нужно их бросить.
	Cancel []string
	// Resume — закреплённые за машиной задания, о которых исполнитель не
	// сообщил (упал и потерял). Их надо предложить заново с `resume`.
	Resume []int64
}

// AddExecutor регистрирует подключившегося исполнителя и сверяет с ним
// задания. running — то, что он сам считает идущим (из hello).
func (d *Dispatcher) AddExecutor(deviceID int64, caps Capabilities, running []string) Reconcile {
	if caps.Slots < 1 {
		caps.Slots = 1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, replaced := d.execs[deviceID]
	ex := &executor{deviceID: deviceID, slots: caps.Slots, projectsDir: caps.ProjectsDir,
		modelList: append([]string(nil), caps.Models...), models: map[string]bool{}, skills: map[string]bool{}}
	for _, m := range caps.Models {
		ex.models[m] = true
	}
	for _, k := range caps.Skills {
		ex.skills[k] = true
	}
	d.execs[deviceID] = ex

	rc := Reconcile{Replaced: replaced}
	reported := map[string]bool{}
	for _, id := range running {
		reported[id] = true
		j, ok := d.jobs[id]
		// Предложенное этой же машине считается принятым: подтверждение могло
		// потеряться в разрыве, а исполнитель уже работает. Отменить — значит
		// убить настоящий прогон из-за потерянного пакета.
		offeredHere := ok && j.state == JobOffered && j.deviceID == deviceID
		if !ok || j.deviceID != deviceID || (!j.pinned() && !offeredHere) {
			rc.Cancel = append(rc.Cancel, id)
			continue
		}
		if j.state == JobPaused {
			// Человек остановил, а до машины это не дошло: продолжать нельзя.
			// Задание остаётся закреплённым и на паузе; машине — «стой».
			rc.Cancel = append(rc.Cancel, id)
			continue
		}
		j.state, j.reoffer, j.expires = JobRunning, false, d.now().Add(LeaseTTL)
		rc.Continue = append(rc.Continue, id)
	}
	// Закреплённое, но не названное — исполнитель его потерял. Оно остаётся за
	// ним (рабочая копия на месте) и уйдёт ему же с recovery. Приостановленное
	// человеком не трогаем: оно ждёт «Возобновить», а не машину.
	for _, j := range d.jobs {
		if j.pinned() && j.state != JobPaused && j.deviceID == deviceID && !reported[j.ID] {
			j.state, j.Resume, j.expires = JobWaiting, true, time.Time{}
			rc.Resume = append(rc.Resume, j.TaskID)
		}
	}
	return rc
}

// RemoveExecutor снимает исполнителя после разрыва. Принятое остаётся
// закреплённым и ждёт ту же машину в пределах таймаута этапа: различить
// «моргнул сокет» и «процесс умер» отсюда нельзя, а возобновить на другой
// машине — нельзя вовсе. Предложенное не трогается: у него свой короткий
// срок, и если машина вернётся до его истечения и назовёт задание в hello —
// подтверждение просто потерялось в разрыве, и работа продолжится. Вернуть
// его в очередь сразу значило бы забыть, кому оно предлагалось.
func (d *Dispatcher) RemoveExecutor(deviceID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.execs, deviceID)
	for _, j := range d.jobs {
		if j.deviceID == deviceID && j.state == JobRunning {
			j.state = JobDetached
			j.expires = d.now().Add(j.Plan.StageTimeout.Duration())
		}
	}
}

// RevokeDevice возвращает все задания машины в общую очередь: устройства
// больше нет, и ждать его бессмысленно. Выполненные этапы сохраняются
// статусами в оркестраторе, а рабочую копию новая машина создаст заново.
func (d *Dispatcher) RevokeDevice(deviceID int64) []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.execs, deviceID)
	var freed []int64
	for _, j := range d.jobs {
		if j.deviceID == deviceID && j.state != JobQueued {
			j.Resume = true
			d.requeueLocked(j)
			freed = append(freed, j.TaskID)
		}
	}
	return freed
}

// Next выдаёт исполнителю следующее подходящее задание. Сначала — его же
// закреплённые, ждущие возобновления; потом общая очередь. Ничего нет —
// возвращает nil: это обычное состояние, а не ошибка.
func (d *Dispatcher) Next(deviceID int64) *protocol.Offer {
	d.mu.Lock()
	defer d.mu.Unlock()
	ex, ok := d.execs[deviceID]
	if !ok || d.busyLocked(deviceID) >= ex.slots {
		return nil
	}
	var pinned []*job
	for _, j := range d.jobs {
		if j.state == JobWaiting && j.deviceID == deviceID && !j.refused[deviceID] {
			pinned = append(pinned, j)
		}
	}
	// Обход карты случаен; та, что ждала дольше, должна идти первой, и
	// поведение должно воспроизводиться от запуска к запуску.
	sort.Slice(pinned, func(a, b int) bool { return pinned[a].TaskID < pinned[b].TaskID })
	for _, j := range pinned {
		j.state, j.reoffer, j.expires = JobOffered, true, d.now().Add(OfferTTL)
		return &protocol.Offer{JobID: j.ID, Plan: j.Plan, Resume: true}
	}
	for i, id := range d.queue {
		j := d.jobs[id]
		if j == nil || j.state != JobQueued || j.refused[deviceID] || len(ex.missing(j.Plan)) > 0 {
			continue
		}
		if j.target != 0 && j.target != deviceID {
			continue
		}
		d.queue = append(d.queue[:i], d.queue[i+1:]...)
		j.state, j.deviceID, j.reoffer = JobOffered, deviceID, false
		j.expires = d.now().Add(OfferTTL)
		return &protocol.Offer{JobID: j.ID, Plan: j.Plan, Resume: j.Resume}
	}
	return nil
}

// Accept подтверждает приём задания. Повторное подтверждение тем же
// исполнителем безвредно: доставка может повториться, и второе исполнение из-за
// этого возникать не должно.
func (d *Dispatcher) Accept(jobID string, deviceID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, err := d.assignedLocked(jobID, deviceID)
	if err != nil {
		return err
	}
	j.state = JobRunning
	j.expires = d.now().Add(LeaseTTL)
	return nil
}

// Renew продлевает лиз.
func (d *Dispatcher) Renew(jobID string, deviceID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, err := d.assignedLocked(jobID, deviceID)
	if err != nil {
		return err
	}
	if j.state != JobRunning {
		return ErrNotAssigned
	}
	j.expires = d.now().Add(LeaseTTL)
	return nil
}

// Reject возвращает предложенное задание в очередь. retryable=false означает,
// что этому исполнителю его предлагать больше не нужно (он не понял план), а
// не что задание отменяется: другая машина может справиться. Закреплённое
// задание отвергнуть нельзя — рабочая копия уже здесь.
func (d *Dispatcher) Reject(jobID string, deviceID int64, retryable bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, err := d.assignedLocked(jobID, deviceID)
	if err != nil {
		return err
	}
	if j.state != JobOffered {
		// Принятое задание отвергнуть нельзя: рабочая копия уже здесь.
		return ErrNotAssigned
	}
	if !retryable {
		j.refused[deviceID] = true
		if j.reoffer {
			// Своя же машина не понимает план: ни ей, ни другим. Снимаем и
			// сообщаем — иначе задание крутилось бы между «ждёт» и
			// «предложено» бесконечно.
			delete(d.tasks, j.TaskID)
			delete(d.jobs, j.ID)
			return ErrPinnedRefused
		}
	}
	d.unofferLocked(j)
	return nil
}

// Finish снимает завершённое задание. Машина проверяется: после отзыва
// устройства то же задание уезжает на другую машину под тем же
// идентификатором, и запоздавший итог со старой снял бы его с новой.
func (d *Dispatcher) Finish(jobID string, deviceID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[jobID]
	if !ok {
		return ErrNoSuchJob
	}
	if j.deviceID != deviceID {
		return ErrOtherDevice
	}
	delete(d.tasks, j.TaskID)
	delete(d.jobs, jobID)
	d.dropFromQueueLocked(jobID)
	return nil
}

// Pause останавливает задание, сохраняя закрепление: рабочая копия остаётся
// на машине, и «Возобновить» вернёт таску туда же. Возвращает машину, которой
// нужно сказать «остановись». Задание в общей очереди просто снимается.
func (d *Dispatcher) Pause(taskID int64) (deviceID int64, jobID string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, has := d.tasks[taskID]
	if !has {
		return 0, "", false
	}
	j := d.jobs[id]
	// Всё, что уже отдано машине, считается закреплённым — и предложенное
	// тоже: подтверждение могло быть в пути, а исполнитель уже работать.
	// Удалить такое значило бы потерять задание, которое идёт. Снимается
	// только то, что лежит в общей очереди и никому не отдано.
	if j.deviceID == 0 {
		delete(d.tasks, taskID)
		delete(d.jobs, id)
		d.dropFromQueueLocked(id)
		return 0, id, true
	}
	j.state, j.reoffer, j.expires = JobPaused, false, time.Time{}
	return j.deviceID, id, true
}

// Park переводит идущее задание в паузу по итогу от исполнителя: таска
// остановилась сама (бюджет) или по нашей отмене, но рабочая копия на месте, и
// «Возобновить» вернёт её туда же. Снимать задание, как завершённое, нельзя.
func (d *Dispatcher) Park(jobID string, deviceID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, err := d.assignedLocked(jobID, deviceID)
	if err != nil {
		return err
	}
	if j.state == JobPaused {
		return nil
	}
	j.state, j.reoffer, j.expires = JobPaused, false, time.Time{}
	return nil
}

// Resume возвращает приостановленное задание в работу на той же машине:
// оно становится ждущим и уйдёт ей при следующем Next с recovery.
func (d *Dispatcher) Resume(taskID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, has := d.tasks[taskID]
	if !has {
		return false
	}
	j := d.jobs[id]
	if j.state != JobPaused {
		return false
	}
	j.state, j.Resume = JobWaiting, true
	return true
}

// Cancel снимает задание таски насовсем (удаление). Возвращает устройство, на
// котором оно шло, — ему нужно сказать «отменено».
func (d *Dispatcher) Cancel(taskID int64) (deviceID int64, jobID string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, has := d.tasks[taskID]
	if !has {
		return 0, "", false
	}
	j := d.jobs[id]
	dev := j.deviceID
	delete(d.tasks, taskID)
	delete(d.jobs, id)
	d.dropFromQueueLocked(id)
	return dev, id, true
}

// Sweep обрабатывает истёкшие сроки: непринятые предложения возвращаются в
// очередь, просроченные лизы переходят в «связь потеряна», а машины, не
// вернувшиеся за таймаут этапа, оставляют таску ждать устройство. Возвращает
// таски, которые с этого момента ждут устройство, — интерфейсу нужна причина.
func (d *Dispatcher) Sweep() (nowWaiting []int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for _, j := range d.jobs {
		if j.expires.IsZero() || now.Before(j.expires) {
			continue
		}
		switch j.state {
		case JobOffered:
			d.unofferLocked(j)
		case JobRunning:
			// Лиз не продлевается при живом соединении — исполнитель завис.
			// Для очереди это то же, что разрыв: ждём его таймаут этапа.
			j.state = JobDetached
			j.expires = now.Add(j.Plan.StageTimeout.Duration())
		case JobDetached:
			j.state, j.expires = JobWaiting, time.Time{}
			nowWaiting = append(nowWaiting, j.TaskID)
		}
	}
	return nowWaiting
}

// MissingFor называет модели и скиллы плана, которых нет у исполнителя. Это
// проверка при постановке: нехватка должна всплыть до того, как таска встала
// в очередь, а не как отказ посреди работы. Неподключённый исполнитель —
// пустой ответ: сравнивать не с чем, и это не повод отклонять таску.
func (d *Dispatcher) MissingFor(deviceID int64, plan *protocol.Plan) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ex, ok := d.execs[deviceID]
	if !ok {
		return nil
	}
	return ex.missing(plan)
}

// missing перечисляет, чего исполнителю не хватает для плана. Пустые списки
// в hello означают «не объявлял» и ничего не запрещают: старый или встроенный
// исполнитель иначе не получил бы ни одного задания.
func (ex *executor) missing(plan *protocol.Plan) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range plan.Stages {
		// Этап без агента модели не несёт — и не требует её от машины.
		if s.Model != "" && len(ex.models) > 0 && !ex.models[s.Model] && !seen["model:"+s.Model] {
			seen["model:"+s.Model] = true
			out = append(out, "модель "+s.Model)
		}
		if s.Skill != "" && len(ex.skills) > 0 && !ex.skills[s.Skill] && !seen["skill:"+s.Skill] {
			seen["skill:"+s.Skill] = true
			out = append(out, "скилл "+s.Skill)
		}
	}
	return out
}

// Unassigned описывает задание, которое сейчас никто не ведёт.
type Unassigned struct {
	// Target — машина, которой оно предназначено (0 — любой).
	Target int64
	// Detached — машина ведёт задание, но связь с ней потеряна: этап, если
	// идёт, завершится на ней, а продолжение ждёт её возвращения.
	Detached bool
	// Missing — чего не хватает на подключённой целевой машине (модель,
	// скилл): задание никогда не уйдёт ей, и молчать об этом нельзя.
	Missing []string
}

// UnassignedJobs — задания, которые никто не ведёт: в очереди, ждущие свою
// машину или оторванные от неё. Координатор по этому списку говорит
// человеку, кого именно ждёт таска и почему.
func (d *Dispatcher) UnassignedJobs() map[int64]Unassigned {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[int64]Unassigned{}
	for _, j := range d.jobs {
		var u Unassigned
		switch j.state {
		case JobQueued:
			u.Target = j.target
		case JobWaiting:
			// Закреплённое и ждущее свою машину: она и есть цель.
			u.Target = j.deviceID
		case JobDetached:
			u.Target, u.Detached = j.deviceID, true
		default:
			continue
		}
		if ex, ok := d.execs[u.Target]; ok && u.Target != 0 {
			u.Missing = ex.missing(j.Plan)
		}
		out[j.TaskID] = u
	}
	return out
}

// Waiting перечисляет таски в общей очереди, никем не подхваченные.
func (d *Dispatcher) Waiting() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []int64{}
	for _, id := range d.queue {
		if j := d.jobs[id]; j != nil && j.state == JobQueued {
			out = append(out, j.TaskID)
		}
	}
	return out
}

// State возвращает состояние задания таски и машину, за которой оно
// закреплено, — для показа причины ожидания.
func (d *Dispatcher) State(taskID int64) (state string, deviceID int64, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, has := d.tasks[taskID]
	if !has {
		return "", 0, false
	}
	j := d.jobs[id]
	return j.state, j.deviceID, true
}

// --- вспомогательное (под замком) ---

func (d *Dispatcher) assignedLocked(jobID string, deviceID int64) (*job, error) {
	j, ok := d.jobs[jobID]
	if !ok {
		return nil, ErrNoSuchJob
	}
	if j.state == JobQueued {
		return nil, ErrNotAssigned
	}
	if j.deviceID != deviceID {
		return nil, ErrOtherDevice
	}
	return j, nil
}

// unofferLocked снимает неудавшееся предложение: закреплённое задание снова
// ждёт свою машину, незакреплённое возвращается в общую очередь.
func (d *Dispatcher) unofferLocked(j *job) {
	if j.reoffer {
		j.state, j.reoffer, j.expires = JobWaiting, false, time.Time{}
		return
	}
	d.requeueLocked(j)
}

func (d *Dispatcher) requeueLocked(j *job) {
	j.state, j.deviceID, j.reoffer, j.expires = JobQueued, 0, false, time.Time{}
	d.dropFromQueueLocked(j.ID)
	// В голову очереди: задание уже ждало, и отправлять его в хвост за только
	// что поставленными значило бы наказывать таску за чужой сбой.
	d.queue = append([]string{j.ID}, d.queue...)
}

func (d *Dispatcher) dropFromQueueLocked(jobID string) {
	for i, id := range d.queue {
		if id == jobID {
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			return
		}
	}
}

// busyLocked считает занятые места: предложенные и идущие задания. Ждущие
// возобновления место не занимают — их ещё не предложили.
func (d *Dispatcher) busyLocked(deviceID int64) int {
	n := 0
	for _, j := range d.jobs {
		if j.deviceID == deviceID && (j.state == JobOffered || j.state == JobRunning) {
			n++
		}
	}
	return n
}

// ExecutorInfo — что известно о подключённом исполнителе: сколько мест
// объявил и сколько занято.
type ExecutorInfo struct {
	Slots       int
	Busy        int
	ProjectsDir string
	Models      []string
}

// Executors перечисляет подключённых исполнителей по устройствам.
func (d *Dispatcher) Executors() map[int64]ExecutorInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[int64]ExecutorInfo, len(d.execs))
	for id, ex := range d.execs {
		out[id] = ExecutorInfo{Slots: ex.slots, Busy: d.busyLocked(id), ProjectsDir: ex.projectsDir, Models: ex.modelList}
	}
	return out
}
