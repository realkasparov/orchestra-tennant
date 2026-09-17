package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/codeindex"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Pipeline — Runner: ведёт задание по этапам плана. Это перенесённый цикл
// этапов оркестратора; вместо записи в базу — события, вместо чтения таски из
// базы — план и собственное состояние.
type Pipeline struct {
	// DataDir — папка данных исполнителя: здесь папки задач и индекс кода.
	DataDir string
	// ProjectsDir — папка проектов машины: проекты заводятся внутри неё.
	ProjectsDir string
	// Index — индекс кода; nil означает «индексация не подключена».
	Index *codeindex.Manager
	// EnsureIndex — заводить индекс проекта по плану. Встроенному исполнителю
	// это не нужно: индексы его машины ведёт оркестратор, у которого есть
	// настройки проектов; демону — некому, кроме него самого.
	EnsureIndex bool
	Log         func(format string, args ...any)
}

const continuationPrompt = "Сначала прочитай TASK_DIR/task.md — там текущее состояние решения. " +
	"Затем продолжи прерванный этап с места остановки, опираясь на task.md и уже созданные артефакты."

// errBudget — потолок расхода достигнут: пауза, не ошибка; «Возобновить»
// продолжит без лимита.
var errBudget = errors.New("budget exceeded")

// run — состояние одного прогона: задание, план и удобные ссылки.
type run struct {
	p    *Pipeline
	job  *Job
	plan *protocol.Plan
	st   *TaskState

	change       changeRequest
	pendingUsage protocol.Usage // расход триажа до того, как заведён этап, куда его отнести
	// questions — вопросы человека, ждущие границы этапа: отвечает цикл
	// этапов, а не горутина сообщений.
	questions chan question
}

// Run исполняет задание. Возвращаемый статус — терминальный статус таски.
func (p *Pipeline) Run(ctx context.Context, job *Job) (string, error) {
	if job.State == nil {
		job.State = &TaskState{}
	}
	r := &run{p: p, job: job, plan: job.Plan, st: job.State, questions: make(chan question, 8)}
	if err := r.prepare(); err != nil {
		r.log("", "Ошибка: "+err.Error())
		return "error", err
	}
	if p.EnsureIndex && p.Index != nil {
		pr := r.plan.Project
		if _, err := p.Index.Ensure(ctx, pr.ID, pr.Path, pr.IndexMode, codeindex.ExcludeList(pr.IndexExclude)); err != nil {
			r.log("", "Индекс кода: "+err.Error())
		}
	}
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	go r.serveMessages(mctx)
	// Сообщение, с которым задание запущено (правка или вопрос к готовой
	// таске), разбирается до этапов: правка заведёт новый раунд, вопрос —
	// ответ в чате.
	if m := r.plan.Message; m != nil && r.st.MessageJob != job.ID {
		r.handleMessage(ctx, m.Text, m.Mode)
		// Отметка — до нового раунда: повторное предложение того же
		// задания (перезапуск, потерянный итог) не должно разбирать правку
		// снова и хоронить начатый раунд.
		r.st.MessageJob = job.ID
		r.job.SaveState()
		if r.change.take() {
			if err := r.checkWorkspace(); err != nil {
				return r.failRound(err)
			}
			if err := r.startChangeRound(); err != nil {
				return r.failRound(err)
			}
		} else if r.allStagesDone() {
			// Вопрос к готовой таске: ответ дан, этапам делать нечего —
			// гонять их цикл значило бы мигать «выполняется → готово».
			r.answerQueued(ctx)
			return "done", nil
		}
	}
	for {
		status, err, restart := r.pipeline(ctx)
		if !restart {
			if ctx.Err() == nil {
				// Вопросы, заданные под конец, не теряются: задание ещё здесь.
				r.answerQueued(ctx)
			}
			return status, err
		}
		// Папка проверяется до архивации артефактов: отказ не должен
		// оставлять пустой раунд с перенесёнными файлами прошлого.
		if err := r.checkWorkspace(); err != nil {
			return r.failRound(err)
		}
		if err := r.startChangeRound(); err != nil {
			return r.failRound(err)
		}
	}
}

