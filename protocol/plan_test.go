package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// validPlan — план схемы 1: этапы, а не шаги. Схема 1 ещё принимается на
// переходный релиз, и её проверки не должны сломаться.
func validPlan() *Plan {
	return &Plan{
		SchemaVersion: 1,
		TaskID:        7,
		Title:         "Игра «Змейка»",
		Prompt:        "сделай змейку",
		Project: Project{
			ID: 1, Name: "demo", Path: "/repo", BaseBranch: "main",
		},
		Stages: []Stage{
			{Key: "analyze", Skill: "analyze-task", Model: "claude-fable-5", Effort: "high"},
			{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "max", Passes: 2},
			{Key: "branch"},
		},
		Continuity:   "per_stage",
		Workspace:    "worktree",
		StageTimeout: Seconds(1800),
	}
}

func TestValidPlanRoundTrips(t *testing.T) {
	p := validPlan()
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.TaskID != p.TaskID || len(back.Stages) != len(p.Stages) {
		t.Fatalf("план изменился при передаче: %+v", back)
	}
	if back.Stage("execute").Passes != 2 {
		t.Error("настройки этапа потерялись")
	}
	if back.StageTimeout.Duration().Minutes() != 30 {
		t.Errorf("таймаут разобран неверно: %v", back.StageTimeout.Duration())
	}
}

// План незнакомой версии отвергается целиком: поля могли поменять смысл, и
// исполнить «понятную часть» значит сделать не то, что просили.
func TestUnknownSchemaRejectedWholesale(t *testing.T) {
	p := validPlan()
	p.SchemaVersion = SchemaVersion + 1
	err := p.Validate()
	var se *ErrSchema
	if !errors.As(err, &se) {
		t.Fatalf("ожидалась ошибка версии, получено: %v", err)
	}
	if !strings.Contains(err.Error(), "обновите исполнителя") {
		t.Errorf("сообщение не подсказывает, что делать: %s", err)
	}

	p.SchemaVersion = MinSchemaVersion - 1
	if err := p.Validate(); !errors.As(err, &se) || !strings.Contains(err.Error(), "обновите оркестратор") {
		t.Errorf("старая версия: %v", err)
	}
}

// Ошибка версии проверяется раньше остальных: у незнакомой схемы разбирать
// прочие поля бессмысленно.
func TestSchemaCheckedBeforeEverythingElse(t *testing.T) {
	p := &Plan{SchemaVersion: 999} // всё остальное пусто и невалидно
	var se *ErrSchema
	if err := p.Validate(); !errors.As(err, &se) {
		t.Fatalf("версия должна проверяться первой, получено: %v", err)
	}
}

