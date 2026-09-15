package executor

import (
	"context"
	"sort"
	"sync"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// journalEvery — раз в сколько подтверждений обновлять LastAck в журнале.
const journalEvery = 100

// Job — одно задание в работе. Runner получает его и через него говорит с
// оркестратором: события, вопросы, ожидание ответов.
type Job struct {
	ID     string
	Plan   *protocol.Plan
	Resume bool
	// Skip — этапы, которые оркестратор считает выполненными: при
	// возобновлении их не переисполняют.
	Skip []string

	ex *Executor

	mu sync.Mutex
	// seq — номер последнего выданного события; pending — ещё не
	// подтверждённые оркестратором, в порядке номеров. После переподключения
	// уходят все pending: оркестратор отбросит те, что уже видел.
	seq     int64
	lastAck int64
	// sent — номер последнего события, успешно записанного в соединение.
	// При переподключении откатывается к lastAck: всё новее уходит заново.
	sent    int64
	pending []*protocol.Event
	// acksSinceWrite — сколько подтверждений прошло с последней записи журнала.
	acksSinceWrite int
	// finished и status — итог, который нужно донести; done тоже может не
	// дойти при разрыве и тогда повторяется после переподключения.
	finished bool
	status   string
	reason   string
	// orphan — задание из журнала после перезапуска: оно не идёт, а ждёт,
	// когда оркестратор вернёт его с resume.
	orphan bool
	// gone — задание снято (итог доставлен или отменено сверкой). Запоздавшее
	// подтверждение не должно после этого воскрешать запись в журнале.
	gone bool

	// sendMu выстраивает отправки одного задания в очередь. Досылка после
	// переподключения и новые события идут из разных горутин; без этого
	// событие N+1 могло уйти раньше N, а оркестратор отбрасывает всё не новее
	// последнего виденного — и N пропало бы.
	sendMu sync.Mutex

	cancel  context.CancelFunc
	cancelR string
	// ended закрывается, когда прогон задания завершился: новое задание той
	// же таски ждёт этого, прежде чем взять её память.
	ended chan struct{}

	answers  chan *protocol.Answer
	messages chan *protocol.Message
	cont     chan protocol.Continue

	// State — состояние таски; заполняет Runner, хранится в журнале.
	State *TaskState
}

// Emit отправляет событие хода работы. Формат — тот же, что интерфейс получает
// сегодня: этап, тип, полезная нагрузка. Номер присваивается здесь.
func (j *Job) Emit(stage, typ string, payload map[string]any) {
	j.mu.Lock()
	j.seq++
	ev := &protocol.Event{Seq: j.seq, TaskID: j.Plan.TaskID, Stage: stage, Type: typ, Payload: payload}
	j.pending = append(j.pending, ev)
	j.mu.Unlock()
	j.ex.taskLog(j, ev)
	j.flush()
}

// flush — единственный путь отправки. Шлёт из pending всё, что новее sent,
// строго по номерам; кто бы ни взял замок первым — новое событие или досылка
// после переподключения, — уйдёт сначала более раннее. Так событие N+1 не
// обгонит N, а оркестратор, отбрасывающий всё не новее последнего виденного,
// ничего не потеряет. Если задание завершено, после событий уходит итог.
func (j *Job) flush() {
	j.sendMu.Lock()
	defer j.sendMu.Unlock()
	for {
		j.mu.Lock()
		var next *protocol.Event
		// pending упорядочен по номерам: первый неотправленный ищется двоичным
		// поиском, иначе досылка большого буфера была бы квадратичной.
		i := sort.Search(len(j.pending), func(k int) bool { return j.pending[k].Seq > j.sent })
		if i < len(j.pending) {
			next = j.pending[i]
		}
		j.mu.Unlock()
		if next == nil {
			break
		}
		if err := j.ex.send(protocol.MsgEvent, j.ID, next); err != nil {
			return
		}
		j.mu.Lock()
		j.sent = next.Seq
		j.mu.Unlock()
	}
	j.mu.Lock()
	finished, status, reason, gone := j.finished, j.status, j.reason, j.gone
	j.mu.Unlock()
	// Снятое сверкой задание итога не шлёт: оркестратор его уже не ждёт, а
	// под тем же идентификатором оно может идти на другой машине.
	if finished && !gone {
		if err := j.ex.send(protocol.MsgDone, j.ID, protocol.Done{JobID: j.ID, Status: status, Reason: reason}); err == nil {
			if j.keepsState() {
				j.ex.park(j.ID)
			} else {
				j.ex.forget(j.ID)
			}
		}
	}
}

// keepsState — после итога память таски остаётся: рабочая копия, сессии
// агента и статусы этапов лежат на этой машине, и любое продолжение —
// «Возобновить» после паузы или ошибки, правка к готовой таске — должно
// идти с того же места, а не заводить worktree поверх существующего.
// Стирается память только удалением таски.
func (j *Job) keepsState() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelR != CancelDelete
}

