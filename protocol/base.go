package protocol

// BasePipeline — встроенный базовый пайплайн: прежний конвейер один в один,
// выраженный в схеме. Одна копия на обе стороны: сервис заводит из неё
// запись в базе, исполнитель переводит по ней планы схемы 1.
func BasePipeline() *Pipeline {
	return &Pipeline{
		Schema: PipelineSchema, Name: "base", Title: "Базовый пайплайн",
		Defaults: PipelineDefaults{Continuity: "non_stop", Workspace: "worktree", QASkill: "answer-question"},
		Rework:   "analyze",
		Steps: []Step{
			{Key: "import", Kind: KindStartTask, Title: "Импорт задачи", Skill: "import-gitlab",
				Desc: "Условие от человека или задача из GitLab: описание, комментарии людей, вложения — в папку задачи"},
			{Key: "analyze", Kind: KindAgent, Title: "Анализ задачи", Skill: "analyze-task",
				Desc: "Читает требования и код, пишет план решения и предлагает имя ветки; при декомпозиции спрашивает, как разбить",
				Bind: map[string]string{
					"TASK_TEXT": "$task.prompt", "DECOMPOSE": "yes", "REVISION": "$task.revision",
					"PREVIOUS_ROUND": "$task.previous_round", "PROJECT_STACK": "$project.stack",
					"PROJECT_DESCRIPTION": "$project.description",
				},
				Emit: map[string]string{"BRANCH_DESCRIPTION": "branch_slug"},
				On:   map[string]map[string]Reaction{"PLAN_RESULT": {"no-code": {Skip: []string{"execute", "review"}}}}},
			{Key: "err_work", Kind: KindAgent, Title: "Работа над ошибками", Skill: "plan-review", Passes: 2, Requires: []string{"analyze"},
				Desc: "Проверяет план свежим взглядом против кода и требований и исправляет его; несколько проходов, чистый проход останавливает остальные",
				Bind: map[string]string{"PASS_NUMBER": "$task.pass", "SCOPE": "$task.pass_scope"},
				On: map[string]map[string]Reaction{
					"PLAN_REVIEW": {"clean": {StopPasses: true}},
					"PLAN_RESULT": {"no-code": {Skip: []string{"execute", "review"}}},
				}},
			{Key: "branch", Kind: KindBranch, Title: "Создание ветки", Desc: "Переименовывает ветку задачи по референсу и описанию — без агента"},
			{Key: "execute", Kind: KindAgent, Title: "Выполнение", Skill: "execute-plan", Requires: []string{"analyze", "branch"},
				Desc: "Реализует план в рабочей копии и коммитит",
				Bind: map[string]string{"REFERENCE": "$task.reference", "TITLE": "$task.branch_title", "BASE": "$task.base_commit"}},
			{Key: "test_gate", Kind: KindTestGate, Title: "Тест-гейт", FixWith: "execute",
				Desc: "Прогоняет команду тестов проекта; падение чинится сессией выполнения один раз"},
			{Key: "review", Kind: KindAgent, Title: "Ревью", Skill: "review-task", Requires: []string{"execute"},
				Desc: "Проверяет получившийся дифф против требований и плана; в режиме «без остановок» сам исправляет найденное",
				Bind: map[string]string{"REFERENCE": "$task.reference", "BASE_COMMIT": "$task.base_commit", "MODE": "$plan.review_mode"}},
			{Key: "handoff", Kind: KindAgent, Title: "Инструкция по проверке", Skill: "handoff-notes", Requires: []string{"execute"},
				Desc: "Пишет, как увидеть результат: что пересобрать и в каком порядке, как запустить, что проверить руками",
				Bind: map[string]string{"REFERENCE": "$task.reference", "BRANCH": "$task.branch_name", "BASE_COMMIT": "$task.base_commit",
					"WORKSPACE": "$plan.workspace", "WORKTREE_DIR": "$task.worktree_dir", "TEST_CMD": "$project.test_cmd"}},
			{Key: "end", Kind: KindFinish, Title: "Результат", Result: "branch"},
		},
	}
}

// AnswerStep — системный шаг ответа на вопрос к готовой таске.
func AnswerStep(skill, model, effort string) *Step {
	return &Step{Key: "answer", Kind: KindAgent, Title: "Обработка запроса пользователя", Skill: skill,
		Desc:  "Отвечает на вопрос к готовой таске, не меняя код",
		Model: model, Effort: effort,
		Bind: map[string]string{"QUESTION": "$task.question", "BRANCH": "$task.branch_name"}}
}

// BuiltinSkills — скиллы, которые сервис несёт в себе и заводит в реестре.
var BuiltinSkills = []string{"import-gitlab", "analyze-task", "plan-review", "execute-plan", "review-task", "handoff-notes", "answer-question"}