// failRound — новый раунд не начался (грязная рабочая копия): ошибка в
// журнал и статус задания, этапы не тронуты.
func (r *run) failRound(err error) (string, error) {
	r.log("", "Ошибка: "+err.Error())
	r.taskStatus("error")
	return "error", err
}

// allStagesDone — в последнем раунде не осталось этапов, которым есть что
// делать (виртуальные не в счёт: ими управляет анализ).
func (r *run) allStagesDone() bool {
	for _, key := range r.stageKeys() {
		st := r.st.stage(key)
		if st == nil || st.Status == "done" || st.Status == "skipped" {
			continue
		}
		if def := r.plan.Stage(key); def != nil && def.Virtual {
			continue
		}
		return false
	}
	return true
}

// prepare заводит папку задачи и первый раунд этапов, если их ещё нет.
func (r *run) prepare() error {
	own := filepath.Join(r.p.DataDir, "tasks", strconv.FormatInt(r.plan.TaskID, 10))
	if r.st.TaskDir == "" && r.plan.TaskDir != "" {
		// Папка задачи оркестратора — подсказка для машины, где он сам и
		// живёт: там лежат вложения человека. На другой машине этого пути
		// может не быть вовсе (чужой домашний каталог), и тогда папка —
		// своя, а не ошибка на первом же шаге.
		if err := os.MkdirAll(filepath.Join(r.plan.TaskDir, "attachments", "user"), 0o755); err == nil {
			r.st.TaskDir = r.plan.TaskDir
		} else {
			r.log("", "Папка задачи оркестратора недоступна ("+err.Error()+") — использую свою: "+own)
		}
	}
	if r.st.TaskDir == "" {
		r.st.TaskDir = own
	}
	if err := os.MkdirAll(filepath.Join(r.st.TaskDir, "attachments", "user"), 0o755); err != nil {
		return err
	}
	if r.st.Title == "" {
		r.st.Title = r.plan.Title
	}
	if r.st.Reference == "" {
		r.st.Reference = r.plan.Reference
	}
	if r.st.BaseCommit == "" {
		r.st.BaseCommit = r.plan.Project.BaseCommit
	}
	if r.st.BranchName == "" {
		r.st.BranchName = r.plan.Project.Branch
	}
	if r.plan.BudgetAck {
		r.st.BudgetAck = true
	}
	seedTaskMD(r.st.TaskDir, r.st.Title, r.plan.Prompt, r.plan.SourceURL)
	if len(r.st.Stages) == 0 {
		r.st.addRound(r.stageKeys())
		// Этапы, которые оркестратор уже считает выполненными (возобновление
		// после потери состояния), не переисполняются.
		for _, key := range r.job.Skip {
			if st := r.st.stage(key); st != nil {
				st.Status = "done"
			}
		}
	}
	r.job.SaveState()
	return nil
}

func (r *run) stageKeys() []string {
	keys := make([]string, 0, len(r.plan.Stages))
	for _, s := range r.plan.Stages {
		keys = append(keys, s.Key)
	}
	return keys
}

