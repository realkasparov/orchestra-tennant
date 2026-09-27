// Package protocol описывает то, чем оркестратор и исполнитель обмениваются:
// план таски, задание с лизом и события хода работы.
//
// Главное свойство плана — это спецификация, а не готовый промпт. Промпт
// этапа — установленный на машине исполнителя скилл; план говорит, какой
// скилл, с какой моделью и с какими параметрами вызвать. Исполнитель ничего не
// сочиняет — он подставляет в скилл параметры из плана и то, что иначе как на
// месте не посчитать: карту репозитория и дифф. Обратное потребовало бы
// сначала привезти весь контекст в оркестратор — то есть перенести туда код
// проекта, чего исполнитель как раз и существует, чтобы не делать.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion — версия схемы плана, которую умеет эта сборка.
//
// Версия едет в каждом плане с первого дня намеренно: демон и оркестратор
// обновляются порознь, и без неё расхождение обнаруживалось бы как непонятное
// поведение в середине таски, а не отказом при подключении.
const SchemaVersion = 2

// MinSchemaVersion — самая старая схема, которую эта сборка ещё понимает.
const MinSchemaVersion = 1

// Plan — задание на выполнение таски целиком.
type Plan struct {
	SchemaVersion int `json:"schema_version"`

	TaskID    int64  `json:"task_id"`
	Reference string `json:"reference,omitempty"`
	Title     string `json:"title"`
	// Prompt — условие задачи от человека. Это не промпт для модели: он
	// подставляется в скилл этапа как параметр.
	Prompt    string `json:"prompt"`
	SourceURL string `json:"source_url,omitempty"`

	Project Project `json:"project"`
	// Stages — этапы плана схемы 1. У схемы 2 их нет: есть Steps.
	Stages []Stage `json:"stages,omitempty"`
	// Steps — шаги пайплайна (схема 2): у агентных шагов скилл с хэшем,
	// точная модель, привязки входов и реакции.
	Steps []Step `json:"steps,omitempty"`
	// Skills — скиллы шагов: исполнитель докачивает недостающие по хэшу.
	Skills []SkillRef `json:"skills,omitempty"`
	// Rework — шаг, с которого начинается раунд правки.
	Rework string `json:"rework,omitempty"`
	// QA — системный шаг ответа на вопрос к готовой таске.
	QA *Step `json:"qa,omitempty"`
	// TaskDir — папка задачи на машине исполнителя, если она уже есть. Подсказка
	// для локального режима: таски, заведённые до появления исполнителя, хранят
	// артефакты в папке, которую выбрал оркестратор. Пусто — исполнитель
	// выбирает сам.
	TaskDir string `json:"task_dir,omitempty"`

	// Continuity — держать ли одну сессию агента на всю таску (`non_stop`) или
	// начинать заново на каждом этапе (`per_stage`).
	Continuity string `json:"continuity"`
	// Workspace — где работать: отдельный worktree или прямо в папке проекта.
	Workspace string `json:"workspace"`

	BudgetUSD    float64 `json:"budget_usd,omitempty"`
	BudgetAck    bool    `json:"budget_ack,omitempty"`
	StageTimeout Seconds `json:"stage_timeout"`

	// Message — сообщение человека, с которым задание запускается: правка или
	// вопрос к уже готовой таске. Пока таска идёт, сообщения приходят
	// отдельным типом; когда задания нет, оно едет вместе с планом.
	Message *Message `json:"message,omitempty"`
}

