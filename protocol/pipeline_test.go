package protocol

import (
	"strings"
	"testing"
)

const solveManifest = `
name: solve-task
title: Решение задачи
version: 1
inputs:
  TASK_TEXT:   { type: text,    required: true }
  REFERENCE:   { type: text,    required: true }
  BASE_BRANCH: { type: git_ref, required: true }
  FEEDBACK:    { type: text,    required: false }
context: [repo_map]
workspace: self
cwd: worktree
tools: unrestricted
code_search: true
questions: true
resumable: true
outputs:
  artifacts:
    - { name: solution.md, title: Решение, type: markdown }
    - name: findings.json
      title: Замечания
      type: json
      schema:
        type: array
        items:
          type: object
          required: [id, text]
          properties:
            id: { type: string }
            severity: { type: string, enum: [critical, important, minor] }
            text: { type: string }
  markers:
    BRANCH: { type: git_branch }
    RESULT: { type: enum, values: [done, blocked] }
checks:
  - artifact_exists: solution.md
  - marker_present: BRANCH
  - has_commits: { branch: BRANCH }
  - clean_tree
model_preference: [opus, fable51, sonnet]
`

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(solveManifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.InputOrder, ","); got != "TASK_TEXT,REFERENCE,BASE_BRANCH,FEEDBACK" {
		t.Errorf("порядок входов %s", got)
	}
	if !m.Inputs["TASK_TEXT"].Required || m.Inputs["FEEDBACK"].Required {
		t.Errorf("обязательность входов: %+v", m.Inputs)
	}
	if m.Workspace != WorkspaceSelf || !m.Resumable || len(m.Tools) != 0 {
		t.Errorf("свойства: %+v", m)
	}
	if len(m.Checks) != 4 || m.Checks[2].Kind != "has_commits" || m.Checks[2].Arg != "BRANCH" || m.Checks[3].Kind != "clean_tree" {
		t.Errorf("проверки: %+v", m.Checks)
	}
	if m.Outputs.Markers["RESULT"].Values[1] != "blocked" {
		t.Errorf("маркеры: %+v", m.Outputs.Markers)
	}
	if err := ValidateJSON([]byte(`[{"id":"f1","severity":"minor","text":"x"}]`), m.Outputs.Artifacts[1].Schema); err != nil {
		t.Errorf("валидный JSON отвергнут: %v", err)
	}
	if err := ValidateJSON([]byte(`[{"id":"f1","severity":"huge","text":"x"}]`), m.Outputs.Artifacts[1].Schema); err == nil || !strings.Contains(err.Error(), "severity") {
		t.Errorf("enum не проверен: %v", err)
	}
	if err := ValidateJSON([]byte(`[{"id":"f1"}]`), m.Outputs.Artifacts[1].Schema); err == nil || !strings.Contains(err.Error(), "text") {
		t.Errorf("required не проверен: %v", err)
	}
}

