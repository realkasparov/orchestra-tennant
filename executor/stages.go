package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/mcpserver"
	"github.com/realkasparov/orchestra-tennant/protocol"
	"github.com/realkasparov/orchestra-tennant/repomap"
)

// Этапы — перенесённые из оркестратора. Промпт этапа — установленный скилл;
// здесь в его вызов подставляются параметры из плана и локальный контекст:
// карта репозитория и дифф, которые иначе как на месте не посчитать.

// refRe — доверенная форма референса: путь GitLab (tn/core/tradernet#42546)
// или локальный запасной (task-42). Запрещает ведущие дефисы, пробелы и всё,
// что превратило бы имя ветки в опцию git или выход из пути: референс приходит
// из маркера REFERENCE агента импорта, на который влияет импортированный текст.
var refRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9/_.-]*#\d+|task-\d+)$`)

func (r *run) stageImport(ctx context.Context, st *StageState) error {
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_URL: %s\nTASK_DIR: %s", r.skill("import", "import-gitlab"), r.plan.SourceURL, r.st.TaskDir)
	text, err := r.runAgentStage(ctx, st, prompt, r.st.TaskDir, 1)
	if err != nil {
		return err
	}
	ref := agent.Reference(text)
	if ref == "" {
		return fmt.Errorf("импорт завершился без маркера REFERENCE")
	}
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "step01-import.md")); err != nil {
		return fmt.Errorf("импорт не создал step01-import.md")
	}
	r.st.Reference = ref
	fields := map[string]any{"reference": ref}
	if title := importTitle(r.st.TaskDir); title != "" {
		r.st.Title = title
		fields["title"] = title
	}
	r.job.Emit("", "task_field", fields)
	return r.finishStage(st)
}

func (r *run) stageAnalyze(ctx context.Context, st *StageState) error {
	decompose := "no"
	if r.plan.Stage("decompose") != nil {
		decompose = "yes"
	}
	// Правка: если человек оставил отзыв, это повторный прогон — строить на
	// существующем плане и уже написанном коде, а не выводить всё заново.
	revision := "no"
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "user-feedback.md")); err == nil {
		revision = "yes"
	}
	projectCtx := ""
	if r.plan.Project.Stack != "" {
		projectCtx += "\nProject stack: " + r.plan.Project.Stack
	}
	if r.plan.Project.Description != "" {
		projectCtx += "\nProject description (from the user):\n" + r.plan.Project.Description
	}
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nDECOMPOSE: %s\nREVISION: %s%s%s\n\nTask text from the user:\n%s",
		r.skill("analyze", "analyze-task"), r.st.TaskDir, decompose, revision, projectCtx, r.repoMapSection(), r.plan.Prompt)
	text, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, 1)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "step02-analyze.md")); err != nil {
		return fmt.Errorf("анализ не создал step02-analyze.md")
	}
	if slug := agent.BranchSlug(text); slug != "" {
		r.st.BranchSlug = sanitizeSlug(slug)
		r.job.Emit("", "task_field", map[string]any{"branch_slug": r.st.BranchSlug})
	}
	// Виртуальная декомпозиция: маркер no-split закрывает её; вопрос типа
	// decompose закрывается в askUser. Ни того, ни другого — ошибка.
	if ds := r.st.stage("decompose"); ds != nil {
		if agent.DecomposeRes(text) == "no-split" {
			r.setStageStatus("decompose", "done")
		} else if ds.Status != "done" {
			r.setStageStatus("decompose", "error")
			return fmt.Errorf("анализ не дал результата декомпозиции (ни DECOMPOSE_RESULT, ни вопроса)")
		}
	}
	r.applyPlanResult(agent.PlanResult(text))
	return r.finishStage(st)
}