// pipeline — цикл по этапам последнего раунда. restart означает, что человек
// прислал правку: раунд нужно завести заново и пройти снова.
func (r *run) pipeline(ctx context.Context) (status string, err error, restart bool) {
	r.taskStatus("running")
	if err := r.checkWorkspace(); err != nil {
		r.log("", "Ошибка: "+err.Error())
		r.taskStatus("error")
		return "error", err, false
	}

	fail := func(stage string, err error) (string, error, bool) {
		if ctx.Err() != nil { // пауза или остановка, не провал
			r.markPaused(stage)
			return "paused", nil, false
		}
		if r.change.take() { // этап прерван правкой — не ошибка
			r.setStageStatus(stage, "pending")
			return "", nil, true
		}
		if errors.Is(err, errBudget) {
			r.markPaused(stage)
			return "paused", err, false
		}
		r.log(stage, "Ошибка: "+err.Error())
		r.setStageStatus(stage, "error")
		r.taskStatus("error")
		return "error", err, false
	}

	// Ожидание «Возобновить» пережило перезапуск (демона или оркестратора):
	// человек его ещё не нажал, и следующий этап не начинается сам.
	if r.st.AwaitContinue != "" {
		if paused := r.waitContinue(ctx, r.st.AwaitContinue); paused {
			return "paused", nil, false
		}
		if r.change.take() {
			return "", nil, true
		}
		r.taskStatus("running")
	}

	keys := r.stageKeys()
	for i, key := range keys {
		st := r.st.stage(key)
		if st == nil || st.Status == "done" || st.Status == "skipped" {
			continue
		}
		if def := r.plan.Stage(key); def != nil && def.Virtual {
			continue // декомпозицией управляет анализ
		}
		// Граница этапа — точка восстановления: без связи этап доделывается,
		// но следующий не начинается, пока оркестратор не подтвердит, что
		// задание всё ещё за нами.
		if err := r.job.WaitConnected(ctx); err != nil {
			return fail(key, err)
		}
		if key != "import" && r.st.WorktreeDir == "" {
			if err := r.setupWorktree(); err != nil {
				return fail(key, err)
			}
		}

		// Свой контекст на этап: правка прерывает этап, не задание.
		sctx, scancel := context.WithCancel(ctx)
		r.change.arm(scancel)
		var err error
		switch key {
		case "import":
			err = r.stageImport(sctx, st)
		case "analyze":
			err = r.stageAnalyze(sctx, st)
		case "err_work":
			err = r.stageErrWork(sctx, st)
		case "branch":
			err = r.stageBranch(st)
		case "execute":
			err = r.stageExecute(sctx, st)
		case "review":
			err = r.stageReview(sctx, st)
		case "handoff":
			err = r.stageHandoff(sctx, st)
		default:
			err = fmt.Errorf("неизвестный этап %q", key)
		}
		r.change.arm(nil)
		scancel()
		if err != nil {
			return fail(key, err)
		}
		if r.change.take() { // правка пришла на границе этапа
			return "", nil, true
		}
		r.job.SaveState()
		r.answerQueued(ctx)

		// per_stage: остановиться после этапа и ждать «Возобновить».
		if r.plan.Continuity == "per_stage" && i != len(keys)-1 {
			if paused := r.waitContinue(ctx, key); paused {
				return "paused", nil, false
			}
			if r.change.take() {
				return "", nil, true
			}
			r.taskStatus("running")
		}
	}

	// Ничего не удаляем: рабочая копия и артефакты остаются до явного
	// «Удалить» или новой правки.
	if r.st.WorktreeDir != "" {
		where := "worktree сохранён: " + r.st.WorktreeDir
		if r.plan.Workspace == "folder" {
			where = "изменения в папке проекта: " + r.st.WorktreeDir
		}
		r.log("", "Готово. Ветка: "+r.st.BranchName+". "+where+
			". Ничего не удаляю — жду указаний (отправьте правку в диалог или удалите задачу вручную).")
	}
	r.emitDiff()
	r.taskStatus("done")
	return "done", nil, false
}

