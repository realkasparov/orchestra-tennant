package protocol

import (
	"encoding/json"
	"time"
)

// Задание живёт по лизу: оркестратор предлагает, исполнитель подтверждает,
// лиз продлевается heartbeat'ом. По истечении лиза задание возвращается в
// очередь.
//
// Поллинг (как у раннеров GitHub Actions и агентов Buildkite) выбран не был:
// у нас уже есть постоянное соединение, и push отдаёт задание сразу. Но у
// поллинга есть свойство, которое терять нельзя, — упавший исполнитель не
// держит задание вечно. Лиз возвращает именно его.
const (
	// LeaseTTL — сколько задание считается принятым без продления.
	LeaseTTL = 90 * time.Second
	// LeaseRenew — как часто исполнитель продлевает лиз. Втрое чаще срока,
	// чтобы одна потерянная посылка не отбирала задание у работающей машины.
	LeaseRenew = 30 * time.Second
)

// Тип сообщения в канале между оркестратором и исполнителем.
const (
	// От оркестратора к исполнителю.
	MsgWelcome  = "welcome"  // ответ на hello: версия схемы, правила лиза, сверка заданий
	MsgOffer    = "offer"    // предложение задания
	MsgCancel   = "cancel"   // отмена (пауза, удаление таски)
	MsgAnswer   = "answer"   // ответ человека на вопрос агента
	MsgMessage  = "message"  // сообщение человека в чат посреди таски
	MsgContinue = "continue" // «Возобновить»: продолжить между этапами или после остановки по бюджету
	MsgAck      = "ack"      // подтверждение приёма событий до номера
	MsgProject  = "project"  // задание на проект: проверить, создать, обновить

	// От исполнителя к оркестратору.
	MsgProjectResult = "project_result" // ответ на задание о проекте

	// От исполнителя к оркестратору.
	MsgHello  = "hello"  // представление при подключении
	MsgAccept = "accept" // задание принято
	MsgReject = "reject" // задание не принято (занят, план не понят)
	MsgRenew  = "renew"  // продление лиза
	MsgEvent  = "event"  // событие хода работы
	MsgDone   = "done"   // задание завершено
)

// Типы событий, которых до протокола не было: раньше оркестратор читал их с
// диска, теперь получает от исполнителя.
const (
	// EventArtifact — файл шага записан: имя и содержимое.
	EventArtifact = "artifact"
	// EventDiff — дифф ветки против базового коммита по завершении этапа.
	EventDiff = "diff"
)

