package protocol

import (
	"fmt"
	"regexp"
	"strings"
)

// Пайплайн — данные, а не код: упорядоченный список шагов, у каждого род из
// фиксированного набора и параметры рода. Шаг ссылается на данные предыдущих
// через `$steps.<ключ>.<выход>`, на таску и проект — `$task.*`, `$project.*`.
// Ветвлений в схеме нет: условия выражаются реакциями `on` у шага, переход
// назад — только раунд правки с шага `rework`.

// PipelineSchema — версия схемы пайплайна; совпадает с версией плана, в
// котором пайплайн едет исполнителю.
const PipelineSchema = 2

// Роды шагов. Набор зашит в код обеих сторон: пайплайны собираются из них,
// новый род добавляется кодом.
const (
	KindStartTask = "start.task"
	KindAgent     = "agent"
	KindBranch    = "action.branch"
	KindTestGate  = "action.test_gate"
	KindFinish    = "finish"
)

var knownKinds = map[string]bool{
	KindStartTask: true, KindAgent: true, KindBranch: true, KindTestGate: true, KindFinish: true,
}

// KnownKind сообщает, известен ли род шага этой сборке.
func KnownKind(kind string) bool { return knownKinds[kind] }

// Pipeline — схема пайплайна.
type Pipeline struct {
	Schema   int              `json:"schema"`
	Name     string           `json:"name"`
	Title    string           `json:"title"`
	Defaults PipelineDefaults `json:"defaults"`
	// Rework — ключ шага, с которого начинается раунд правки. Пусто — первый
	// агентный шаг.
	Rework string `json:"rework,omitempty"`
	Steps  []Step `json:"steps"`
}

// PipelineDefaults — параметры прогона по умолчанию; переопределяются у
// проекта и таски.
type PipelineDefaults struct {
	Continuity      string  `json:"continuity,omitempty"` // non_stop | per_stage
	Workspace       string  `json:"workspace,omitempty"`  // worktree | folder
	BudgetUSD       float64 `json:"budget_usd,omitempty"`
	StageTimeoutMin int     `json:"stage_timeout_min,omitempty"`
	// QASkill — скилл ответа на вопрос к готовой таске.
	QASkill string `json:"qa_skill,omitempty"`
}

// Reaction — что сделать, когда маркер принял значение.
type Reaction struct {
	// Skip — шаги текущего раунда, которые пропустить.
	Skip []string `json:"skip,omitempty"`
	// StopPasses — не запускать оставшиеся прогоны шага.
	StopPasses bool `json:"stop_passes,omitempty"`
}

