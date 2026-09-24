package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Роды шагов. Движок исполняет любой список шагов плана: что делать, говорит
// род; что подставить в промпт — привязки плана и манифест скилла; что
// проверить после — проверки манифеста; что сделать с ответом — реакции.

// --- start.task ---

// stepStart — стартовый шаг со ссылкой на импорт: скилл импорта забирает
// задачу в папку и называет референс. Без ссылки строки этапа у шага нет, и
// сюда он не попадает; его выходы (текст, название) заводит prepare через
// resolve.
func (r *run) stepStart(ctx context.Context, st *StageState, def *protocol.Step) error {
	sp := r.specFor(def)
	sp.cwd = r.st.TaskDir
	prompt := fmt.Sprintf("Use the %s skill.\nTASK_DIR: %s\nTASK_URL: %s", def.Skill, r.st.TaskDir, r.plan.SourceURL)
	text, err := r.runAgentStage(ctx, st, sp, prompt, 1)
	if err != nil {
		return err
	}
	if err := r.runChecks(def, text, false); err != nil {
		return err
	}
	ref := agent.LastMarker(text, "REFERENCE:")
	if ref == "" {
		return fmt.Errorf("импорт завершился без маркера REFERENCE")
	}
	r.st.Reference = ref
	r.st.setOutput(def.Key, "reference", ref)
	fields := map[string]any{"reference": ref}
	if title := importTitle(r.st.TaskDir); title != "" {
		r.st.Title = title
		r.st.setOutput(def.Key, "title", title)
		fields["title"] = title
	}
	r.job.Emit("", "task_field", fields)
	return r.finishStage(st)
}

// --- agent ---