// applyPlanResult включает/выключает execute и review текущего раунда по
// вердикту плана: no-code — этапам нечего делать, пропускаем заранее вместо
// того, чтобы жечь токены на подтверждение пустоты.
func (r *run) applyPlanResult(verdict string) {
	switch verdict {
	case "no-code":
		r.setStageStatus("execute", "skipped")
		r.setStageStatus("review", "skipped")
		r.log("", "План: изменения кода не требуются — «Выполнение» и «Ревью» пропущены.")
	case "code":
		for _, key := range []string{"execute", "review"} {
			if s := r.st.stage(key); s != nil && s.Status == "skipped" {
				r.setStageStatus(key, "pending")
			}
		}
	}
}

// repoMapSection — карта символов рабочей копии для промпта анализа. Ошибка
// построения этап не роняет: агент просто осмотрится сам.
func (r *run) repoMapSection() string {
	if r.st.WorktreeDir == "" {
		return ""
	}
	rm, err := repomap.Build(r.st.WorktreeDir)
	if err != nil {
		r.log("analyze", "Карта репозитория не построена: "+err.Error())
		return ""
	}
	rendered := rm.Render(repomap.DefaultBudget)
	if rendered == "" {
		return ""
	}
	r.log("analyze", fmt.Sprintf("Карта репозитория: %d файлов с кодом, %d КБ в промпт анализа.", rm.TotalFiles, len(rendered)>>10))
	return "\n\n" + rendered
}

// planSection — секция с текстом актуального плана. Большой план (>24К)
// агент прочитает сам.
func planSection(taskDir string) string {
	for _, name := range []string{"step03-refined-plan.md", "step02-analyze.md"} {
		data, err := os.ReadFile(filepath.Join(taskDir, name))
		if err != nil {
			continue
		}
		if len(data) > 24<<10 {
			return ""
		}
		return fmt.Sprintf("\n\nPLAN (current contents of %s, embedded for convenience — no need to Read it; edits still go to the file):\n<<<PLAN\n%s\nPLAN>>>", name, data)
	}
	return ""
}