func TestPlanValidation(t *testing.T) {
	cases := []struct {
		name string
		fix  func(*Plan)
		want string
	}{
		{"без таски", func(p *Plan) { p.TaskID = 0 }, "идентификатора таски"},
		{"без пути", func(p *Plan) { p.Project.Path = "  " }, "пути к проекту"},
		{"без ветки", func(p *Plan) { p.Project.BaseBranch = "" }, "базовой ветки"},
		{"без этапов", func(p *Plan) { p.Stages = nil }, "ни одного этапа"},
		{"этап без ключа", func(p *Plan) { p.Stages[0].Key = "" }, "без ключа"},
		{"этап дважды", func(p *Plan) { p.Stages[1].Key = p.Stages[0].Key }, "дважды"},
		{"без модели", func(p *Plan) { p.Stages[0].Model = "" }, "не указана модель"},
		{"модель у механики", func(p *Plan) { p.Stages[2].Model = "claude-opus-5" }, "модель ему не нужна"},
		{"чужой режим", func(p *Plan) { p.Stages[0].Effort = "ultra" }, "режим усилий"},
		{"отрицательные прогоны", func(p *Plan) { p.Stages[0].Passes = -1 }, "отрицательное число"},
		{"чужая непрерывность", func(p *Plan) { p.Continuity = "always" }, "непрерывности"},
		{"чужая копия", func(p *Plan) { p.Workspace = "docker" }, "рабочей копии"},
		{"отрицательный бюджет", func(p *Plan) { p.BudgetUSD = -1 }, "отрицательный бюджет"},
		{"без таймаута", func(p *Plan) { p.StageTimeout = 0 }, "таймаут"},
	}
	for _, c := range cases {
		p := validPlan()
		c.fix(p)
		err := p.Validate()
		if err == nil {
			t.Errorf("%s: план принят", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: сообщение %q не содержит %q", c.name, err, c.want)
		}
	}
}

// Невалидный план не должен существовать в виде значения, которое можно
// случайно начать исполнять, — ни на приёме, ни на отправке.
func TestInvalidPlanNeverMaterializes(t *testing.T) {
	bad := validPlan()
	bad.Stages = nil
	if _, err := bad.Encode(); err == nil {
		t.Error("невалидный план ушёл в отправку")
	}
	raw, _ := json.Marshal(bad)
	if p, err := Decode(raw); err == nil {
		t.Errorf("невалидный план принят: %+v", p)
	}
	if _, err := Decode([]byte("{не json")); err == nil {
		t.Error("мусор разобран как план")
	}
}

// В плане не должно быть кода проекта: если бы он там был, оркестратору
// пришлось бы сначала привезти репозиторий к себе.
func TestPlanCarriesNoSourceCode(t *testing.T) {
	raw, err := validPlan().Encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"files", "content", "diff", "patch", "repo_map", "chunks"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("план несёт поле %q — содержимое репозитория в плане не передаётся", forbidden)
		}
	}
}

func TestNegotiateSchema(t *testing.T) {
	if v, err := NegotiateSchema(MinSchemaVersion, SchemaVersion); err != nil || v != SchemaVersion {
		t.Errorf("совпадающие диапазоны: %d, %v", v, err)
	}
	// Исполнитель старее: договариваемся о версии, которую он понимает.
	if v, err := NegotiateSchema(1, 1); err != nil || v != 1 {
		t.Errorf("старый исполнитель: %d, %v", v, err)
	}
	// Пересечения нет — договориться нельзя, и это отказ, а не тихое согласие.
	if _, err := NegotiateSchema(SchemaVersion+5, SchemaVersion+9); err == nil {
		t.Error("непересекающиеся диапазоны сошлись")
	}
}

// validPlanV2 — план схемы 2 по базовому пайплайну.
func validPlanV2() *Plan {
	p := validPlan()
	hashes := map[string]string{}
	for _, n := range BuiltinSkills {
		hashes[n] = "h-" + n
	}
	if err := p.Upgrade(hashes); err != nil {
		panic(err)
	}
	return p
}

func TestPlanV2RoundTrips(t *testing.T) {
	p := validPlanV2()
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.SchemaVersion != 2 || len(back.Steps) != len(p.Steps) || len(back.Skills) != len(p.Skills) {
		t.Fatalf("план схемы 2 изменился при передаче: %+v", back)
	}
	if back.Step("execute").Bind["BASE"] != "$task.base_commit" {
		t.Error("привязки потерялись")
	}
}

func TestPlanV2Validation(t *testing.T) {
	cases := []struct {
		name string
		fix  func(*Plan)
		want string
	}{
		{"без шагов", func(p *Plan) { p.Steps = nil }, "ни одного шага"},
		{"шаг без хэша", func(p *Plan) { p.Steps[1].SkillHash = "" }, "без хэша"},
		{"без модели", func(p *Plan) { p.Steps[1].Model = "" }, "не указана модель"},
		{"неизвестный род", func(p *Plan) { p.Steps[2].Kind = "action.deploy" }, "неизвестный род"},
		{"ссылка вперёд", func(p *Plan) { p.Steps[1].Bind["X"] = "$steps.review.out" }, "нет раньше"},
	}
	for _, c := range cases {
		p := validPlanV2()
		c.fix(p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: получено %v, ждали %q", c.name, err, c.want)
		}
	}
}
