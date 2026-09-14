package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cmdSetup — интерактивная настройка. Все шаги можно задать флагами: так
// настройка воспроизводится скриптом и проверяется тестом.
func cmdSetup(args []string) error {
	fs, home := flagsFor("setup")
	orch := fs.String("orchestrator", "", "адрес оркестратора (по умолчанию http://127.0.0.1:8765)")
	pairKey := fs.String("pair-key", "", "ключ подключения из панели «Добавить устройство»")
	models := fs.String("models", "", "модели через запятую (по умолчанию все найденные)")
	slots := fs.Int("slots", 0, "сколько заданий вести одновременно (по умолчанию 2)")
	projects := fs.String("projects", "", "папка проектов (по умолчанию ~/orchestra-projects)")
	service := fs.String("service", "ask", "установить фоновую службу: yes | no | ask")
	yes := fs.Bool("yes", false, "не задавать вопросов, брать значения по умолчанию")
	noModelCheck := fs.Bool("skip-model-check", false, "не спрашивать модели (для тестов настройки; готовой такая настройка не считается)")
	_ = fs.Parse(args)

	p := paths{*home}
	var pr asker = silent{yes: *yes}
	if !*yes && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		pr = tui{}
	}
	ctx, cancel := signalContext()
	defer cancel()
	out := os.Stdout

	fmt.Fprintf(out, "Настройка orchestra-tennant (папка: %s)\n\n", p)
	cfg, err := loadConfig(p)
	if err != nil && !errors.Is(err, errNotConfigured) {
		return err
	}
	if cfg == nil {
		cfg = &Config{}
	}
	// Признак «настроен» не сбрасывается заранее: прерванный на середине
	// повторный setup (Ctrl+C на выборе моделей) не должен превращать
	// работающего демона в ненастроенного. Его выставят самопроверки в конце.

	// 1. Оркестратор.
	fmt.Fprintln(out, "1/5 Оркестратор")
	addr := *orch
	if addr == "" {
		def := cfg.Orchestrator
		if def == "" {
			def = "http://127.0.0.1:8765"
		}
		if addr, err = pr.text("Адрес оркестратора", def, func(v string) error {
			_, err := normalizeAddr(v)
			return err
		}); err != nil {
			return err
		}
	}
	if cfg.Orchestrator, err = normalizeAddr(addr); err != nil {
		return err
	}
	cfg.Remote = !isLocalAddr(cfg.Orchestrator)
	if cfg.Remote {
		cfg.Configured = false
		fmt.Fprintf(out, "  Адрес %s не на этой машине. Адрес сохранён, но транспорта для удалённого\n"+
			"  оркестратора в этой версии нет: демон умеет подключаться только к сокету на своей машине.\n"+
			"  Настройка не завершена.\n", cfg.Orchestrator)
		if err := saveConfig(p, cfg); err != nil {
			return err
		}
		return errors.New("удалённый оркестратор пока не поддерживается")
	}
	c := newClient(cfg.Orchestrator)
	if err := c.reachable(ctx); err != nil {
		_ = saveConfig(p, cfg)
		return fmt.Errorf("оркестратор по адресу %s не отвечает: %v", cfg.Orchestrator, err)
	}
	fmt.Fprintf(out, "  Оркестратор отвечает: %s\n\n", cfg.Orchestrator)

	// 2. Ключ подключения. Уже обменянный ключ не пропадает: проваленная
	// проверка после пайринга не должна стоить человеку нового ключа.
	fmt.Fprintln(out, "2/5 Ключ подключения")
	keep := false
	if cfg.DeviceKey != "" && *pairKey == "" {
		if hb, err := c.heartbeat(ctx, cfg.DeviceKey); err == nil {
			cfg.DeviceID, cfg.Socket = hb.DeviceID, hb.Socket
			fmt.Fprintf(out, "  Устройство «%s» уже подключено прежним ключом.\n", cfg.DeviceName)
			again, err := pr.confirm("Подключить заново другим ключом?", false)
			if err != nil {
				return err
			}
			keep = !again
		}
	}
	if !keep {
		key := *pairKey
		if key == "" {
			if key, err = pr.text("Ключ подключения (из панели «Добавить устройство»)", "", func(v string) error {
				if strings.TrimSpace(v) == "" {
					return errors.New("без ключа подключиться нечем")
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if strings.TrimSpace(key) == "" {
			_ = saveConfig(p, cfg)
			return errors.New("нужен ключ подключения из панели «Добавить устройство» (флаг -pair-key)")
		}
		pd, err := c.pair(ctx, strings.TrimSpace(key))
		if err != nil {
			_ = saveConfig(p, cfg)
			return err
		}
		cfg.DeviceKey, cfg.DeviceID, cfg.DeviceName = pd.DeviceKey, pd.Device.ID, pd.Device.Name
		if err := saveConfig(p, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "  Устройство «%s» подключено, ключ сохранён.\n", cfg.DeviceName)
	}
	fmt.Fprintln(out)

	// 3. Модели.
	fmt.Fprintln(out, "3/5 Модели")
	providers := discover(ctx)
	if len(providers) == 0 {
		fmt.Fprintln(out, "  Не найдено ни одного способа обращения к моделям: нет claude, codex, ollama, ключей в окружении.")
		_ = saveConfig(p, cfg)
		return errors.New("без моделей исполнителю нечем работать — установите claude CLI и повторите setup")
	}
	for _, pv := range providers {
		fmt.Fprintf(out, "  найдено: %s — %s\n", pv.Title, pv.Detail)
	}
	usable := usableModels(providers)
	if len(usable) == 0 {
		_ = saveConfig(p, cfg)
		return errors.New("ни один из найденных способов исполнитель пока не запускает — нужен claude CLI")
	}
	if *models != "" {
		cfg.Models, err = parseModels(*models, usable)
	} else {
		cfg.Models, err = pr.multi("Какие модели использовать", usable, intersect(cfg.Models, usable))
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  Выбрано: %s\n\n", strings.Join(cfg.Models, ", "))

	// 4. Одновременные задания.
	fmt.Fprintln(out, "4/5 Одновременные задания")
	n := *slots
	if n == 0 {
		def := cfg.Slots
		if def == 0 {
			def = 2
		}
		s, err := pr.text("Сколько заданий вести одновременно", strconv.Itoa(def), func(v string) error {
			k, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("нужно целое число, а не %q", strings.TrimSpace(v))
			}
			if k < 1 || k > 100 {
				return fmt.Errorf("число заданий %d вне диапазона 1..100", k)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if n, err = strconv.Atoi(strings.TrimSpace(s)); err != nil {
			return fmt.Errorf("число заданий: %q — нужно целое", s)
		}
	}
	if n < 1 || n > 100 {
		return fmt.Errorf("число заданий %d вне диапазона 1..100", n)
	}
	cfg.Slots = n
	fmt.Fprintln(out)

	// 5. Папка проектов — одна на машину; проекты оркестратор заводит внутри.
	fmt.Fprintln(out, "5/5 Папка проектов")
	dir := *projects
	if dir == "" {
		def := cfg.ProjectsDir
		if def == "" {
			def = filepath.Join(userHome(), "orchestra-projects")
		}
		if dir, err = pr.path("Папка, в которой лежат и будут создаваться проекты", def); err != nil {
			return err
		}
	}
	if dir, err = expandHome(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("папка проектов: %w", err)
	}
	cfg.ProjectsDir = dir
	fmt.Fprintf(out, "  Папка проектов: %s\n\n", dir)

	// Самопроверки. Проваленная не оставляет состояния «настроен».
	fmt.Fprintln(out, "Самопроверки")
	mc := checkModel
	if *noModelCheck {
		mc = func(context.Context, string) error {
			return errors.New("проверка пропущена флагом")
		}
	}
	ok := runChecks(ctx, cfg, out, mc)
	cfg.Configured = ok
	if ok {
		cfg.ConfiguredAt = time.Now()
	}
	if err := saveConfig(p, cfg); err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "\nНастройка не завершена: исправьте причину и запустите setup снова (ключ сохранён, вводить заново не нужно).")
		return errors.New("самопроверки не пройдены")
	}
	fmt.Fprintf(out, "\nГотово. Конфигурация: %s\n\n", p.config())

	// Служба.
	install := false
	switch *service {
	case "yes":
		install = true
	case "no":
	default:
		if install, err = pr.confirm("Установить как фоновую службу ("+serviceKind()+")?", true); err != nil {
			return err
		}
	}
	if install {
		if err := installService(p); err != nil {
			return fmt.Errorf("служба: %w", err)
		}
		fmt.Fprintf(out, "Служба установлена и запущена. Состояние: orchestra-tennant status; журнал: orchestra-tennant logs -f\n")
	} else {
		fmt.Fprintln(out, "Запуск вручную: orchestra-tennant run")
	}
	return nil
}

// intersect — прежний выбор, ограниченный тем, что есть сейчас: модели,
// которых больше нет на машине, отмечать нечего.
func intersect(prev, usable []string) []string {
	var out []string
	for _, m := range prev {
		for _, u := range usable {
			if u == m {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// parseModels разбирает список из флага и сверяет с найденным.
func parseModels(s string, usable []string) ([]string, error) {
	ok := map[string]bool{}
	for _, u := range usable {
		ok[u] = true
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		m := strings.TrimSpace(part)
		if m == "" {
			continue
		}
		if !ok[m] {
			return nil, fmt.Errorf("модель %q недоступна на этой машине; доступны: %s", m, strings.Join(usable, ", "))
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("список моделей пуст")
	}
	return out, nil
}

// expandHome раскрывает «~» и делает путь абсолютным.
func expandHome(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(userHome(), strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// isTerminal — стандартный ввод подключён к терминалу, а не к трубе или
// службе.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