// diffSection — готовый дифф ветки для ревью: список файлов всегда, патчи —
// пока укладываются в бюджет промпта.
func diffSection(worktree, base string) string {
	files, err := gitops.Diff(worktree, base)
	if err != nil || len(files) == 0 {
		return ""
	}
	const patchBudget = 60 << 10
	total := 0
	for _, f := range files {
		total += len(f.Patch)
	}
	var sb strings.Builder
	sb.WriteString("\n\nDIFF (already computed against BASE_COMMIT — use it instead of running git diff")
	if total == 0 || total > patchBudget {
		sb.WriteString("; patches omitted for size, run git diff for file details):\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "%s\t%s\t+%d -%d\n", f.Status, f.Path, f.Additions, f.Deletions)
		}
		return sb.String()
	}
	sb.WriteString("):\n<<<DIFF\n")
	for _, f := range files {
		fmt.Fprintf(&sb, "--- %s [%s] +%d -%d\n", f.Path, f.Status, f.Additions, f.Deletions)
		if f.Patch != "" {
			sb.WriteString(f.Patch)
			if !strings.HasSuffix(f.Patch, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	sb.WriteString("DIFF>>>")
	return sb.String()
}

func (r *run) stageErrWork(ctx context.Context, st *StageState) error {
	n := 2
	if def := r.plan.Stage("err_work"); def != nil && def.Passes > 0 {
		n = def.Passes
	}
	var start int
	switch {
	case st.CurrentPass == 0:
		start = 1
	case st.Status == "paused" || st.Status == "waiting_user":
		start = st.CurrentPass
	default: // error → переделать упавший прогон свежей сессией
		start = st.CurrentPass
		st.SessionID = ""
		st.Status = "pending"
	}
	finalVerdict := ""
	for pass := start; pass <= n; pass++ {
		if pass != st.CurrentPass {
			st.SessionID = ""
			st.Status = "pending"
		}
		r.log(st.Key, fmt.Sprintf("Работа над ошибками: прогон %d из %d", pass, n))
		scope := ""
		if pass > 1 {
			scope = "\nSCOPE: delta — проверяй правки прошлого прогона и изменённые секции плана, не повторяй полную проверку (см. Pass scope в скилле)."
		}
		prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nPASS_NUMBER: %d%s%s",
			r.skill("err_work", "plan-review"), r.st.TaskDir, pass, scope, planSection(r.st.TaskDir))
		text, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, pass)
		if err != nil {
			return err
		}
		plan, rerr := os.ReadFile(filepath.Join(r.st.TaskDir, "step03-refined-plan.md"))
		if rerr != nil || !strings.Contains(string(plan), fmt.Sprintf("Pass %d", pass)) {
			return fmt.Errorf("прогон %d не создал/не дополнил step03-refined-plan.md записью Self-review", pass)
		}
		if v := agent.PlanResult(text); v != "" {
			finalVerdict = v
		}
		// Ранний выход: чистый прогон означает, что план сошёлся с кодом.
		if pass < n && agent.PlanReview(text) == "clean" {
			r.log(st.Key, fmt.Sprintf("Ревью плана: прогон %d без замечаний — оставшиеся прогоны (%d) пропущены.", pass, n-pass))
			break
		}
	}
	r.applyPlanResult(finalVerdict)
	return r.finishStage(st)
}

// toolsFor — список инструментов этапа с учётом непрерывности: в non_stop всё
// без ограничений, в per_stage этапы с недоверенным содержимым получают
// ограниченный список без Bash.
func toolsFor(continuity, stageKey string) []string {
	if continuity == "non_stop" {
		return nil
	}
	switch stageKey {
	case "import":
		// Префиксный allowlist здесь не работает: матчер прав Claude Code
		// отвергает форму команд glab с пайпами и подстановками независимо от
		// разрешённого бинаря. Импорт идёт без ограничений, а его
		// безопасность — правила скилла про недоверенное содержимое.
		return nil
	case "analyze", "err_work":
		return []string{"Read", "Write", "Edit", "Glob", "Grep", "Task", "WebFetch"}
	default:
		return nil
	}
}

// withTools добавляет инструменты к списку этапа. Пустой список — «без
// ограничений», добавлять нечего.
func withTools(base, extra []string) []string {
	if len(base) == 0 || len(extra) == 0 {
		return base
	}
	return append(append([]string(nil), base...), extra...)
}

// stageBranch — детерминированный этап без агента.
func (r *run) stageBranch(st *StageState) error {
	st.Status = "running"
	r.emitStage(st, "running")
	ref := r.st.Reference
	if !refRe.MatchString(ref) {
		if ref != "" {
			r.log(st.Key, fmt.Sprintf("Референс %q не прошёл валидацию — использую task-%d", ref, r.plan.TaskID))
		}
		ref = fmt.Sprintf("task-%d", r.plan.TaskID)
		r.st.Reference = ref
		r.job.Emit("", "task_field", map[string]any{"reference": ref})
	}
	slug := r.st.BranchSlug
	if slug == "" {
		slug = sanitizeSlug(slugify(r.st.Title))
		if slug == "" {
			slug = "task"
		}
	}
	final := ref + "-" + slug
	if r.st.BranchName == final {
		return r.finishStage(st)
	}
	if err := gitops.RenameBranch(r.st.WorktreeDir, r.st.BranchName, final); err != nil {
		return fmt.Errorf("переименование ветки: %w", err)
	}
	r.st.BranchName = final
	r.job.Emit("", "task_field", map[string]any{"branch_name": final})
	r.log(st.Key, "Ветка: "+final)
	return r.finishStage(st)
}

func (r *run) stageExecute(ctx context.Context, st *StageState) error {
	title := humanize(r.st.BranchSlug)
	if title == "" {
		title = r.st.Title
	}
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nREFERENCE: %s\nTITLE: %s\nBASE: %s%s",
		r.skill("execute", "execute-plan"), r.st.TaskDir, r.st.Reference, title, r.st.BaseCommit, planSection(r.st.TaskDir))
	if _, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, 1); err != nil {
		return err
	}
	head, err := gitops.HeadSHA(r.st.WorktreeDir)
	if err != nil {
		return err
	}
	// База раунда, а не задачи: в раунде 2+ HEAD уже отличается от базы
	// задачи, и пустой раунд прошёл бы проверку.
	if head == r.roundBase() {
		return fmt.Errorf("выполнение не создало ни одного коммита")
	}
	clean, err := gitops.IsClean(r.st.WorktreeDir)
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("после выполнения рабочее дерево не чистое")
	}
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "step04-execution.md")); err != nil {
		return fmt.Errorf("выполнение не создало step04-execution.md")
	}
	if err := r.testGate(ctx, st); err != nil {
		return err
	}
	r.reindexChanged(head)
	r.emitDiff()
	return r.finishStage(st)
}

