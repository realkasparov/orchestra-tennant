// Package repomap строит компактную карту символов репозитория: какие файлы
// важны и что в них объявлено. Карта детерминированная (никаких эмбеддингов и
// сетевых вызовов) и вкладывается в промпт анализа, чтобы агент не тратил
// разведочные ходы на «осмотреться» в незнакомой базе.
//
// Ранжирование — упрощённый вариант подхода Aider: важен тот файл, чьи
// объявленные символы чаще упоминаются в ОСТАЛЬНЫХ файлах репозитория.
package repomap

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultBudget — потолок карты в байтах. Порядок величины подобран под этап
// анализа: заметно меньше вложенного плана (24К), но хватает на сотни файлов.
const DefaultBudget = 16 << 10

const (
	maxFiles    = 5000      // выше этого репозиторий сканируется частично
	maxFileSize = 512 << 10 // мегабайтные файлы — почти всегда данные, не код
)

// skipDirs — каталоги, которые не несут авторского кода проекта.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, ".next": true, "coverage": true,
	".venv": true, "venv": true, ".git": true, "third_party": true,
}

// symbolRe — правила извлечения объявлений по расширению файла. Регэкспы, а не
// tree-sitter: он тянет cgo-зависимость ради выигрыша, который на уровне
// «список объявлений верхнего уровня» почти не заметен.
var symbolRe = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^type\s+([A-Z_a-z]\w*)`),
	},
	".ts": tsRules(), ".tsx": tsRules(), ".js": tsRules(), ".jsx": tsRules(), ".mjs": tsRules(),
	".py": {
		regexp.MustCompile(`(?m)^\s{0,4}def\s+([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^class\s+([A-Z_a-z]\w*)`),
	},
	".rb": {
		regexp.MustCompile(`(?m)^\s*(?:class|module)\s+([A-Z]\w*)`),
		regexp.MustCompile(`(?m)^\s*def\s+(?:self\.)?([A-Z_a-z]\w*)`),
	},
	".rs": {
		regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:fn|struct|enum|trait)\s+([A-Z_a-z]\w*)`),
	},
	".java": javaRules(), ".kt": javaRules(), ".cs": javaRules(),
	".php": {
		regexp.MustCompile(`(?m)^\s*(?:abstract\s+|final\s+)?(?:class|interface|trait)\s+([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^\s*(?:public\s+|private\s+|protected\s+|static\s+)*function\s+([A-Z_a-z]\w*)`),
	},
}