// waitContinue ждёт «Возобновить» после этапа key. Ожидание записано в
// состоянии: после перезапуска оно продолжается, а не пропускается. Пока
// ждём, отвечаем на вопросы из чата — этап не идёт, момент удобный.
// Возвращает true, если задание остановили.
func (r *run) waitContinue(ctx context.Context, key string) (paused bool) {
	r.st.AwaitContinue = key
	r.job.SaveState()
	r.taskStatus("waiting_user")
	r.log(key, "Этап завершён. Нажмите «Возобновить», чтобы продолжить.")
	for {
		select {
		case c := <-r.job.Continue():
			if c.BudgetAck {
				r.st.BudgetAck = true
			}
			r.st.AwaitContinue = ""
			r.job.SaveState()
			return false
		case q := <-r.questions:
			r.answerQuestionRound(ctx, q.text, q.usage)
		case <-ctx.Done():
			// Остановили человеком: его «Возобновить» после паузы и есть
			// ответ на это ожидание, второй раз спрашивать не нужно.
			// Перезапуск демона или оркестратора — не человек: ожидание
			// остаётся.
			if r.job.cancelReason() == CancelPause {
				r.st.AwaitContinue = ""
			}
			r.markPaused("")
			return true
		}
	}
}

// --- запуск агента ---

// runAgentStage — один прогон этапа с циклом вопросов QUESTIONS_JSON.
// Возвращает склеенный текст ответов агента.
func (r *run) runAgentStage(ctx context.Context, st *StageState, prompt, cwd string, pass int) (string, error) {
	// Прежний статус нужен до того, как этап помечен идущим: по нему видно,
	// что прогон был прерван и его сессию можно продолжить.
	prev := st.Status
	st.Status = "running"
	r.emitStage(st, "running")

	resume := ""
	if st.SessionID != "" && (prev == "paused" || prev == "waiting_user") && st.CurrentPass == pass {
		resume = st.SessionID
		answers, err := r.waitAndCollectAnswers(ctx, st)
		if err != nil {
			return "", err
		}
		if answers != "" {
			prompt = answers
		} else {
			prompt = continuationPrompt
		}
	}
	return r.runAgentSession(ctx, st, prompt, cwd, resume, pass)
}

// runAgentSession — прогон агента этапа; resume — сессия, которую надо
// продолжить (пусто — новая). Отдельно от runAgentStage: автопочинка после
// тест-гейта продолжает сессию реализации, хотя этап и не прерывался.
func (r *run) runAgentSession(ctx context.Context, st *StageState, prompt, cwd, resume string, pass int) (string, error) {
	var all strings.Builder
	for {
		if err := r.checkBudget(); err != nil {
			return all.String(), err
		}
		// Вотчдог: один прогон не может идти дольше лимита.
		runCtx, cancel := context.WithTimeout(ctx, r.plan.StageTimeout.Duration())
		onEvent := func(ev agent.StreamEvent) {
			r.job.Emit(st.Key, ev.Type, ev.Payload)
		}
		mcpCfg, extraTools := r.p.codeSearchFor(r.plan, st.Key)
		def := r.plan.Stage(st.Key)
		model, effort := "fable", ""
		if def != nil {
			model, effort = def.Model, def.Effort
		}
		res, err := agent.Run(runCtx, agent.RunOpts{
			Prompt: prompt, Resume: resume,
			Model: agent.ModelID(model), Effort: effort,
			CWD: cwd, AddDirs: []string{r.st.TaskDir},
			AllowedTools: withTools(toolsFor(r.plan.Continuity, st.Key), extraTools),
			MCPConfig:    mcpCfg,
		}, onEvent)
		timedOut := runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		cancel()
		if res != nil && res.SessionID != "" {
			st.SessionID, st.CurrentPass = res.SessionID, pass
		}
		r.recordUsage(st, res)
		r.job.SaveState()
		if timedOut {
			return all.String(), fmt.Errorf("этап превысил лимит времени (%s) и был остановлен", r.plan.StageTimeout.Duration())
		}
		if err != nil {
			return all.String(), err
		}
		all.WriteString(res.FullText)
		resume = res.SessionID

		qs, present, qerr := agent.Questions(res.FullText)
		if qerr != nil {
			return all.String(), fmt.Errorf("невалидный QUESTIONS_JSON: %w", qerr)
		}
		if !present || len(qs) == 0 {
			return all.String(), nil
		}
		answers, err := r.askUser(ctx, st, qs)
		if err != nil {
			return all.String(), err
		}
		prompt = answers
	}
}