// reindexChanged ставит изменённые этапом файлы в очередь переиндексации —
// асинхронно и мягко: отставание индекса не блокирует пайплайн.
func (r *run) reindexChanged(head string) {
	if r.p.Index == nil || r.st.WorktreeDir == "" {
		return
	}
	files, err := gitops.ChangedFiles(r.st.WorktreeDir, r.roundBase(), head)
	if err != nil || len(files) == 0 {
		return
	}
	r.p.Index.EnqueueChanged(r.plan.Project.ID, files)
}

func (r *run) stageReview(ctx context.Context, st *StageState) error {
	if head, herr := gitops.HeadSHA(r.st.WorktreeDir); herr == nil && head == r.roundBase() {
		r.log(st.Key, "Ревью: новых коммитов в этом раунде нет — этап пропущен.")
		st.Status = "skipped"
		r.emitStage(st, "skipped")
		return nil
	}
	mode := "report-only"
	if r.plan.Continuity == "non_stop" {
		mode = "autofix"
	}
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nREFERENCE: %s\nBASE_COMMIT: %s\nMODE: %s%s",
		r.skill("review", "review-task"), r.st.TaskDir, r.st.Reference, r.st.BaseCommit, mode, diffSection(r.st.WorktreeDir, r.st.BaseCommit))
	if _, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, 1); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "step05-review.md")); err != nil {
		return fmt.Errorf("ревью не создало step05-review.md")
	}
	r.emitDiff()
	return r.finishStage(st)
}

// stageHandoff — последний этап: инструкция, как запустить, установить или
// проверить результат. Код не трогает; пишет step06-handoff.md. Самая
// частая беда после готовой таски — человек смотрит на старую сборку, и
// этап существует ровно затем, чтобы сказать, что пересобрать.
func (r *run) stageHandoff(ctx context.Context, st *StageState) error {
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nREFERENCE: %s\nBRANCH: %s\nBASE_COMMIT: %s\nWORKSPACE: %s\nWORKTREE_DIR: %s\nTEST_CMD: %s",
		r.skill("handoff", "handoff-notes"), r.st.TaskDir, r.st.Reference, r.st.BranchName, r.st.BaseCommit,
		r.plan.Workspace, r.st.WorktreeDir, r.plan.Project.TestCmd)
	if _, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, 1); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.st.TaskDir, "step06-handoff.md")); err != nil {
		return fmt.Errorf("этап не создал step06-handoff.md")
	}
	return r.finishStage(st)
}

// skill — имя скилла этапа из плана; запасное — если план старой схемы его
// не назвал.
func (r *run) skill(key, fallback string) string {
	if def := r.plan.Stage(key); def != nil && def.Skill != "" {
		return def.Skill
	}
	return fallback
}

// --- тест-гейт ---

const gateTimeout = 10 * time.Minute