// Envelope — конверт любого сообщения. Тип снаружи, содержимое внутри сырым
// JSON: разбирать содержимое имеет смысл только зная тип, а неизвестный тип
// должен пропускаться целиком, а не ронять разбор.
type Envelope struct {
	Type string `json:"type"`
	// JobID пуст только у hello.
	JobID string          `json:"job_id,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Hello — представление исполнителя при подключении.
type Hello struct {
	DeviceKey string `json:"device_key"`
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	Version   string `json:"agent_version,omitempty"`
	// MinSchema и MaxSchema — диапазон версий плана, который понимает
	// исполнитель. Согласование версии при подключении, а не при первом
	// задании: расхождение должно всплывать, пока человек ещё рядом.
	MinSchema int `json:"min_schema"`
	MaxSchema int `json:"max_schema"`
	// Slots — сколько заданий исполнитель готов вести одновременно.
	Slots int `json:"slots"`
	// ProjectsDir — папка проектов машины, единственный корень, в котором
	// исполнитель заводит и ищет проекты. Оркестратор описывает проекты
	// относительно него.
	ProjectsDir string `json:"projects_dir,omitempty"`
	// Models и Skills — что установлено на машине. План собирается из этого:
	// нехватка ловится при постановке, а не посреди таски.
	Models []string `json:"models"`
	Skills []string `json:"skills"`
	// Running — задания, которые исполнитель ведёт с прошлого соединения.
	// Оркестратор сверяет их с закреплением и отвечает, что продолжать, а что
	// бросить.
	Running []string `json:"running,omitempty"`
	// Parked — задания, память которых исполнитель хранит после паузы или
	// ошибки (рабочая копия, сессии агента), не ведя их. Оркестратор по
	// этому списку велит забыть те, чьи таски удалены, пока связи не было.
	Parked []JobRef `json:"parked,omitempty"`
}

// JobRef — задание и его таска: по одному идентификатору задания после
// перезапуска оркестратора таску уже не найти.
type JobRef struct {
	JobID  string `json:"job_id"`
	TaskID int64  `json:"task_id"`
}

// Welcome — ответ оркестратора на hello.
type Welcome struct {
	DeviceID int64 `json:"device_id"`
	// Schema — версия, в которой оркестратор будет слать планы.
	Schema int `json:"schema"`
	// LeaseTTLSec и RenewSec сообщают исполнителю правила лиза, чтобы они не
	// были зашиты в него константами двух разных сборок.
	LeaseTTLSec int `json:"lease_ttl_sec"`
	RenewSec    int `json:"renew_sec"`
	// Continue и Cancel — итог сверки заданий из hello.Running.
	Continue []string `json:"continue,omitempty"`
	Cancel   []string `json:"cancel,omitempty"`
}

// Answer — ответ человека на вопрос агента.
type Answer struct {
	JobID      string `json:"job_id"`
	QuestionID int64  `json:"question_id"`
	Text       string `json:"text"`
}

// Message — сообщение человека в чат посреди таски. Триаж делает исполнитель:
// ему нужна модель.
type Message struct {
	JobID string `json:"job_id"`
	Text  string `json:"text"`
	Mode  string `json:"mode,omitempty"`
}

// Continue — «Возобновить» от человека. BudgetAck снимает лимит бюджета:
// дальше он не проверяется, иначе пауза повторялась бы после каждого шага.
type Continue struct {
	JobID     string `json:"job_id"`
	BudgetAck bool   `json:"budget_ack,omitempty"`
}

// Cancel — отмена задания с причиной: пауза сохраняет возможность продолжить,
// удаление — нет.
type Cancel struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"` // pause | delete
	// TaskID — для удаления таски, задание которой оркестратор уже не
	// помнит (закончилось ошибкой, а память на машине осталась): без
	// идентификатора задания исполнитель ищет её по таске.
	TaskID int64 `json:"task_id,omitempty"`
}

// Ack — оркестратор принял события задания по номер Seq включительно. После
// переподключения исполнитель досылает всё, что новее.
type Ack struct {
	JobID string `json:"job_id"`
	Seq   int64  `json:"seq"`
}

// Offer — предложение задания.
type Offer struct {
	JobID string `json:"job_id"`
	Plan  *Plan  `json:"plan"`
	// Resume — таска уже шла и продолжается: этапы со статусом done
	// переисполнять не нужно.
	Resume bool `json:"resume,omitempty"`
	// LastSeq — последний номер события этого задания, который оркестратор
	// уже сохранил. Исполнитель после перезапуска продолжает нумерацию с него:
	// начать с единицы значило бы, что новые события отброшены как уже
	// виденные, а его собственная память о подтверждениях могла отстать.
	LastSeq int64 `json:"last_seq,omitempty"`
	// Done — этапы, которые оркестратор уже считает выполненными; при
	// возобновлении исполнитель их пропускает.
	Done []string `json:"done,omitempty"`
}

// Reject — отказ от задания с причиной. Причина обязательна: молчаливый отказ
// оставил бы таску в очереди без объяснения, почему она не едет.
type Reject struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
	// Retryable — стоит ли предлагать это задание снова (занят — да,
	// непонятная схема плана — нет).
	Retryable bool `json:"retryable"`
}

