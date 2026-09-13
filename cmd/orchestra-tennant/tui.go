package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
)

// Вопросы настройки в терминале. Два способа ответить: интерактивный — поля
// с курсором, список моделей со стрелками и пробелом, путь с автодополнением
// папок по Tab; и безмолвный — из флагов, когда терминала нет или задан
// -yes. Оба отвечают на одни и те же вопросы, поэтому настройка скриптом и
// руками не расходится.

// asker — вопросы человеку.
type asker interface {
	// text — строка; validate, если задан, не выпускает из поля с ошибкой.
	text(label, def string, validate func(string) error) (string, error)
	// path — путь к папке с автодополнением существующих папок.
	path(label, def string) (string, error)
	// multi — несколько из списка; preselected — что отмечено сначала.
	multi(label string, options, preselected []string) ([]string, error)
	confirm(label string, def bool) (bool, error)
}

var errAborted = errors.New("ввод прерван")

// --- интерактивно ---

type tui struct{}

func (tui) keymap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	// Tab — принять подсказку пути, а не перейти дальше: так дополняют путь
	// все оболочки, и рука тянется именно к Tab.
	km.Input.AcceptSuggestion = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "дополнить"))
	km.Input.Next = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "далее"))
	km.Input.Submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "готово"))
	km.MultiSelect.Toggle = key.NewBinding(key.WithKeys(" ", "x"), key.WithHelp("пробел", "отметить"))
	km.MultiSelect.Up = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑", "вверх"))
	km.MultiSelect.Down = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓", "вниз"))
	km.MultiSelect.Submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "готово"))
	km.MultiSelect.SelectAll = key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "отметить все"))
	km.MultiSelect.SelectNone = key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "снять все"), key.WithDisabled())
	km.MultiSelect.Filter = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "фильтр"))
	km.MultiSelect.SetFilter = key.NewBinding(key.WithKeys("enter", "esc"), key.WithHelp("esc", "применить фильтр"), key.WithDisabled())
	km.MultiSelect.ClearFilter = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "снять фильтр"), key.WithDisabled())
	km.MultiSelect.HalfPageUp = key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "½ страницы вверх"))
	km.MultiSelect.HalfPageDown = key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "½ страницы вниз"))
	km.MultiSelect.GotoTop = key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("home", "в начало"))
	km.MultiSelect.GotoBottom = key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end", "в конец"))
	km.Input.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "назад"))
	km.MultiSelect.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "назад"))
	km.Confirm.Prev = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "назад"))
	km.Confirm.Next = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "далее"))
	km.MultiSelect.Next = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "далее"))
	km.Confirm.Submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "готово"))
	km.Confirm.Toggle = key.NewBinding(key.WithKeys("h", "l", "right", "left", "tab"), key.WithHelp("←/→", "выбрать"))
	km.Confirm.Accept = key.NewBinding(key.WithKeys("y", "Y", "д", "Д"), key.WithHelp("y", "да"))
	km.Confirm.Reject = key.NewBinding(key.WithKeys("n", "N", "н", "Н"), key.WithHelp("n", "нет"))
	return km
}

func (t tui) run(field huh.Field) error {
	err := huh.NewForm(huh.NewGroup(field)).WithKeyMap(t.keymap()).WithTheme(huh.ThemeBase()).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return errAborted
	}
	return err
}

func (t tui) text(label, def string, validate func(string) error) (string, error) {
	v := def
	in := huh.NewInput().Title(label).Value(&v).Prompt("  ")
	if validate != nil {
		in = in.Validate(validate)
	}
	if err := t.run(in); err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (t tui) path(label, def string) (string, error) {
	v := def
	in := huh.NewInput().Title(label).Description("Tab — дополнить именем существующей папки").
		Value(&v).Prompt("  ").SuggestionsFunc(func() []string { return dirSuggestions(v) }, &v)
	if err := t.run(in); err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (t tui) multi(label string, options, preselected []string) ([]string, error) {
	pre := map[string]bool{}
	for _, p := range preselected {
		pre[p] = true
	}
	opts := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		opts = append(opts, huh.NewOption(o, o).Selected(len(pre) == 0 || pre[o]))
	}
	var picked []string
	ms := huh.NewMultiSelect[string]().Title(label).Description("↑/↓ — переход, пробел — отметить, Enter — готово").
		Options(opts...).Value(&picked).
		Validate(func(v []string) error {
			if len(v) == 0 {
				return errors.New("отметьте хотя бы одну модель")
			}
			return nil
		})
	if err := t.run(ms); err != nil {
		return nil, err
	}
	// В порядке списка, а не отметки.
	out := make([]string, 0, len(picked))
	for _, o := range options {
		for _, p := range picked {
			if p == o {
				out = append(out, o)
				break
			}
		}
	}
	return out, nil
}

func (t tui) confirm(label string, def bool) (bool, error) {
	v := def
	c := huh.NewConfirm().Title(label).Affirmative("Да").Negative("Нет").Value(&v)
	if err := t.run(c); err != nil {
		return false, err
	}
	return v, nil
}

// dirSuggestions — существующие папки, с которых начинается введённый путь:
// «~/orch» → «~/orchestra-projects». Подсказка сохраняет форму ввода («~»
// остаётся «~»), потому что поле дополняет строку, а не заменяет её.
func dirSuggestions(typed string) []string {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return nil
	}
	expanded := typed
	if typed == "~" || strings.HasPrefix(typed, "~/") {
		expanded = filepath.Join(userHome(), strings.TrimPrefix(typed, "~"))
		if strings.HasSuffix(typed, "/") {
			expanded += "/"
		}
	}
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	// Введённая часть до последнего разделителя остаётся как есть.
	typedDir := typed[:len(typed)-len(prefix)]
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		out = append(out, typedDir+name+"/")
	}
	sort.Strings(out)
	return out
}

// --- из флагов, без терминала ---

// silent отвечает значениями по умолчанию: так настройка идёт скриптом
// (-yes) и там, где терминала нет. Вопрос без значения по умолчанию — ошибка
// с подсказкой, каким флагом его задать.
type silent struct {
	yes bool // -yes: молча брать значения по умолчанию, даже пустые
}

func (s silent) text(label, def string, validate func(string) error) (string, error) {
	if def == "" && !s.yes {
		return "", fmt.Errorf("нужно значение «%s», а терминала нет — задайте его флагом", label)
	}
	if validate != nil {
		if err := validate(def); err != nil {
			return "", err
		}
	}
	return def, nil
}

func (s silent) path(label, def string) (string, error) { return s.text(label, def, nil) }

func (s silent) multi(label string, options, preselected []string) ([]string, error) {
	if len(preselected) > 0 {
		return preselected, nil
	}
	return append([]string(nil), options...), nil
}

func (s silent) confirm(label string, def bool) (bool, error) { return def, nil }