func TestParseManifestErrors(t *testing.T) {
	cases := map[string]string{
		"inputs.X.type":  "name: a\nversion: 1\ninputs:\n  X: {type: number}\n",
		"schema":         "name: a\nversion: 1\noutputs:\n  artifacts:\n    - {name: f.json, type: json}\n",
		"неизвестное":    "name: a\nversion: 1\nbogus: 1\n",
		"не объявлен":    "name: a\nversion: 1\nchecks:\n  - artifact_exists: x.md\n",
		"has_commits":    "name: a\nversion: 1\noutputs:\n  markers:\n    R: {type: text}\nchecks:\n  - has_commits: {branch: R}\n",
		"workspace":      "name: a\nversion: 1\nworkspace: shared\n",
	}
	for want, raw := range cases {
		_, err := ParseManifest([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: ожидалась ошибка с %q, получено %v", want, want, err)
		}
	}
}

func TestBasePipelineValid(t *testing.T) {
	p := BasePipeline()
	if err := p.Validate(nil); err != nil {
		t.Fatalf("базовый пайплайн невалиден: %v", err)
	}
	if p.ReworkKey() != "analyze" {
		t.Errorf("rework %s", p.ReworkKey())
	}
	if got := strings.Join(p.StepKeys(false), ","); got != "analyze,err_work,branch,execute,test_gate,review,handoff" {
		t.Errorf("ключи без старта: %s", got)
	}
	if got := p.StepKeys(true)[0]; got != "import" {
		t.Errorf("первый ключ со стартом: %s", got)
	}
}

func TestPipelineValidateErrors(t *testing.T) {
	man, _ := ParseManifest([]byte(solveManifest))
	ms := Manifests{"solve-task": man}
	mk := func(edit func(p *Pipeline)) *Pipeline {
		p := &Pipeline{Schema: 2, Name: "mono", Title: "m", Steps: []Step{
			{Key: "start", Kind: KindStartTask, Title: "Условие"},
			{Key: "solve", Kind: KindAgent, Title: "Решение", Skill: "solve-task",
				Bind: map[string]string{"TASK_TEXT": "$task.prompt", "REFERENCE": "$task.reference", "BASE_BRANCH": "$project.base_branch"}},
			{Key: "end", Kind: KindFinish, Title: "Результат", Result: "branch"},
		}}
		edit(p)
		return p
	}
	if err := mk(func(*Pipeline) {}).Validate(ms); err != nil {
		t.Fatalf("валидный отвергнут: %v", err)
	}
	cases := map[string]func(p *Pipeline){
		"не привязан":       func(p *Pipeline) { delete(p.Steps[1].Bind, "REFERENCE") },
		"не объявляет вход": func(p *Pipeline) { p.Steps[1].Bind["EXTRA"] = "x" },
		"нет раньше":        func(p *Pipeline) { p.Steps[1].Bind["FEEDBACK"] = "$steps.end.out" },
		"нет поля":          func(p *Pipeline) { p.Steps[1].Bind["FEEDBACK"] = "$task.nope" },
		"неизвестный род":   func(p *Pipeline) { p.Steps[1].Kind = "action.deploy" },
		"стартовым":         func(p *Pipeline) { p.Steps[0].Kind = KindBranch },
		"финишем":           func(p *Pipeline) { p.Steps[2].Kind = KindBranch },
		"дважды":            func(p *Pipeline) { p.Steps[0].Key = "solve" },
		"не найден":         func(p *Pipeline) { p.Steps[1].Skill = "ghost" },
		"несуществующий":    func(p *Pipeline) { p.Rework = "nope" },
		"последующие":       func(p *Pipeline) { p.Steps[1].On = map[string]map[string]Reaction{"RESULT": {"done": {Skip: []string{"start"}}}} },
		"не объявляет маркер": func(p *Pipeline) { p.Steps[1].Emit = map[string]string{"NOPE": "title"} },
		"нельзя записать":   func(p *Pipeline) { p.Steps[1].Emit = map[string]string{"BRANCH": "prompt"} },
	}
	for want, edit := range cases {
		err := mk(edit).Validate(ms)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: ожидалась ошибка с %q, получено %v", want, want, err)
		}
	}
}

func TestUpgradeV1(t *testing.T) {
	p := &Plan{SchemaVersion: 1, TaskID: 7, Project: Project{Path: "/p", BaseBranch: "main"},
		Continuity: "non_stop", Workspace: "worktree", StageTimeout: 60,
		Stages: []Stage{
			{Key: "analyze", Skill: "analyze-task", Model: "claude-opus-5", Effort: "high"},
			{Key: "decompose", Virtual: true},
			{Key: "branch"},
			{Key: "execute", Skill: "execute-plan", Model: "claude-opus-5", Effort: "high"},
		}}
	hashes := map[string]string{}
	for _, n := range BuiltinSkills {
		hashes[n] = "h-" + n
	}
	if err := p.Upgrade(hashes); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("после перевода план невалиден: %v", err)
	}
	if p.Step("analyze").Bind["DECOMPOSE"] != "yes" || p.Step("analyze").Disabled {
		t.Errorf("анализ: %+v", p.Step("analyze"))
	}
	if !p.Step("err_work").Disabled || !p.Step("review").Disabled || p.Step("test_gate").Disabled {
		t.Errorf("выключенные: err_work=%v review=%v test_gate=%v", p.Step("err_work").Disabled, p.Step("review").Disabled, p.Step("test_gate").Disabled)
	}
	if p.QA == nil || p.QA.Model != "claude-opus-5" || p.QA.SkillHash != "h-answer-question" {
		t.Errorf("QA: %+v", p.QA)
	}
	if len(p.Stages) != 0 || p.SchemaVersion != 2 {
		t.Errorf("остались этапы схемы 1")
	}
}