// askUser записывает вопросы, ждёт ответов и собирает промпт продолжения.
func (r *run) askUser(ctx context.Context, st *StageState, qs []agent.QuestionSpec) (string, error) {
	hasDecompose := false
	for _, q := range qs {
		saved := r.st.addQuestion(QuestionState{Stage: st.Key, Type: q.Type, Question: q.Question,
			Options: q.Options, AllowCustom: q.AllowCustom})
		if q.Type == "decompose" {
			hasDecompose = true
		}
		r.job.Emit(st.Key, "question", map[string]any{
			"question_id": saved.ID, "type": q.Type, "question": q.Question,
			"options": q.Options, "allow_custom": q.AllowCustom,
		})
	}
	r.job.SaveState()
	if hasDecompose {
		r.setStageStatus("decompose", "waiting_user")
	}
	answers, err := r.waitAndCollectAnswers(ctx, st)
	if err != nil {
		return "", err
	}
	if hasDecompose {
		r.setStageStatus("decompose", "done")
	}
	if answers == "" {
		answers = continuationPrompt
	}
	return answers, nil
}

// waitAndCollectAnswers ждёт, пока не останется открытых вопросов, и
// возвращает промпт из ответов этого этапа, ещё не переданных агенту.
func (r *run) waitAndCollectAnswers(ctx context.Context, st *StageState) (string, error) {
	waited := false
	for len(r.st.openQuestions()) > 0 {
		if !waited {
			waited = true
			st.Status = "waiting_user"
			r.emitStage(st, "waiting_user")
			r.taskStatus("waiting_user")
			r.job.SaveState()
		}
		select {
		case a := <-r.job.Answers():
			if q := r.st.question(a.QuestionID); q != nil && q.Status == "open" {
				q.Answer, q.Status = a.Text, "answered"
				r.job.SaveState()
			}
		case q := <-r.questions:
			r.answerQuestionRound(ctx, q.text, q.usage)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if waited {
		r.taskStatus("running")
		st.Status = "running"
		r.emitStage(st, "running")
	}
	var sb strings.Builder
	delivered := 0
	for i := range r.st.Questions {
		q := &r.st.Questions[i]
		if q.Stage != st.Key || q.Status != "answered" {
			continue
		}
		if delivered == 0 {
			sb.WriteString("Ответы пользователя:\n")
		}
		fmt.Fprintf(&sb, "- Вопрос: %s\n  Ответ: %s\n", q.Question, q.Answer)
		q.Status = "delivered"
		delivered++
	}
	if delivered == 0 {
		return "", nil
	}
	r.job.SaveState()
	return sb.String(), nil
}

// --- бюджет и расход ---

func (r *run) checkBudget() error {
	if r.plan.BudgetUSD <= 0 || r.st.BudgetAck {
		return nil
	}
	if cost := r.st.cost(); cost >= r.plan.BudgetUSD {
		r.log("", fmt.Sprintf("Бюджет задачи исчерпан: $%.2f из $%.2f. Задача на паузе — «Возобновить» продолжит без лимита.",
			cost, r.plan.BudgetUSD))
		return errBudget
	}
	return nil
}

func usageOf(res *agent.Result) protocol.Usage {
	if res == nil {
		return protocol.Usage{}
	}
	return protocol.Usage{TokIn: res.Usage.InputTokens, TokOut: res.Usage.OutputTokens,
		CacheWrite: res.Usage.CacheWrite, CacheRead: res.Usage.CacheRead, CostUSD: res.Usage.CostUSD}
}

// recordUsage накапливает расход прогона в этап и дублирует итог в
// stage_status, чтобы вкладка «Этапы» обновилась без перезагрузки.
func (r *run) recordUsage(st *StageState, res *agent.Result) {
	u := usageOf(res)
	if st == nil || u.Zero() {
		return
	}
	st.Usage = st.Usage.Add(u)
	r.job.Emit(st.Key, "stage_status", map[string]any{
		"key": st.Key, "status": "running", "round": st.Round, "usage": st.Usage,
	})
}

// --- статусы и события ---

func (r *run) log(stage, text string) {
	r.job.Emit(stage, "log", map[string]any{"text": text})
}

func (r *run) emitStage(st *StageState, status string) {
	r.job.Emit(st.Key, "stage_status", map[string]any{"key": st.Key, "status": status, "round": st.Round})
}

func (r *run) taskStatus(status string) {
	r.job.Emit("", "task_status", map[string]any{"status": status})
}

// setStageStatus меняет статус этапа последнего раунда по ключу.
func (r *run) setStageStatus(key, status string) {
	st := r.st.stage(key)
	if st == nil {
		return
	}
	st.Status = status
	r.emitStage(st, status)
}

func (r *run) finishStage(st *StageState) error {
	st.Status = "done"
	r.emitStage(st, "done")
	r.emitArtifacts()
	return nil
}

func (r *run) markPaused(stageKey string) {
	if stageKey != "" {
		r.setStageStatus(stageKey, "paused")
	}
	for _, st := range r.st.Stages {
		if st.Status == "running" || st.Status == "waiting_user" {
			st.Status = "paused"
			r.emitStage(st, "paused")
		}
	}
	r.taskStatus("paused")
	r.job.SaveState()
}

// emitArtifacts отправляет файлы шагов: оркестратор читал их с диска, теперь
// диска у него нет.
func (r *run) emitArtifacts() {
	entries, err := os.ReadDir(r.st.TaskDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.st.TaskDir, name))
		if err != nil {
			continue
		}
		r.job.Emit("", protocol.EventArtifact, map[string]any{"name": name, "content": string(data)})
	}
}

