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
	"syscall"
	"time"

	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/mcpserver"
	"github.com/realkasparov/orchestra-tennant/protocol"
	"github.com/realkasparov/orchestra-tennant/repomap"
)

// Вспомогательное для шагов: провайдеры контекста (карта репозитория, план,
// файлы плана, дифф), тест-гейт, поиск по коду, имена веток.

// refRe — доверенная форма референса: путь GitLab (tn/core/tradernet#42546)
// или локальный запасной (task-42). Запрещает ведущие дефисы, пробелы и всё,
// что превратило бы имя ветки в опцию git или выход из пути: референс приходит
// из маркера REFERENCE агента импорта, на который влияет импортированный текст.
var refRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9/_.-]*#\d+|task-\d+)$`)

// repoMapSection — карта символов рабочей копии для промпта анализа. Ошибка
// построения этап не роняет: агент просто осмотрится сам.
func (r *run) repoMapSection(stage string) string {
	if r.st.WorktreeDir == "" {
		return ""
	}
	rm, err := repomap.Build(r.st.WorktreeDir)
	if err != nil {
		r.log(stage, "Карта репозитория не построена: "+err.Error())
		return ""
	}
	rendered := rm.Render(repomap.DefaultBudget)
	if rendered == "" {
		return ""
	}
	r.log(stage, fmt.Sprintf("Карта репозитория: %d файлов с кодом, %d КБ в промпт анализа.", rm.TotalFiles, len(rendered)>>10))
	return "\n\n" + rendered
}

// archiveRound переносит артефакты раунда n (файлы names) в
// TASK_DIR/round<n>/: этапы нового раунда должны видеть свои файлы, а не
// прошлогодний план.
func archiveRound(taskDir string, n int, names []string) error {
	dir := filepath.Join(taskDir, fmt.Sprintf("round%d", n))
	for _, name := range names {
		src := filepath.Join(taskDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// roundDir — папка артефактов раунда n, если она есть.
func roundDir(taskDir string, n int) string {
	if n < 1 {
		return ""
	}
	dir := filepath.Join(taskDir, fmt.Sprintf("round%d", n))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// planText — текст актуального плана: step03, если ревью плана прошло,
// иначе step02.
func planText(taskDir string) string {
	for _, name := range []string{"step03-refined-plan.md", "step02-analyze.md"} {
		if data, err := os.ReadFile(filepath.Join(taskDir, name)); err == nil {
			return string(data)
		}
	}
	return ""
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

// planPathRe — путь к файлу в обратных кавычках, как их пишет план:
// `web/src/game.ts`, `web/src/game.ts:67`. Кавычки отсекают прозу и
// команды; расширение — каталоги.
var planPathRe = regexp.MustCompile("`([A-Za-z0-9_][A-Za-z0-9_./-]*\\.[A-Za-z0-9]{1,8})(?::\\d+(?:-\\d+)?)?`")

// planFiles — файлы, которые план называет, в порядке первого упоминания,
// без повторов; только те, что есть в рабочей копии и не выходят из неё.
func planFiles(worktree, plan string) []string {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range planPathRe.FindAllStringSubmatch(plan, -1) {
		rel := filepath.Clean(m[1])
		if seen[rel] || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			continue
		}
		seen[rel] = true
		// Символическая ссылка наружу (config -> /etc/…) в промпт не идёт:
		// сравнивается настоящий путь, а не имя.
		real, err := filepath.EvalSymlinks(filepath.Join(worktree, rel))
		if err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) {
			continue
		}
		if fi, err := os.Stat(real); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// filesSection — содержимое файлов плана в промпт: свежий контекст этапа
// иначе читает те же файлы заново, по одному вызову на каждый. Бюджет — как
// у диффа; файл, не влезающий в остаток, пропускается и назван, чтобы агент
// прочитал его сам. Бинарные файлы не вкладываются.
func filesSection(worktree, plan string) string {
	const budget = 60 << 10
	const perFile = 32 << 10
	files := planFiles(worktree, plan)
	if len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	var skipped []string
	used := 0
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(worktree, rel))
		if err != nil || len(data) == 0 || strings.IndexByte(string(data[:min(len(data), 8<<10)]), 0) >= 0 {
			continue
		}
		if len(data) > perFile || used+len(data) > budget {
			skipped = append(skipped, rel)
			continue
		}
		used += len(data)
		fmt.Fprintf(&sb, "<<<FILE %s\n%s\nFILE>>>\n", rel, strings.TrimRight(string(data), "\n"))
	}
	if sb.Len() == 0 && len(skipped) == 0 {
		return ""
	}
	out := "\n\nFILES (current contents of the files the plan names, as they are in the checkout now — embedded for convenience, no need to Read them; a file you have edited must be re-read before further edits):\n" + sb.String()
	if len(skipped) > 0 {
		out += "Not embedded (too large for the prompt — Read them yourself): " + strings.Join(skipped, ", ") + "\n"
	}
	return out
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

// withTools добавляет инструменты к списку этапа. Пустой список — «без
// ограничений», добавлять нечего.
func withTools(base, extra []string) []string {
	if len(base) == 0 || len(extra) == 0 {
		return base
	}
	return append(append([]string(nil), base...), extra...)
}

// --- тест-гейт ---

// shellCommand — команда проекта в своей группе процессов: по отмене или
// таймауту убивается вся группа, а не один /bin/sh — иначе дочерние
// процессы (node, go test) держали бы вывод, и CombinedOutput ждал бы их
// бесконечно.
func shellCommand(ctx context.Context, cmd, dir string) *exec.Cmd {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 5 * time.Second
	return c
}

const gateTimeout = 10 * time.Minute

func runTestGate(ctx context.Context, cmd, dir string) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	c := shellCommand(gctx, cmd, dir)
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

// --- поиск по коду ---

// codeSearchFor — конфигурация MCP и дополнительные инструменты шага, чей
// манифест просит семантический поиск. MCP-сервер — тот же бинарник; он
// открывает индекс с диска напрямую.
func (p *Pipeline) codeSearchFor(plan *protocol.Plan, enabled bool) (string, []string) {
	if p.Index == nil || !enabled {
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