// stepAgent — агентный шаг: промпт по привязкам и манифесту, прогоны с
// ранней остановкой, маркеры, проверки, реакции.
func (r *run) stepAgent(ctx context.Context, st *StageState, def *protocol.Step) error {
	sp := r.specFor(def)
	m := r.manifest(def.Skill)
	// Ревью без новых коммитов в раунде — нечего смотреть: шаг с контекстом
	// диффа пропускается, как раньше пропускалось ревью.
	if m != nil && hasContext(m, "diff") && r.st.WorktreeDir != "" && r.st.BaseCommit != "" {
		if head, herr := gitops.HeadSHA(r.st.WorktreeDir); herr == nil && head == r.roundBase() {
			r.log(st.Key, "Новых коммитов в этом раунде нет — этап пропущен.")
			st.Status = "skipped"
			r.emitStage(st, "skipped")
			return nil
		}
	}
	n := def.Passes
	if n < 1 {
		n = 1
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
	if start > n {
		start = n
	}
	lastText := ""
	for pass := start; pass <= n; pass++ {
		if pass != st.CurrentPass {
			st.SessionID = ""
			st.Status = "pending"
		}
		if n > 1 {
			r.log(st.Key, fmt.Sprintf("Прогон %d из %d", pass, n))
		}
		r.pass = pass
		r.passScope = ""
		if pass > 1 {
			r.passScope = "delta — проверяй правки прошлого прогона и изменённые секции плана, не повторяй полную проверку (см. Pass scope в скилле)."
		}
		prompt, err := r.buildPrompt(def, m)
		if err != nil {
			return err
		}
		text, err := r.runAgentStage(ctx, st, sp, prompt, pass)
		if err != nil {
			return err
		}
		lastText = text
		if err := r.runChecks(def, text, true); err != nil {
			return err
		}
		stop, err := r.applyMarkers(def, m, text)
		if err != nil {
			return err
		}
		if stop && pass < n {
			r.log(st.Key, fmt.Sprintf("Прогон %d без замечаний — оставшиеся прогоны (%d) пропущены.", pass, n-pass))
			break
		}
	}
	_ = lastText
	if r.st.WorktreeDir != "" && r.st.BranchName != "" {
		if head, herr := gitops.HeadSHA(r.st.WorktreeDir); herr == nil {
			r.reindexChanged(head)
		}
		r.emitDiff()
	}
	return r.finishStage(st)
}

func hasContext(m *protocol.Manifest, name string) bool {
	for _, c := range m.Context {
		if c == name {
			return true
		}
	}
	return false
}

// buildPrompt собирает промпт шага: вызов скилла, TASK_DIR, привязанные
// входы в порядке манифеста (короткие — строкой, многострочные — секцией
// после), затем контекст манифеста.
func (r *run) buildPrompt(def *protocol.Step, m *protocol.Manifest) (string, error) {
	var head, sections strings.Builder
	fmt.Fprintf(&head, "Use the %s skill.\nTASK_DIR: %s", def.Skill, r.st.TaskDir)
	order := make([]string, 0, len(def.Bind))
	if m != nil {
		for _, name := range m.InputOrder {
			if _, ok := def.Bind[name]; ok {
				order = append(order, name)
			}
		}
	} else {
		for name := range def.Bind {
			order = append(order, name)
		}
		sortStrings(order)
	}
	for _, name := range order {
		val, err := r.resolve(def.Bind[name], m, name)
		if err != nil {
			return "", fmt.Errorf("вход %s: %w", name, err)
		}
		required := m != nil && m.Inputs[name].Required
		if val == "" && !required {
			continue
		}
		if strings.Contains(val, "\n") || len(val) > 200 {
			fmt.Fprintf(&sections, "\n\n%s:\n%s", name, strings.TrimRight(val, "\n"))
			continue
		}
		fmt.Fprintf(&head, "\n%s: %s", name, val)
	}
	if m != nil {
		for _, c := range []string{"repo_map", "plan", "files", "diff"} {
			if !hasContext(m, c) {
				continue
			}
			switch c {
			case "repo_map":
				sections.WriteString(r.repoMapSection(def.Key))
			case "plan":
				sections.WriteString(planSection(r.st.TaskDir))
			case "files":
				sections.WriteString(filesSection(r.st.WorktreeDir, planText(r.st.TaskDir)))
			case "diff":
				sections.WriteString(diffSection(r.st.WorktreeDir, r.st.BaseCommit))
			}
		}
	}
	return head.String() + sections.String(), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// resolve — значение привязки: литерал или ссылка `$task.*`, `$project.*`,
// `$plan.*`, `$steps.<ключ>.<выход>`. Вход типа text, привязанный к
// артефакту, получает содержимое файла; остальные — путь.
func (r *run) resolve(src string, m *protocol.Manifest, input string) (string, error) {
	scope, name, out, ok := protocol.ParseRef(src)
	if !ok {
		return src, nil
	}
	switch scope {
	case "task":
		return r.taskRef(name), nil
	case "project":
		p := r.plan.Project
		switch name {
		case "name":
			return p.Name, nil
		case "path":
			return p.Path, nil
		case "base_branch":
			return p.BaseBranch, nil
		case "test_cmd":
			return p.TestCmd, nil
		case "stack":
			return p.Stack, nil
		case "description":
			return p.Description, nil
		}
	case "plan":
		switch name {
		case "workspace":
			if r.st.SelfWorkspace {
				return "folder", nil
			}
			return r.plan.Workspace, nil
		case "continuity":
			return r.plan.Continuity, nil
		case "review_mode":
			if r.plan.Continuity == "non_stop" {
				return "autofix", nil
			}
			return "report-only", nil
		case "decompose":
			return "no", nil
		}
	case "steps":
		val := r.st.output(name, out)
		if val == "" {
			return "", nil
		}
		// Артефакт: путь для path, содержимое для text.
		if strings.HasPrefix(val, r.st.TaskDir+string(filepath.Separator)) {
			if m != nil && m.Inputs[input].Type == "text" {
				data, err := os.ReadFile(val)
				if err != nil {
					return "", err
				}
				return string(data), nil
			}
		}
		return val, nil
	}
	return "", fmt.Errorf("ссылка %s не разобрана", src)
}

// taskRef — поля таски для привязок.
func (r *run) taskRef(name string) string {
	switch name {
	case "dir":
		return r.st.TaskDir
	case "prompt":
		return r.plan.Prompt
	case "title":
		return r.st.Title
	case "branch_title":
		if t := humanize(r.st.BranchSlug); t != "" {
			return t
		}
		return r.st.Title
	case "reference":
		return r.st.Reference
	case "url":
		return r.plan.SourceURL
	case "branch_name":
		return r.st.BranchName
	case "base_commit":
		return r.st.BaseCommit
	case "round_base":
		return r.roundBase()
	case "worktree_dir":
		return r.st.WorktreeDir
	case "feedback":
		return r.st.Feedback
	case "revision":
		if _, err := os.Stat(filepath.Join(r.st.TaskDir, "user-feedback.md")); err == nil {
			return "yes"
		}
		return "no"
	case "previous_round":
		if _, err := os.Stat(filepath.Join(r.st.TaskDir, "user-feedback.md")); err != nil {
			return ""
		}
		return roundDir(r.st.TaskDir, r.st.round()-1)
	case "pass":
		return strconv.Itoa(max(r.pass, 1))
	case "pass_scope":
		return r.passScope
	case "question":
		return r.question
	}
	return ""
}

// applyMarkers разбирает маркеры манифеста: проверяет значения, запоминает
// выходы, пишет поля таски (emit) и применяет реакции (on). Возвращает
// true, если реакция велела остановить прогоны.
func (r *run) applyMarkers(def *protocol.Step, m *protocol.Manifest, text string) (bool, error) {
	if m == nil {
		return false, nil
	}
	stop := false
	for _, name := range m.Outputs.MarkerOrder {
		mk := m.Outputs.Markers[name]
		val := agent.LastMarker(text, name+":")
		if val == "" {
			continue
		}
		if mk.Type == "enum" {
			okv := false
			for _, v := range mk.Values {
				if v == val {
					okv = true
				}
			}
			if !okv {
				return false, fmt.Errorf("маркер %s: недопустимое значение %s (ожидается %s)", name, val, strings.Join(mk.Values, " | "))
			}
		}
		if mk.Type == "git_branch" {
			if err := gitops.CheckRef(val); err != nil {
				return false, fmt.Errorf("маркер %s: %w", name, err)
			}
			if r.st.SelfWorkspace && r.st.BranchName != val {
				r.st.BranchName = val
				r.job.Emit("", "task_field", map[string]any{"branch_name": val})
				r.log(def.Key, "Ветка: "+val)
			}
		}
		r.st.setOutput(def.Key, name, val)
		if field, ok := def.Emit[name]; ok {
			r.emitField(def.Key, field, val)
		}
		if re, ok := def.On[name][val]; ok {
			for _, k := range re.Skip {
				if st := r.st.stage(k); st != nil && st.Status == "pending" {
					r.setStageStatus(k, "skipped")
				}
			}
			if len(re.Skip) > 0 {
				r.log(def.Key, fmt.Sprintf("%s: %s — пропущены шаги: %s", name, val, r.titlesOf(re.Skip)))
			}
			if re.StopPasses {
				stop = true
			}
		}
	}
	for _, a := range m.Outputs.Artifacts {
		path := filepath.Join(r.st.TaskDir, a.Name)
		if _, err := os.Stat(path); err == nil {
			r.st.setOutput(def.Key, a.Name, path)
		}
	}
	return stop, nil
}

// emitField пишет поле таски из маркера.
func (r *run) emitField(stepKey, field, val string) {
	switch field {
	case "reference":
		r.st.Reference = val
	case "title":
		r.st.Title = val
	case "branch_slug":
		val = sanitizeSlug(val)
		r.st.BranchSlug = val
	case "branch_name":
		r.st.BranchName = val
	default:
		return
	}
	r.job.Emit("", "task_field", map[string]any{field: val})
}

func (r *run) titlesOf(keys []string) string {
	var out []string
	for _, k := range keys {
		if s := r.plan.Step(k); s != nil {
			out = append(out, "«"+s.Title+"»")
		} else {
			out = append(out, k)
		}
	}
	return strings.Join(out, ", ")
}

// runChecks — пост-проверки манифеста после прогона.
func (r *run) runChecks(def *protocol.Step, text string, gitChecks bool) error {
	m := r.manifest(def.Skill)
	if m == nil {
		return nil
	}
	for _, c := range m.Checks {
		switch c.Kind {
		case "artifact_exists":
			if _, err := os.Stat(filepath.Join(r.st.TaskDir, c.Arg)); err != nil {
				return fmt.Errorf("%s не создал %s", def.Title, c.Arg)
			}
		case "json_valid":
			a := m.Artifact(c.Arg)
			data, err := os.ReadFile(filepath.Join(r.st.TaskDir, c.Arg))
			if err != nil {
				return fmt.Errorf("%s не создал %s", def.Title, c.Arg)
			}
			if a != nil && a.Schema != nil {
				if err := protocol.ValidateJSON(data, a.Schema); err != nil {
					return fmt.Errorf("%s: %s не по схеме: %v", def.Title, c.Arg, err)
				}
			}
		case "marker_present":
			if agent.LastMarker(text, c.Arg+":") == "" {
				return fmt.Errorf("%s завершился без маркера %s", def.Title, c.Arg)
			}
		case "has_commits":
			if !gitChecks || r.st.WorktreeDir == "" {
				continue
			}
			if c.Arg != "" {
				branch := agent.LastMarker(text, c.Arg+":")
				if branch == "" {
					return fmt.Errorf("%s завершился без маркера %s", def.Title, c.Arg)
				}
				n, err := gitops.CountCommits(r.st.WorktreeDir, r.roundBase(), branch)
				if err != nil {
					return fmt.Errorf("ветка %s: %w", branch, err)
				}
				if n == 0 {
					return fmt.Errorf("в ветке %s нет ни одного коммита сверх базы", branch)
				}
				continue
			}
			head, err := gitops.HeadSHA(r.st.WorktreeDir)
			if err != nil {
				return err
			}
			if head == r.roundBase() {
				return fmt.Errorf("%s не создал ни одного коммита", def.Title)
			}
		case "clean_tree":
			if !gitChecks || r.st.WorktreeDir == "" {
				continue
			}
			dirty, err := gitops.DirtyFiles(r.st.WorktreeDir)
			if err != nil {
				return err
			}
			if len(dirty) > 0 {
				return fmt.Errorf("после шага «%s» рабочее дерево не чистое: %s", def.Title, strings.Join(dirty, ", "))
			}
		}
	}
	return nil
}

// --- action.branch ---

// stepBranch — детерминированный шаг без агента: переименовать рабочую ветку
// в <референс>-<описание>.
func (r *run) stepBranch(st *StageState) error {
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
	if r.st.BranchName == final || strings.HasPrefix(r.st.BranchName, final+"-") {
		r.st.setOutput(st.Key, "branch", r.st.BranchName)
		return r.finishStage(st)
	}
	if r.st.WorktreeDir == "" || r.st.BranchName == "" {
		return fmt.Errorf("нечего переименовывать: рабочая копия ещё не создана")
	}
	for i := 2; gitops.HasRef(r.st.WorktreeDir, final); i++ {
		final = fmt.Sprintf("%s-%s-%d", ref, slug, i)
	}
	if err := gitops.RenameBranch(r.st.WorktreeDir, r.st.BranchName, final); err != nil {
		return fmt.Errorf("переименование ветки: %w", err)
	}
	r.st.BranchName = final
	r.st.setOutput(st.Key, "branch", final)
	r.job.Emit("", "task_field", map[string]any{"branch_name": final})
	r.log(st.Key, "Ветка: "+final)
	return r.finishStage(st)
}

// --- action.test_gate ---

// stepTestGate — команда тестов проекта после реализации: падение чинится
// один раз сессией шага из fix_with, затем прогон повторяется.
func (r *run) stepTestGate(ctx context.Context, st *StageState, def *protocol.Step) error {
	cmd := strings.TrimSpace(r.plan.Project.TestCmd)
	if cmd == "" || r.st.WorktreeDir == "" {
		st.Status = "skipped"
		r.emitStage(st, "skipped")
		return nil
	}
	st.Status = "running"
	r.emitStage(st, "running")
	r.log(st.Key, "Тест-гейт: "+cmd)
	out, gerr := runTestGate(ctx, cmd, r.st.WorktreeDir)
	if gerr == nil {
		r.log(st.Key, "Тест-гейт пройден ✓")
		return r.finishStage(st)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fixDef := r.plan.Step(def.FixWith)
	fixSt := r.st.stage(def.FixWith)
	if fixDef == nil || fixSt == nil || fixSt.SessionID == "" {
		return fmt.Errorf("тест-гейт не прошёл: %s\n%s", gerr, out)
	}
	r.log(st.Key, "Тест-гейт упал — одна попытка автопочинки.\n"+out)
	prompt := fmt.Sprintf("Команда тестов проекта упала после твоей реализации.\nКоманда: %s\nВывод (хвост):\n%s\n\n"+
		"Почини причину падения, прогони команду сам до зелёного статуса и закоммить правку "+
		"(commit message: %s: fix tests). Не отключай и не ослабляй сами тесты без веской причины — если тест "+
		"устарел по сути задачи, объясни это в step04-execution.md.", cmd, out, r.st.Reference)
	sp := r.specFor(fixDef)
	if _, err := r.runAgentSession(ctx, fixSt, sp, prompt, fixSt.SessionID, fixSt.CurrentPass); err != nil {
		return err
	}
	if dirty, derr := gitops.DirtyFiles(r.st.WorktreeDir); derr != nil {
		return derr
	} else if len(dirty) > 0 {
		return fmt.Errorf("после починки тестов рабочее дерево не чистое: %s", strings.Join(dirty, ", "))
	}
	out2, gerr2 := runTestGate(ctx, cmd, r.st.WorktreeDir)
	if gerr2 != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("тест-гейт не прошёл после автопочинки: %s\n%s", gerr2, out2)
	}
	r.log(st.Key, "Тест-гейт пройден после починки ✓")
	if head, herr := gitops.HeadSHA(r.st.WorktreeDir); herr == nil {
		r.reindexChanged(head)
	}
	r.emitDiff()
	return r.finishStage(st)
}

// reindexChanged ставит изменённые файлы в очередь переиндексации.
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