// rewind откатывает отправленное к подтверждённому: связь восстановлена, и
// всё, что оркестратор не подтвердил, уходит заново. Зовётся до того, как
// соединение станет доступно отправке, — иначе новое событие успело бы уйти
// раньше досылки.
func (j *Job) rewind() {
	j.mu.Lock()
	j.sent = j.lastAck
	j.mu.Unlock()
}

// acked отбрасывает подтверждённые события и запоминает границу в журнале.
func (j *Job) acked(seq int64) {
	j.mu.Lock()
	if seq > j.lastAck {
		j.lastAck = seq
	}
	i := 0
	for i < len(j.pending) && j.pending[i].Seq <= seq {
		i++
	}
	j.pending = j.pending[i:]
	// Запись в журнал под тем же замком, что и проверка gone: иначе между
	// проверкой и записью задание успевало сняться, и файл воскресал.
	// И не на каждое подтверждение: LastAck в журнале — нижняя граница, и
	// отставший на сотню событий он лишь заставит дослать лишнее после
	// перезапуска. Писать файл на каждое событие таски — тысячи записей зря.
	j.acksSinceWrite++
	if !j.gone && (j.acksSinceWrite >= journalEvery || len(j.pending) == 0) {
		j.acksSinceWrite = 0
		rec := &Record{JobID: j.ID, TaskID: j.Plan.TaskID, Plan: j.Plan, Resume: j.Resume, LastAck: j.lastAck, State: j.State}
		if err := j.ex.journal.Put(rec); err != nil {
			j.ex.logf("журнал: %v", err)
		}
	}
	j.mu.Unlock()
}

// retire снимает задание: помечает и стирает запись журнала под одним замком,
// чтобы запоздавшее подтверждение не записало её заново.
func (j *Job) retire() {
	j.mu.Lock()
	j.gone = true
	_ = j.ex.journal.Remove(j.ID)
	j.mu.Unlock()
}

// finish фиксирует итог. Само сообщение done уходит из flush: там же, где
// досылка, чтобы итог не обогнал события.
func (j *Job) finish(status, reason string) {
	j.mu.Lock()
	j.finished, j.status, j.reason = true, status, reason
	j.mu.Unlock()
}

func (j *Job) isFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished
}

// stop прерывает задание с причиной. Runner увидит отменённый контекст, а
// итог станет paused с этой причиной.
func (j *Job) stop(reason string) {
	j.mu.Lock()
	if j.cancelR == "" {
		j.cancelR = reason
	}
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (j *Job) cancelReason() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelR
}

func (j *Job) deliverAnswer(a *protocol.Answer) {
	select {
	case j.answers <- a:
	default:
		j.ex.logf("задание %s: очередь ответов переполнена, ответ %d отброшен", j.ID, a.QuestionID)
	}
}

func (j *Job) deliverMessage(m *protocol.Message) {
	select {
	case j.messages <- m:
	default:
		j.ex.logf("задание %s: очередь сообщений переполнена", j.ID)
	}
}

func (j *Job) deliverContinue(budgetAck bool) {
	select {
	case j.cont <- protocol.Continue{JobID: j.ID, BudgetAck: budgetAck}:
	default: // уже лежит одно — второе ничего не добавит
	}
}

// Answers — ответы человека на вопросы агента.
func (j *Job) Answers() <-chan *protocol.Answer { return j.answers }

// Messages — сообщения человека в чат посреди таски.
func (j *Job) Messages() <-chan *protocol.Message { return j.messages }

// Continue срабатывает на «Возобновить»: между этапами в режиме per_stage
// или после остановки по бюджету.
func (j *Job) Continue() <-chan protocol.Continue { return j.cont }

// SaveState записывает состояние таски в журнал.
func (j *Job) SaveState() {
	j.mu.Lock()
	if j.gone {
		j.mu.Unlock()
		return
	}
	rec := &Record{JobID: j.ID, TaskID: j.Plan.TaskID, Plan: j.Plan, Resume: j.Resume, LastAck: j.lastAck, State: j.State}
	if err := j.ex.journal.Put(rec); err != nil {
		j.ex.logf("журнал: %v", err)
	}
	j.mu.Unlock()
}

// WaitConnected ждёт связь с оркестратором. Runner зовёт его на границе
// этапа: этап доделывается без связи, а следующий не начинается, пока
// оркестратор не подтвердил, что задание всё ещё за нами.
func (j *Job) WaitConnected(ctx context.Context) error {
	for {
		j.ex.mu.Lock()
		conn, ch := j.ex.conn, j.ex.connected
		j.ex.mu.Unlock()
		if conn != nil {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