// Step — один шаг пайплайна.
type Step struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`

	// --- agent, start.task (скилл импорта) ---
	Skill string `json:"skill,omitempty"`
	// SkillHash — версия скилла; в схеме пайплайна пусто (последняя), в
	// плане таски обязателен.
	SkillHash string `json:"skill_hash,omitempty"`
	// Model — в схеме пайплайна и переопределениях короткий ключ (opus), в
	// плане — точный идентификатор сборки.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Passes int    `json:"passes,omitempty"`
	// Bind — вход манифеста ← источник (`$task.prompt`, `$steps.start.text`
	// или литерал).
	Bind map[string]string `json:"bind,omitempty"`
	// Emit — маркер → поле таски: reference | title | branch_slug | branch_name.
	Emit map[string]string `json:"emit,omitempty"`
	// On — маркер → значение → реакция.
	On map[string]map[string]Reaction `json:"on,omitempty"`

	// --- action.test_gate ---
	// FixWith — агентный шаг, чьей сессией чинить упавшие тесты.
	FixWith string `json:"fix_with,omitempty"`

	// --- finish ---
	// Result — что показать как результат: branch | report.
	Result string `json:"result,omitempty"`

	// Disabled — шаг выключен переопределением: в плане присутствует со
	// статусом skipped, чтобы история этапов не менялась.
	Disabled bool `json:"disabled,omitempty"`
}

// IsAgent — шаг запускает агента (и нуждается в скилле и модели).
func (s *Step) IsAgent() bool { return s.Kind == KindAgent }

// Runs — шаг отражается строкой этапа: всё, кроме финиша. Стартовый шаг без
// ссылки на импорт строки не получает — решает исполнитель по плану.
func (s *Step) Runs() bool { return s.Kind != KindFinish }

// Step возвращает шаг по ключу.
func (p *Pipeline) Step(key string) *Step {
	for i := range p.Steps {
		if p.Steps[i].Key == key {
			return &p.Steps[i]
		}
	}
	return nil
}

// ReworkKey — шаг, с которого начинается раунд правки: явный или первый
// агентный.
func (p *Pipeline) ReworkKey() string {
	if p.Rework != "" {
		return p.Rework
	}
	for _, s := range p.Steps {
		if s.Kind == KindAgent {
			return s.Key
		}
	}
	return ""
}

// StepKeys — ключи шагов, у которых есть строка этапа (без финиша).
// withStart — включать ли стартовый шаг (у таски со ссылкой на импорт).
func (p *Pipeline) StepKeys(withStart bool) []string {
	var out []string
	for _, s := range p.Steps {
		if s.Kind == KindFinish {
			continue
		}
		if strings.HasPrefix(s.Kind, "start.") && !withStart {
			continue
		}
		out = append(out, s.Key)
	}
	return out
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var refRe = regexp.MustCompile(`^\$(task|project|plan|steps)\.([a-z_]+)(?:\.([a-zA-Z0-9_.-]+))?$`)

// Известные поля таски, проекта и плана, доступные в привязках.
var (
	taskRefs = map[string]bool{"dir": true, "prompt": true, "title": true, "reference": true, "url": true,
		"branch_name": true, "base_commit": true, "round_base": true, "worktree_dir": true,
		"feedback": true, "revision": true, "previous_round": true, "pass": true, "pass_scope": true,
		"branch_title": true, "question": true}
	projectRefs = map[string]bool{"name": true, "path": true, "base_branch": true, "test_cmd": true,
		"stack": true, "description": true}
	planRefs = map[string]bool{"workspace": true, "continuity": true, "review_mode": true, "decompose": true}
)

// Manifests — манифесты по имени скилла; nil означает «проверять только
// структуру» (на стороне, где манифестов ещё нет).
type Manifests map[string]*Manifest

// Validate проверяет схему: роды, ключи, порядок, ссылки назад, привязки
// обязательных входов (если манифесты даны), реакции на существующие шаги.
func (p *Pipeline) Validate(m Manifests) error {
	if p.Schema != PipelineSchema {
		return fmt.Errorf("схема пайплайна %d, поддерживается %d", p.Schema, PipelineSchema)
	}
	if !keyRe.MatchString(p.Name) {
		return fmt.Errorf("имя пайплайна %q: только латиница, цифры и подчёркивание", p.Name)
	}
	if len(p.Steps) < 2 {
		return fmt.Errorf("пайплайну нужны хотя бы стартовый шаг и финиш")
	}
	if !strings.HasPrefix(p.Steps[0].Kind, "start.") {
		return fmt.Errorf("первый шаг должен быть стартовым, а не %q", p.Steps[0].Kind)
	}
	if p.Steps[len(p.Steps)-1].Kind != KindFinish {
		return fmt.Errorf("последний шаг должен быть финишем, а не %q", p.Steps[len(p.Steps)-1].Kind)
	}
	seen := map[string]int{}
	workspace := ""
	for i, s := range p.Steps {
		if !keyRe.MatchString(s.Key) {
			return fmt.Errorf("шаг %d: ключ %q — только латиница, цифры и подчёркивание", i, s.Key)
		}
		if _, dup := seen[s.Key]; dup {
			return fmt.Errorf("шаг %q встречается дважды", s.Key)
		}
		if !knownKinds[s.Kind] {
			return fmt.Errorf("шаг %q: неизвестный род %q", s.Key, s.Kind)
		}
		if i > 0 && strings.HasPrefix(s.Kind, "start.") {
			return fmt.Errorf("шаг %q: стартовый шаг может быть только первым", s.Key)
		}
		if i < len(p.Steps)-1 && s.Kind == KindFinish {
			return fmt.Errorf("шаг %q: финиш может быть только последним", s.Key)
		}
		if strings.TrimSpace(s.Title) == "" {
			return fmt.Errorf("шаг %q: нет заголовка", s.Key)
		}
		if s.Kind == KindAgent && strings.TrimSpace(s.Skill) == "" {
			return fmt.Errorf("шаг %q: агентному шагу нужен скилл", s.Key)
		}
		if s.Kind != KindAgent && s.Kind != KindStartTask && (s.Skill != "" || s.Model != "") {
			return fmt.Errorf("шаг %q: род %s не запускает агента, скилл и модель ему не нужны", s.Key, s.Kind)
		}
		if s.Passes < 0 {
			return fmt.Errorf("шаг %q: отрицательное число прогонов", s.Key)
		}
		if s.Effort != "" && !knownEfforts[s.Effort] {
			return fmt.Errorf("шаг %q: неизвестный режим усилий %q", s.Key, s.Effort)
		}
		for input, src := range s.Bind {
			if err := checkRef(src, seen); err != nil {
				return fmt.Errorf("шаг %q, вход %s: %w", s.Key, input, err)
			}
		}
		for marker, field := range s.Emit {
			if !emitFields[field] {
				return fmt.Errorf("шаг %q: маркер %s нельзя записать в поле %q", s.Key, marker, field)
			}
		}
		for marker, byValue := range s.On {
			for value, re := range byValue {
				for _, k := range re.Skip {
					j, ok := indexOf(p.Steps, k)
					if !ok {
						return fmt.Errorf("шаг %q: реакция %s=%s пропускает несуществующий шаг %q", s.Key, marker, value, k)
					}
					if j <= i {
						return fmt.Errorf("шаг %q: реакция %s=%s может пропускать только последующие шаги, а %q идёт раньше", s.Key, marker, value, k)
					}
				}
				if re.StopPasses && s.Passes == 0 && s.Kind != KindAgent {
					return fmt.Errorf("шаг %q: stop_passes без прогонов", s.Key)
				}
			}
		}
		if s.Kind == KindTestGate && s.FixWith != "" {
			j, ok := indexOf(p.Steps, s.FixWith)
			if !ok || p.Steps[j].Kind != KindAgent {
				return fmt.Errorf("шаг %q: fix_with должен указывать на агентный шаг, а не %q", s.Key, s.FixWith)
			}
			if j >= i {
				return fmt.Errorf("шаг %q: fix_with должен указывать на предыдущий шаг", s.Key)
			}
		}
		if s.Kind == KindFinish && s.Result != "" && s.Result != "branch" && s.Result != "report" {
			return fmt.Errorf("шаг %q: неизвестный вид результата %q", s.Key, s.Result)
		}
		if m != nil && (s.Kind == KindAgent || (s.Kind == KindStartTask && s.Skill != "")) {
			man := m[s.Skill]
			if man == nil {
				return fmt.Errorf("шаг %q: скилл %q не найден в реестре", s.Key, s.Skill)
			}
			if s.Kind == KindAgent {
				for _, name := range man.InputOrder {
					in := man.Inputs[name]
					if in.Required {
						if _, ok := s.Bind[name]; !ok {
							return fmt.Errorf("шаг %q: обязательный вход %s скилла %s не привязан", s.Key, name, s.Skill)
						}
					}
				}
				for name := range s.Bind {
					if _, ok := man.Inputs[name]; !ok {
						return fmt.Errorf("шаг %q: скилл %s не объявляет вход %s", s.Key, s.Skill, name)
					}
				}
				for marker := range s.Emit {
					if _, ok := man.Outputs.Markers[marker]; !ok {
						return fmt.Errorf("шаг %q: скилл %s не объявляет маркер %s", s.Key, s.Skill, marker)
					}
				}
				for marker := range s.On {
					if _, ok := man.Outputs.Markers[marker]; !ok {
						return fmt.Errorf("шаг %q: скилл %s не объявляет маркер %s", s.Key, s.Skill, marker)
					}
				}
				ws := man.Workspace
				if ws == "" {
					ws = WorkspaceManaged
				}
				if workspace != "" && workspace != ws {
					return fmt.Errorf("шаг %q: скилл %s управляет рабочей копией сам, а другой шаг ждёт её от движка — в одном пайплайне так нельзя", s.Key, s.Skill)
				}
				workspace = ws
			}
		}
		seen[s.Key] = i
	}
	if p.Rework != "" {
		if _, ok := seen[p.Rework]; !ok {
			return fmt.Errorf("rework указывает на несуществующий шаг %q", p.Rework)
		}
	}
	if p.Defaults.Continuity != "" && !knownContinuity[p.Defaults.Continuity] {
		return fmt.Errorf("неизвестный режим непрерывности %q", p.Defaults.Continuity)
	}
	if p.Defaults.Workspace != "" && !knownWorkspace[p.Defaults.Workspace] {
		return fmt.Errorf("неизвестный режим рабочей копии %q", p.Defaults.Workspace)
	}
	return nil
}

var emitFields = map[string]bool{"reference": true, "title": true, "branch_slug": true, "branch_name": true}

func indexOf(steps []Step, key string) (int, bool) {
	for i := range steps {
		if steps[i].Key == key {
			return i, true
		}
	}
	return 0, false
}

// checkRef проверяет ссылку привязки: литерал допустим, `$…` — только на
// известные поля и на предыдущие шаги.
func checkRef(src string, before map[string]int) error {
	if !strings.HasPrefix(src, "$") {
		return nil
	}
	m := refRe.FindStringSubmatch(src)
	if m == nil {
		return fmt.Errorf("ссылка %q не разобрана", src)
	}
	switch m[1] {
	case "task":
		if !taskRefs[m[2]] || m[3] != "" {
			return fmt.Errorf("у таски нет поля %q", m[2])
		}
	case "project":
		if !projectRefs[m[2]] || m[3] != "" {
			return fmt.Errorf("у проекта нет поля %q", m[2])
		}
	case "plan":
		if !planRefs[m[2]] || m[3] != "" {
			return fmt.Errorf("у плана нет поля %q", m[2])
		}
	case "steps":
		if _, ok := before[m[2]]; !ok {
			return fmt.Errorf("ссылка на шаг %q, которого нет раньше по списку", m[2])
		}
		if m[3] == "" {
			return fmt.Errorf("ссылка %q без имени выхода", src)
		}
	}
	return nil
}

// ParseRef разбирает ссылку `$scope.name[.output]`; ok=false у литерала.
func ParseRef(src string) (scope, name, output string, ok bool) {
	m := refRe.FindStringSubmatch(src)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