// Project — то, над чем работает исполнитель.
type Project struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Path — путь к репозиторию на машине исполнителя.
	Path         string `json:"path"`
	BaseBranch   string `json:"base_branch"`
	BaseCommit   string `json:"base_commit,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Stack        string `json:"stack,omitempty"`
	Description  string `json:"description,omitempty"`
	TestCmd      string `json:"test_cmd,omitempty"`
	IndexMode    string `json:"index_mode,omitempty"`
	IndexExclude string `json:"index_exclude,omitempty"`
	PostCreate   string `json:"post_create_hook,omitempty"`
}

// Stage — один этап конвейера.
type Stage struct {
	Key string `json:"key"`
	// Skill — установленный на машине исполнителя скилл, который и есть промпт
	// этапа. Назван явно, а не подразумевается ключом: конструктор пайплайнов
	// позже ляжет на протокол без переделки, а исполнитель проверит наличие.
	// Пусто у этапов без агента (создание ветки) и у виртуальных.
	Skill string `json:"skill,omitempty"`
	// Model — точный идентификатор сборки (`claude-opus-5`), а не короткий
	// ключ: план должен быть воспроизводим, и «opus» на двух машинах разных
	// версий — две разные модели. Пусто у этапов без агента.
	Model string `json:"model,omitempty"`
	// Effort — режим усилий агента (low | medium | high | max). Пусто у
	// этапов без агента.
	Effort string `json:"effort,omitempty"`
	// Passes — сколько прогонов делать (для err_work — число дельта-проходов);
	// 0 означает «один прогон».
	Passes int `json:"passes,omitempty"`
	// Virtual — этап не запускает агента сам, им управляет соседний этап.
	Virtual bool `json:"virtual,omitempty"`
}

// Seconds — длительность в секундах: по проводу идёт число, а не строка Go,
// потому что читать этот план будет не только Go.
type Seconds int

// Duration возвращает длительность в виде time.Duration.
func (s Seconds) Duration() time.Duration { return time.Duration(s) * time.Second }

// ErrSchema — план непонятной версии. Отдельный тип, потому что реакция на него
// особая: отказаться целиком, а не пытаться исполнить понятную часть.
type ErrSchema struct {
	Got, Min, Max int
}

func (e *ErrSchema) Error() string {
	if e.Got > e.Max {
		return fmt.Sprintf("план версии %d новее поддерживаемой (%d) — обновите исполнителя", e.Got, e.Max)
	}
	return fmt.Sprintf("план версии %d старее поддерживаемой (%d) — обновите оркестратор", e.Got, e.Min)
}

// знакомые значения перечислимых полей
var (
	knownEfforts    = map[string]bool{"": true, "low": true, "medium": true, "high": true, "max": true}
	knownContinuity = map[string]bool{"non_stop": true, "per_stage": true}
	knownWorkspace  = map[string]bool{"worktree": true, "folder": true}
)

// Validate проверяет план целиком. Ошибка версии возвращается первой и как
// *ErrSchema: остальные поля незнакомой схемы проверять бессмысленно — их
// смысл мог измениться.
func (p *Plan) Validate() error {
	if p.SchemaVersion < MinSchemaVersion || p.SchemaVersion > SchemaVersion {
		return &ErrSchema{Got: p.SchemaVersion, Min: MinSchemaVersion, Max: SchemaVersion}
	}
	if p.TaskID <= 0 {
		return fmt.Errorf("в плане нет идентификатора таски")
	}
	if strings.TrimSpace(p.Project.Path) == "" {
		return fmt.Errorf("в плане нет пути к проекту")
	}
	if strings.TrimSpace(p.Project.BaseBranch) == "" {
		return fmt.Errorf("в плане нет базовой ветки")
	}
	if p.SchemaVersion >= 2 {
		if err := p.validateSteps(); err != nil {
			return err
		}
	} else if err := p.validateStages(); err != nil {
		return err
	}
	if !knownContinuity[p.Continuity] {
		return fmt.Errorf("неизвестный режим непрерывности %q", p.Continuity)
	}
	if !knownWorkspace[p.Workspace] {
		return fmt.Errorf("неизвестный режим рабочей копии %q", p.Workspace)
	}
	if p.BudgetUSD < 0 {
		return fmt.Errorf("отрицательный бюджет")
	}
	if p.StageTimeout <= 0 {
		return fmt.Errorf("не задан таймаут этапа")
	}
	return nil
}

// validateSteps — проверка плана схемы 2: структура пайплайна плюс то, чего
// в схеме пайплайна ещё нет, — хэш скилла и точная модель у агентных шагов.
func (p *Plan) validateSteps() error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("в плане нет ни одного шага")
	}
	pipe := &Pipeline{Schema: PipelineSchema, Name: "plan", Rework: p.Rework, Steps: p.Steps}
	if err := pipe.Validate(nil); err != nil {
		return err
	}
	hashes := map[string]bool{}
	for _, sk := range p.Skills {
		if sk.Name == "" || sk.Hash == "" {
			return fmt.Errorf("скилл без имени или хэша")
		}
		hashes[sk.Hash] = true
	}
	for _, s := range p.Steps {
		if s.Disabled {
			continue // выключенный шаг не запускается — модель ему не нужна
		}
		if s.Kind == KindAgent || (s.Kind == KindStartTask && s.Skill != "" && strings.TrimSpace(p.SourceURL) != "") {
			if s.SkillHash == "" || !hashes[s.SkillHash] {
				return fmt.Errorf("шаг %q: скилл %s без хэша в списке скиллов плана", s.Key, s.Skill)
			}
			if strings.TrimSpace(s.Model) == "" {
				return fmt.Errorf("шаг %q: не указана модель", s.Key)
			}
		}
	}
	if p.QA != nil {
		if p.QA.Skill == "" || p.QA.SkillHash == "" || !hashes[p.QA.SkillHash] || p.QA.Model == "" {
			return fmt.Errorf("шаг ответа на вопрос без скилла или модели")
		}
	}
	return nil
}

// validateStages — проверка плана схемы 1.
func (p *Plan) validateStages() error {
	if len(p.Stages) == 0 {
		return fmt.Errorf("в плане нет ни одного этапа")
	}
	seen := map[string]bool{}
	for i, s := range p.Stages {
		if s.Key == "" {
			return fmt.Errorf("этап %d без ключа", i)
		}
		if seen[s.Key] {
			return fmt.Errorf("этап %q встречается дважды", s.Key)
		}
		seen[s.Key] = true
		if s.Skill != "" {
			// Этап с агентом: модель обязательна, режим усилий — из известных.
			if strings.TrimSpace(s.Model) == "" {
				return fmt.Errorf("этап %q: не указана модель", s.Key)
			}
			if !knownEfforts[s.Effort] {
				return fmt.Errorf("этап %q: неизвестный режим усилий %q", s.Key, s.Effort)
			}
		} else if s.Model != "" || s.Effort != "" {
			return fmt.Errorf("этап %q без скилла не запускает агента, модель ему не нужна", s.Key)
		}
		if s.Passes < 0 {
			return fmt.Errorf("этап %q: отрицательное число прогонов", s.Key)
		}
	}
	return nil
}

// Step возвращает шаг плана схемы 2 по ключу.
func (p *Plan) Step(key string) *Step {
	for i := range p.Steps {
		if p.Steps[i].Key == key {
			return &p.Steps[i]
		}
	}
	return nil
}

// Upgrade переводит план схемы 1 в схему 2 по базовому пайплайну: этапы
// прежнего плана становятся шагами с их моделями и прогонами, отсутствующие
// — выключенными. hashes — хэши встроенных скиллов по имени на этой машине.
// План схемы 2 возвращается как есть.
func (p *Plan) Upgrade(hashes map[string]string) error {
	if p.SchemaVersion >= 2 {
		return nil
	}
	base := BasePipeline()
	decompose := p.Stage("decompose") != nil
	var steps []Step
	skills := map[string]bool{}
	var model, effort string
	for _, s := range base.Steps {
		st := s
		old := p.Stage(s.Key)
		switch s.Kind {
		case KindStartTask, KindFinish:
		case KindTestGate:
			st.Disabled = p.Stage("execute") == nil
		default:
			if old == nil {
				st.Disabled = true
			} else {
				st.Model, st.Effort, st.Passes = old.Model, old.Effort, old.Passes
				if st.Passes == 0 {
					st.Passes = s.Passes
				}
				if s.Kind == KindAgent && model == "" {
					model, effort = old.Model, old.Effort
				}
			}
		}
		if s.Key == "analyze" {
			st.Bind = copyMap(s.Bind)
			if decompose {
				st.Bind["DECOMPOSE"] = "yes"
			} else {
				st.Bind["DECOMPOSE"] = "no"
			}
		}
		if st.Skill != "" {
			h, ok := hashes[st.Skill]
			if !ok {
				return fmt.Errorf("план схемы 1: нет встроенного скилла %s для перевода", st.Skill)
			}
			st.SkillHash = h
			skills[st.Skill] = true
		}
		steps = append(steps, st)
	}
	if model == "" {
		model = ModelID("fable")
	}
	p.Steps, p.Stages, p.Rework = steps, nil, base.Rework
	if h, ok := hashes["answer-question"]; ok {
		p.QA = AnswerStep("answer-question", model, effort)
		p.QA.SkillHash = h
		skills["answer-question"] = true
	}
	p.Skills = nil
	for name := range skills {
		p.Skills = append(p.Skills, SkillRef{Name: name, Hash: hashes[name]})
	}
	p.SchemaVersion = 2
	return nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Stage возвращает описание этапа по ключу.
func (p *Plan) Stage(key string) *Stage {
	for i := range p.Stages {
		if p.Stages[i].Key == key {
			return &p.Stages[i]
		}
	}
	return nil
}

// Decode разбирает план и сразу проверяет его: план, который не прошёл
// проверку, не должен существовать в виде значения, которое можно случайно
// начать исполнять.
func Decode(raw []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("план не разобрать: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Encode сериализует план, проверив его перед отправкой.
func (p *Plan) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
