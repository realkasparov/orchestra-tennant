package executor

import (
	"fmt"
	"strings"

	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Журнал таски человеческими словами. Оркестратор показывает события в
// чате таски; тот же смысл должен читаться и в agent.log на машине —
// строка начинается с проекта и таски, дальше то, что произошло. По этому
// префиксу `orchestra-tennant log` фильтрует журнал.

// TaskLogPrefix — начало строки журнала о таске: по нему команда log
// отбирает строки проекта и таски.
func TaskLogPrefix(project string, taskID int64) string {
	return fmt.Sprintf("Project %s · Task %d · ", project, taskID)
}

var stageTitles = map[string]string{
	"import": "Импорт задачи", "analyze": "Анализ задачи", "decompose": "Декомпозиция",
	"err_work": "Работа над ошибками", "branch": "Создание ветки", "execute": "Выполнение",
	"review": "Ревью", "handoff": "Инструкция по проверке", "answer": "Ответ на запрос",
}

func stageTitle(key string) string {
	if t, ok := stageTitles[key]; ok {
		return t
	}
	return key
}

var statusTitles = map[string]string{
	"queued": "в очереди", "running": "выполняется", "waiting_user": "ждёт ответа",
	"paused": "остановлена", "done": "готово", "error": "ошибка", "pending": "ожидает",
	"skipped": "пропущен", "draft": "черновик",
}

func statusTitle(s string) string {
	if t, ok := statusTitles[s]; ok {
		return t
	}
	return s
}

// oneLine — первая строка текста, обрезанная до max рун: в журнале
// достаточно начала, полный текст есть в чате таски.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// describeEvent — событие таски одной строкой; пусто — событие в журнале
// не нужно (шум вроде результатов инструментов).
func describeEvent(ev *protocol.Event) string {
	p := ev.Payload
	stage := ""
	if ev.Stage != "" {
		stage = stageTitle(ev.Stage) + ": "
	}
	switch ev.Type {
	case "task_status":
		return "Таска: " + statusTitle(str(p, "status"))
	case "log":
		// Продолжения многострочного текста (вывод тестов) — с отступом:
		// строка без префикса таски выпала бы из фильтра log -project.
		return stage + strings.ReplaceAll(strings.TrimRight(str(p, "text"), "\n"), "\n", "\n    ")
	case "stage_status":
		if _, hasUsage := p["usage"]; hasUsage {
			return "" // расход — в чате; журналу хватает статусов
		}
		st := str(p, "status")
		round := ""
		if r, ok := p["round"].(float64); ok && r > 1 {
			round = fmt.Sprintf(" (раунд %d)", int(r))
		} else if r, ok := p["round"].(int); ok && r > 1 {
			round = fmt.Sprintf(" (раунд %d)", r)
		}
		return fmt.Sprintf("Этап «%s»%s — %s", stageTitle(str(p, "key")), round, statusTitle(st))
	case "agent_text":
		return stage + "Агент: " + oneLine(str(p, "text"), 200)
	case "tool_use":
		return stage + "Инструмент " + str(p, "name") + ": " + oneLine(str(p, "summary"), 160)
	case "tool_result":
		if str(p, "summary") == "error" {
			return stage + "Инструмент завершился ошибкой"
		}
		return ""
	case "question":
		return stage + "Вопрос агента: " + oneLine(str(p, "question"), 200)
	case "task_field":
		var parts []string
		for _, k := range []string{"branch_name", "reference", "title", "worktree_dir"} {
			if v := str(p, k); v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "Поля таски: " + strings.Join(parts, ", ")
	case protocol.EventArtifact:
		return "" // файлы шагов уходят после каждого этапа — в журнале это шум
	case protocol.EventDiff:
		if files, ok := p["files"].([]gitops.FileDiff); ok {
			return fmt.Sprintf("Дифф ветки: файлов — %d", len(files))
		}
		return "Дифф ветки обновлён"
	}
	return ""
}

// taskLog пишет событие таски в журнал демона человеческой строкой.
func (e *Executor) taskLog(j *Job, ev *protocol.Event) {
	if j == nil || j.Plan == nil {
		return
	}
	line := describeEvent(ev)
	if line == "" {
		return
	}
	e.logf("%s%s", TaskLogPrefix(j.Plan.Project.Name, j.Plan.TaskID), line)
}

// taskNote — строка журнала о таске не из события: принято, итог.
func (e *Executor) taskNote(j *Job, format string, args ...any) {
	if j == nil || j.Plan == nil {
		return
	}
	e.logf("%s%s", TaskLogPrefix(j.Plan.Project.Name, j.Plan.TaskID), fmt.Sprintf(format, args...))
}