func runTestGate(ctx context.Context, cmd, dir string) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	c := exec.CommandContext(gctx, "/bin/sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	tail := string(out)
	if len(tail) > 4000 {
		tail = "…" + tail[len(tail)-4000:]
	}
	if gctx.Err() == context.DeadlineExceeded {
		return tail, fmt.Errorf("тест-гейт превысил лимит времени (%s)", gateTimeout)
	}
	return tail, err
}

// testGate — команда тестов проекта после execute. Падение → одна попытка
// автопочинки той же сессией, затем повторный прогон.
func (r *run) testGate(ctx context.Context, st *StageState) error {
	cmd := strings.TrimSpace(r.plan.Project.TestCmd)
	if cmd == "" {
		return nil
	}
	r.log(st.Key, "Тест-гейт: "+cmd)
	out, gerr := runTestGate(ctx, cmd, r.st.WorktreeDir)
	if gerr == nil {
		r.log(st.Key, "Тест-гейт пройден ✓")
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.log(st.Key, "Тест-гейт упал — одна попытка автопочинки.\n"+out)
	prompt := fmt.Sprintf("Команда тестов проекта упала после твоей реализации.\nКоманда: %s\nВывод (хвост):\n%s\n\n"+
		"Почини причину падения, прогони команду сам до зелёного статуса и закоммить правку "+
		"(commit message: %s: fix tests). Не отключай и не ослабляй сами тесты без веской причины — если тест "+
		"устарел по сути задачи, объясни это в step04-execution.md.", cmd, out, r.st.Reference)
	if _, err := r.runAgentStage(ctx, st, prompt, r.st.WorktreeDir, st.CurrentPass); err != nil {
		return err
	}
	out2, gerr2 := runTestGate(ctx, cmd, r.st.WorktreeDir)
	if gerr2 != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("тест-гейт не прошёл после автопочинки: %s\n%s", gerr2, out2)
	}
	r.log(st.Key, "Тест-гейт пройден после починки ✓")
	return nil
}

// --- поиск по коду ---

// codeSearchStages — этапы, которым семантический поиск полезен. Ревью плана
// и кода его не получают намеренно: они проверяют конкретные утверждения, и им
// достаточно grep и вложенного диффа.
var codeSearchStages = map[string]bool{"analyze": true, "execute": true}

// codeSearchFor — конфигурация MCP и дополнительные инструменты этапа. MCP-
// сервер — тот же бинарник; он открывает индекс с диска напрямую: ходить за
// поиском в HTTP API оркестратора ему нечем — сессии у него нет.
func (p *Pipeline) codeSearchFor(plan *protocol.Plan, stageKey string) (string, []string) {
	if p.Index == nil || !codeSearchStages[stageKey] {
		return "", nil
	}
	if st := p.Index.Status(plan.Project.ID); st == nil || !st.Enabled {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	cfg := map[string]any{"mcpServers": map[string]any{
		mcpserver.ServerName: map[string]any{
			"command": exe,
			"args": []string{
				"-mcp-codesearch",
				"-mcp-data", p.DataDir,
				"-mcp-project", strconv.FormatInt(plan.Project.ID, 10),
				"-mcp-root", plan.Project.Path,
			},
		},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", nil
	}
	return string(raw), []string{mcpserver.ToolName}
}

// --- разное ---

// importTitle — заголовок из первой строки step01-import.md.
func importTitle(taskDir string) string {
	data, err := os.ReadFile(filepath.Join(taskDir, "step01-import.md"))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimPrefix(strings.TrimSpace(line), "# ")
	for _, sep := range []string{" — ", " - "} {
		if _, after, ok := strings.Cut(line, sep); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(line)
}

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o",
	'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts",
	'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func slugify(title string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			sb.WriteRune(r)
		case translit[r] != "":
			sb.WriteString(translit[r])
		default:
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

func sanitizeSlug(s string) string {
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	if len(parts) > 5 {
		parts = parts[:5]
	}
	return strings.Join(parts, "-")
}

func humanize(slug string) string {
	s := strings.ReplaceAll(slug, "-", " ")
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
