package executor

import "github.com/realkasparov/orchestra-tennant/protocol"

// Состояние задания живёт у исполнителя, а оркестратор видит его зеркало из
// событий. Раньше всё это лежало в базе оркестратора и читалось прямо оттуда;
// исполнитель на другой машине к базе доступа не имеет, поэтому память о
// том, какой этап на каком прогоне и какая у него сессия агента, — его
// собственная и хранится в журнале вместе с заданием.

// StageState — этап конкретного раунда.
type StageState struct {
	Key   string `json:"key"`
	Round int    `json:"round"`
	// Status: pending | running | waiting_user | paused | done | error | skipped.
	Status string `json:"status"`
	// SessionID — сессия агента для возобновления прерванного прогона.
	SessionID   string         `json:"session_id,omitempty"`
	CurrentPass int            `json:"current_pass,omitempty"`
	Usage       protocol.Usage `json:"usage"`
}

// QuestionState — вопрос агента и его судьба.
type QuestionState struct {
	// ID — номер внутри задания. Оркестратор хранит вопрос под своим
	// идентификатором и при ответе присылает этот.
	ID          int64    `json:"id"`
	Stage       string   `json:"stage"`
	Type        string   `json:"type"`
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	AllowCustom bool     `json:"allow_custom,omitempty"`
	Answer      string   `json:"answer,omitempty"`
	// Status: open | answered | delivered (ответ уже передан агенту).
	Status string `json:"status"`
}

// TaskState — всё, что исполнитель знает о таске помимо плана.
type TaskState struct {
	TaskDir     string `json:"task_dir"`
	WorktreeDir string `json:"worktree_dir,omitempty"`
	BranchName  string `json:"branch_name,omitempty"`
	BranchSlug  string `json:"branch_slug,omitempty"`
	BaseCommit  string `json:"base_commit,omitempty"`
	RoundBase   string `json:"round_base,omitempty"`
	Reference   string `json:"reference,omitempty"`
	Title       string `json:"title,omitempty"`
	BudgetAck   bool   `json:"budget_ack,omitempty"`

	// Stages — указатели намеренно: этап держат в руках всё время его
	// прогона, а раунды добавляются и посреди него (ответ на вопрос в чате),
	// и срез, переехавший при добавлении, оставил бы в руках копию.
	Stages       []*StageState   `json:"stages"`
	Questions    []QuestionState `json:"questions,omitempty"`
	NextQuestion int64           `json:"next_question"`
}

// stage возвращает этап последнего раунда по ключу: список упорядочен по
// раундам, и последнее совпадение — актуальное. Прошлые раунды — история, их
// не перезапускают и не переименовывают.
func (s *TaskState) stage(key string) *StageState {
	var found *StageState
	for _, st := range s.Stages {
		if st.Key == key {
			found = st
		}
	}
	return found
}

// round — номер последнего раунда (1 у свежей таски).
func (s *TaskState) round() int {
	r := 1
	for _, st := range s.Stages {
		if st.Round > r {
			r = st.Round
		}
	}
	return r
}

// addRound заводит этапы нового раунда.
func (s *TaskState) addRound(keys []string) int {
	r := 0
	if len(s.Stages) > 0 {
		r = s.round()
	}
	r++
	for _, k := range keys {
		s.Stages = append(s.Stages, &StageState{Key: k, Round: r, Status: "pending"})
	}
	return r
}

// cost — суммарный расход по всем этапам: единственный источник правды о
// стоимости таски, из него же считается бюджет.
func (s *TaskState) cost() float64 {
	total := 0.0
	for _, st := range s.Stages {
		total += st.Usage.CostUSD
	}
	return total
}

// openQuestions — вопросы без ответа.
func (s *TaskState) openQuestions() []*QuestionState {
	var out []*QuestionState
	for i := range s.Questions {
		if s.Questions[i].Status == "open" {
			out = append(out, &s.Questions[i])
		}
	}
	return out
}

func (s *TaskState) question(id int64) *QuestionState {
	for i := range s.Questions {
		if s.Questions[i].ID == id {
			return &s.Questions[i]
		}
	}
	return nil
}

func (s *TaskState) addQuestion(q QuestionState) *QuestionState {
	s.NextQuestion++
	q.ID, q.Status = s.NextQuestion, "open"
	s.Questions = append(s.Questions, q)
	return &s.Questions[len(s.Questions)-1]
}