// emitDiff отправляет дифф ветки против базы задачи.
func (r *run) emitDiff() {
	if r.st.WorktreeDir == "" {
		return
	}
	files, err := gitops.Diff(r.st.WorktreeDir, r.st.BaseCommit)
	if err != nil {
		return
	}
	head, _ := gitops.HeadSHA(r.st.WorktreeDir)
	r.job.Emit("", protocol.EventDiff, map[string]any{
		"branch": r.st.BranchName, "head": head, "base": r.st.BaseCommit, "files": files,
	})
}

// --- рабочая копия ---

// checkWorkspace — рабочая копия в папке проекта всё ещё наша: там наша
// ветка и в ней не идёт другая таска. Иначе продолжение легло бы в чужую
// ветку.
func (r *run) checkWorkspace() error {
	proj := r.plan.Project
	if r.plan.Workspace != "folder" {
		return nil
	}
	if other := r.job.ex.folderHolder(proj.Path, r.plan.TaskID); other != 0 {
		return fmt.Errorf("папка проекта занята таской #%d, которая сейчас идёт в ней — запустите эту таску в режиме git worktree или дождитесь той", other)
	}
	if r.st.WorktreeDir == "" || r.st.BranchName == "" {
		return nil
	}
	cur, err := gitops.CurrentBranch(r.st.WorktreeDir)
	if err != nil {
		return err
	}
	if cur == "HEAD" {
		return fmt.Errorf("папка проекта в отсоединённом состоянии (detached HEAD), а таска ждёт свою ветку «%s» — выполните git checkout %s", r.st.BranchName, r.st.BranchName)
	}
	if cur != r.st.BranchName {
		return fmt.Errorf("в папке проекта сейчас ветка «%s», а таска ждёт свою «%s» — переключите ветку или откройте таску в папке проекта", cur, r.st.BranchName)
	}
	return nil
}