// tsRules намеренно якорятся на начало строки без отступа (или на export):
// объявления внутри функций — это локальные переменные, они забивают карту
// шумом вроде `out`, `next`, `el` и вытесняют настоящие точки входа.
func tsRules() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?m)^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+\*?([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^(?:export\s+)?(?:interface|type|enum)\s+([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^(?:export\s+)?const\s+([A-Z_a-z]\w*)\s*[:=]`),
	}
}

func javaRules() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*(?:public|private|protected|internal)?\s*(?:static\s+|final\s+|abstract\s+|sealed\s+|data\s+)*(?:class|interface|record|enum|object|struct)\s+([A-Z_a-z]\w*)`),
		regexp.MustCompile(`(?m)^\s*(?:public|private|protected|internal)\s+(?:static\s+|final\s+|async\s+|override\s+|virtual\s+)*[\w<>\[\],.?]+\s+([A-Z_a-z]\w*)\s*\(`),
	}
}

var identRe = regexp.MustCompile(`[A-Za-z_]\w{2,}`)

// File — один файл карты с его символами и рангом.
type File struct {
	Path    string
	Symbols []string
	Rank    int // сколько ДРУГИХ файлов упоминают символы этого файла
}

// Map — результат построения карты.
type Map struct {
	Files      []File // отсортированы по убыванию ранга
	TotalFiles int    // сколько файлов с кодом найдено всего
	Truncated  bool   // карта не поместилась в бюджет целиком
}

// Build сканирует репозиторий в dir и возвращает ранжированную карту символов.
// Пустая карта (без ошибки) — нормальный результат для репозитория без
// поддерживаемых языков.
func Build(dir string) (*Map, error) {
	paths, err := listFiles(dir)
	if err != nil {
		return nil, err
	}

	type parsed struct {
		path    string
		symbols []string
		idents  map[string]bool
	}
	files := make([]parsed, 0, len(paths))
	for _, rel := range paths {
		data, rerr := os.ReadFile(filepath.Join(dir, rel))
		if rerr != nil || len(data) > maxFileSize {
			continue
		}
		syms := extractSymbols(rel, data)
		if len(syms) == 0 {
			continue
		}
		idents := map[string]bool{}
		for _, m := range identRe.FindAll(data, -1) {
			idents[string(m)] = true
		}
		files = append(files, parsed{path: rel, symbols: syms, idents: idents})
	}

	// Инвертированный индекс «символ → файлы, где он упоминается»: прямое
	// сравнение каждого файла с каждым — квадрат по числу файлов и на большом
	// репозитории считается минутами.
	declared := map[string]bool{}
	for i := range files {
		for _, s := range files[i].symbols {
			declared[s] = true
		}
	}
	mentions := make(map[string][]int, len(declared))
	for j := range files {
		for ident := range files[j].idents {
			if declared[ident] {
				mentions[ident] = append(mentions[ident], j)
			}
		}
	}

	// Ранг файла: сколько ДРУГИХ файлов упоминают хотя бы один его символ.
	out := &Map{TotalFiles: len(files)}
	seen := make(map[int]bool)
	for i := range files {
		clear(seen)
		for _, s := range files[i].symbols {
			for _, j := range mentions[s] {
				if j != i {
					seen[j] = true
				}
			}
		}
		out.Files = append(out.Files, File{Path: files[i].path, Symbols: files[i].symbols, Rank: len(seen)})
	}
	// Детерминированный порядок: ранг убывает, при равенстве — путь по алфавиту.
	sort.Slice(out.Files, func(i, j int) bool {
		if out.Files[i].Rank != out.Files[j].Rank {
			return out.Files[i].Rank > out.Files[j].Rank
		}
		return out.Files[i].Path < out.Files[j].Path
	})
	return out, nil
}

// Render превращает карту в текст для промпта. Результат целиком (вместе с
// заголовком) укладывается в budget байт; budget <= 0 означает DefaultBudget.
func (m *Map) Render(budget int) string {
	if m == nil || len(m.Files) == 0 {
		return ""
	}
	if budget <= 0 {
		budget = DefaultBudget
	}
	lines := make([]string, len(m.Files))
	for i, f := range m.Files {
		lines[i] = f.Path + "\n  " + strings.Join(f.Symbols, ", ") + "\n"
	}
	// Заголовок зависит от того, сколько строк поместилось, а это — от длины
	// заголовка. Разрываем круг, отбрасывая хвост, пока целое не влезет.
	for shown := len(lines); shown > 0; shown-- {
		head := header(shown, m.TotalFiles)
		total := len(head)
		for _, l := range lines[:shown] {
			total += len(l)
		}
		if total <= budget {
			m.Truncated = shown < len(lines)
			return head + strings.Join(lines[:shown], "")
		}
	}
	return ""
}

func header(shown, total int) string {
	head := "REPO MAP (файлы репозитория, ранжированные по числу внешних ссылок на их символы; " +
		"ориентир, а не полный список — детали смотри Grep/Read"
	if shown < total {
		head += "; показаны " + itoa(shown) + " из " + itoa(total) + " файлов"
	}
	return head + "):\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Decl — объявление в файле: имя и байтовое смещение его начала.
type Decl struct {
	Name   string
	Offset int
}

// Decls возвращает объявления файла, отсортированные по смещению. Это общий
// разбор языков для карты (какие символы есть) и для индекса (где резать файл
// на чанки) — правила языков живут в одном месте.
func Decls(path string, data []byte) []Decl {
	rules, ok := symbolRe[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return nil
	}
	var out []Decl
	for _, re := range rules {
		for _, loc := range re.FindAllSubmatchIndex(data, -1) {
			// loc[0] — начало совпадения, loc[2:4] — группа с именем.
			name := string(data[loc[2]:loc[3]])
			if isNoise(name) {
				continue
			}
			out = append(out, Decl{Name: name, Offset: loc[0]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// extractSymbols возвращает уникальные объявления файла в порядке появления.
func extractSymbols(path string, data []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range Decls(path, data) {
		if seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		out = append(out, d.Name)
	}
	// Слишком длинный список от одного файла вытесняет остальные — обрезаем.
	if len(out) > 40 {
		out = append(out[:40:40], "…")
	}
	return out
}

// Supported сообщает, разбирается ли файл с таким расширением.
func Supported(path string) bool {
	_, ok := symbolRe[strings.ToLower(filepath.Ext(path))]
	return ok
}

// isNoise отсеивает ключевые слова, которые регэкспы иногда ловят как имена.
func isNoise(name string) bool {
	switch name {
	case "if", "for", "switch", "return", "func", "class", "function", "new",
		"const", "let", "var", "type", "interface", "public", "private", "static":
		return true
	}
	return false
}

// ListCodeFiles возвращает пути файлов с кодом (относительно dir), которые
// имеет смысл разбирать: без .gitignore-мусора, вендоренных каталогов и
// неподдерживаемых расширений. Используется и картой, и индексом кода.
func ListCodeFiles(dir string) ([]string, error) { return listFiles(dir) }

// listFiles берёт список файлов из git (это бесплатно даёт учёт .gitignore),
// а вне git-репозитория обходит дерево сам.
func listFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err == nil {
		var paths []string
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" && !skipPath(p) {
				paths = append(paths, p)
			}
			if len(paths) >= maxFiles {
				break
			}
		}
		return paths, nil
	}

	var paths []string
	werr := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || len(paths) >= maxFiles {
			return nil //nolint:nilerr // недоступный подкаталог не должен ронять карту
		}
		if info.IsDir() {
			// Корень пропускать нельзя, даже если его имя начинается с точки.
			if p != dir && (skipDirs[info.Name()] || strings.HasPrefix(info.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr == nil && !skipPath(rel) {
			paths = append(paths, rel)
		}
		return nil
	})
	return paths, werr
}

func skipPath(rel string) bool {
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skipDirs[part] {
			return true
		}
	}
	base := filepath.Base(rel)
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".lock") {
		return true
	}
	_, known := symbolRe[strings.ToLower(filepath.Ext(rel))]
	return !known
}