// Event — событие хода работы. Формат совпадает с тем, что уже уходит в
// браузер: переход на протокол не должен менять картину в интерфейсе.
type Event struct {
	// Seq — порядковый номер внутри задания, с единицы. Нужен для досылки
	// после переподключения: оркестратор отбрасывает уже виденные номера,
	// иначе расход этапа задвоился бы.
	Seq     int64          `json:"seq"`
	TaskID  int64          `json:"task_id"`
	Stage   string         `json:"stage,omitempty"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Done — задание завершено; статус повторяет терминальный статус таски.
type Done struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// NegotiateSchema выбирает версию плана, понятную обеим сторонам.
func NegotiateSchema(minPeer, maxPeer int) (int, error) {
	lo, hi := MinSchemaVersion, SchemaVersion
	if minPeer > lo {
		lo = minPeer
	}
	if maxPeer < hi {
		hi = maxPeer
	}
	if lo > hi {
		return 0, &ErrSchema{Got: maxPeer, Min: MinSchemaVersion, Max: SchemaVersion}
	}
	return hi, nil
}

// Usage — расход одного прогона или этапа. Поля повторяют то, что интерфейс
// читает из stage_status: формат события не меняется.
type Usage struct {
	TokIn      int64   `json:"tok_in"`
	TokOut     int64   `json:"tok_out"`
	CacheWrite int64   `json:"cache_write"`
	CacheRead  int64   `json:"cache_read"`
	CostUSD    float64 `json:"cost_usd"`
}

// Zero сообщает, что ничего не учтено.
func (u Usage) Zero() bool { return u == Usage{} }

// Add — поэлементная сумма.
func (u Usage) Add(v Usage) Usage {
	return Usage{TokIn: u.TokIn + v.TokIn, TokOut: u.TokOut + v.TokOut,
		CacheWrite: u.CacheWrite + v.CacheWrite, CacheRead: u.CacheRead + v.CacheRead,
		CostUSD: u.CostUSD + v.CostUSD}
}

// --- проекты ---

// Действия задания на проект.
const (
	ProjectCheck  = "check"  // сухая проверка: чек-лист, диск не трогается
	ProjectCreate = "create" // клон, init или регистрация существующей папки
	ProjectUpdate = "update" // настройки изменились: индекс, хук
	// ProjectView — показать ветку в папке проекта: папка переключается на
	// её коммит в отсоединённом состоянии, сама ветка остаётся в worktree,
	// и таска продолжает работать. Ветка, равная базовой, выкладывается
	// как есть — это «вернуть папку на основную ветку».
	ProjectView = "view"
)

// ProjectSpec — описание проекта, как план таски: что нужно, а не как
// сделать. Оркестратор его собирает, машина исполняет.
type ProjectSpec struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
	// Dir — папка проекта относительно папки проектов машины. Абсолютный
	// путь допустим только для проектов, заведённых до появления машин.
	Dir string `json:"dir"`
	// Repo — откуда клонировать; пусто — папка существует или инициализируется.
	Repo *ProjectRepo `json:"repo,omitempty"`

	BaseBranch string `json:"base_branch"`
	// Branch — для view: какую ветку показать в папке проекта.
	Branch       string `json:"branch,omitempty"`
	PostCreate   string `json:"post_create_hook,omitempty"`
	Description  string `json:"description,omitempty"`
	Stack        string `json:"stack,omitempty"`
	TestCmd      string `json:"test_cmd,omitempty"`
	IndexMode    string `json:"index_mode,omitempty"`
	IndexExclude string `json:"index_exclude,omitempty"`
}

// ProjectRepo — репозиторий для клона. Токен нужен только на время клона и в
// origin не сохраняется; по локальному сокету он уходит той же машине, на
// которой лежат настройки, — контур доверия тот же.
type ProjectRepo struct {
	HostURL  string `json:"host_url"`
	RepoPath string `json:"repo_path"`
	Token    string `json:"token,omitempty"`
}

// ProjectRequest — задание на проект: запрос с ответом, не поток событий.
type ProjectRequest struct {
	ReqID  string      `json:"req_id"`
	Action string      `json:"action"`
	Spec   ProjectSpec `json:"spec"`
}

// ProjectCheckItem — строка чек-листа проверки.
type ProjectCheckItem struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Level  string `json:"level"` // ok | warn | err
}

// ProjectResult — ответ машины на задание о проекте.
type ProjectResult struct {
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Path — абсолютный путь проекта на машине.
	Path string `json:"path,omitempty"`
	// BaseBranch — базовая ветка, которой проект заведён: у клона без явной
	// ветки это ветка по умолчанию репозитория, которую знает только машина.
	BaseBranch  string `json:"base_branch,omitempty"`
	Cloned      bool   `json:"cloned,omitempty"`
	Initialized bool   `json:"initialized,omitempty"`
	// Repo — в папке уже есть git-репозиторий (до задания). Переезд проекта
	// на другую машину разрешён только в такую папку: заводить пустой
	// репозиторий вместо перенесённого кода — не переезд, а потеря.
	Repo bool `json:"repo,omitempty"`
	// Branches — ветки найденного репозитория (локальные и origin): из них
	// человек выбирает базовую, а BaseBranch у проверки — главная ветка
	// репозитория, которая подставится, если он не выбрал сам.
	Branches []string           `json:"branches,omitempty"`
	Checks   []ProjectCheckItem `json:"checks,omitempty"`
	Verdict  string             `json:"verdict,omitempty"` // ok | warn | err
}