func (r *run) setupWorktree() error {
	proj := r.plan.Project
	// Репозиторий без origin — обычное дело для локального проекта: базу
	// берём как есть, без «не удался» в журнале каждой таски.
	if !gitops.HasRemote(proj.Path, "origin") {
		r.log("", "У репозитория нет origin — база берётся из локальной ветки "+proj.BaseBranch+".")
	} else if err := gitops.FetchBase(proj.Path, proj.BaseBranch); err != nil {
		r.log("", "fetch origin не удался (продолжаю от локальной базы): "+err.Error())
	}
	branch := fmt.Sprintf("task/%d-wip", r.plan.TaskID)

	var dir, sha string
	var err error
	if r.plan.Workspace == "folder" {
		dir = proj.Path
		sha, err = gitops.CheckoutNewBranch(proj.Path, branch, proj.BaseBranch)
		if err != nil {
			return fmt.Errorf("создание ветки в папке проекта: %w", err)
		}
		r.log("", "Ветка создана прямо в папке проекта: "+dir+" (база "+short(sha)+")")
	} else {
		dir = filepath.Join(filepath.Dir(proj.Path), filepath.Base(proj.Path)+"-agent-worktrees", filepath.Base(r.st.TaskDir))
		sha, err = gitops.AddWorktree(proj.Path, dir, branch, proj.BaseBranch)
		if err != nil {
			return fmt.Errorf("создание worktree: %w", err)
		}
		r.log("", "Worktree создан: "+dir+" (база "+short(sha)+")")
	}
	r.job.mu.Lock()
	r.st.WorktreeDir, r.st.BranchName, r.st.BaseCommit, r.st.RoundBase = dir, branch, sha, sha
	r.job.mu.Unlock()
	r.job.Emit("", "task_field", map[string]any{
		"worktree_dir": dir, "branch_name": branch, "base_commit": sha, "round_base": sha,
	})
	r.job.SaveState()
	if proj.PostCreate != "" {
		hctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
		defer cancel()
		cmd := shellCommand(hctx, proj.PostCreate, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("post-create hook: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// roundBase — база текущего раунда (для старых задач — база задачи).
func (r *run) roundBase() string {
	if r.st.RoundBase != "" {
		return r.st.RoundBase
	}
	return r.st.BaseCommit
}

// seedTaskMD пишет начальный task.md, если его ещё нет.
func seedTaskMD(taskDir, title, prompt, sourceURL string) {
	path := filepath.Join(taskDir, "task.md")
	if _, err := os.Stat(path); err == nil {
		return
	}
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	b.WriteString("_Компактная сводка последнего состояния решения. Обновляется каждым этапом._\n\n")
	if sourceURL != "" {
		b.WriteString("Источник: " + sourceURL + "\n\n")
	}
	b.WriteString("## Условие\n\n" + strings.TrimSpace(prompt) + "\n")
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

// Cleanup убирает рабочую копию и папку удалённой задачи. В режиме «в папке»
// рабочая копия — сам проект: её не трогаем, остаётся только фича-ветка.
func (p *Pipeline) Cleanup(job *Job) {
	st := job.State
	if st == nil {
		return
	}
	if st.WorktreeDir != "" && job.Plan.Workspace != "folder" {
		if err := gitops.RemoveWorktree(job.Plan.Project.Path, st.WorktreeDir); err != nil {
			// Папку могли убрать руками: тогда остаётся только запись в
			// .git/worktrees, и её снимает prune — иначе git считал бы
			// путь занятым при следующей таске с тем же номером.
			if _, statErr := os.Stat(st.WorktreeDir); statErr != nil {
				_ = gitops.PruneWorktrees(job.Plan.Project.Path)
			} else {
				job.Emit("", "log", map[string]any{"text": "Ошибка удаления worktree: " + err.Error()})
			}
		}
	}
	if st.TaskDir != "" {
		_ = os.RemoveAll(st.TaskDir)
	}
}
