package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/realkasparov/orchestra-tennant/agent"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Сообщения человека в чат посреди таски. Раньше их разбирал оркестратор;
// теперь — исполнитель: триажу нужна модель, ответу на вопрос — код ветки, а
// правке — новый раунд этапов в той же рабочей копии.

// changeRequest — правка, которую нужно применить новым раундом: текущий этап
// прерывается, отзыв записывается, и цикл этапов начинает раунд заново.
type changeRequest struct {
	mu      sync.Mutex
	pending bool
	cancel  context.CancelFunc // прерывает текущий этап, не всё задание
	// notify будит ожидание «Возобновить»: этап не идёт, отменять нечего,
	// а правка не должна ждать нажатия человека.
	notify chan struct{}
	// usage — расход триажа правок до того, как заведён этап, куда его
	// отнести; пишется из горутины сообщений, читается циклом этапов.
	usage protocol.Usage
}

func (c *changeRequest) request() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending {
		return false
	}
	c.pending = true
	if c.cancel != nil {
		c.cancel()
	}
	if c.notify == nil {
		c.notify = make(chan struct{}, 1)
	}
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return true
}

// wake — канал с буфером на одно уведомление о принятой правке.
func (c *changeRequest) wake() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notify == nil {
		c.notify = make(chan struct{}, 1)
	}
	return c.notify
}

func (c *changeRequest) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending
	c.pending = false
	// Токен пробуждения снимается вместе с правкой: иначе следующее
	// ожидание «Возобновить» проснулось бы от него без правки.
	if c.notify != nil {
		select {
		case <-c.notify:
		default:
		}
	}
	return p
}

// addUsage копит расход триажа; takeUsage отдаёт накопленное и обнуляет.
func (c *changeRequest) addUsage(u protocol.Usage) {
	c.mu.Lock()
	c.usage = c.usage.Add(u)
	c.mu.Unlock()
}

func (c *changeRequest) takeUsage() protocol.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.usage
	c.usage = protocol.Usage{}
	return u
}

func (c *changeRequest) arm(cancel context.CancelFunc) {
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
}

// clarifyRequest — уточнение человека к идущему агентному шагу: прогон
// прерывается (если скилл умеет продолжать), и та же сессия получает текст
// уточнения. Новый раунд при этом не заводится — сделанное остаётся.
type clarifyRequest struct {
	mu     sync.Mutex
	text   string
	cancel context.CancelFunc // прерывает текущий прогон агента
	// interrupts — скилл объявил resumable: прогон можно прервать; иначе
	// уточнение ждёт конца прогона.
	interrupts bool
	// active — идёт прогон агента: уточнение имеет смысл; иначе сообщение
	// разбирается как к стоящей таске.
	active bool
}

// arm запоминает, как прервать текущий прогон; nil — прогона нет.
func (c *clarifyRequest) arm(cancel context.CancelFunc, interrupts bool) {
	c.mu.Lock()
	c.cancel, c.interrupts, c.active = cancel, interrupts, cancel != nil
	c.mu.Unlock()
}

// running — идёт ли сейчас прогон агента.
func (c *clarifyRequest) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// request принимает уточнение: дописывает к ожидающему и прерывает прогон,
// если скилл умеет продолжить.
func (c *clarifyRequest) request(text string) {
	c.mu.Lock()
	if c.text != "" {
		c.text += "\n\n" + text
	} else {
		c.text = text
	}
	cancel, interrupts := c.cancel, c.interrupts
	c.mu.Unlock()
	if cancel != nil && interrupts {
		cancel()
	}
}

// take отдаёт накопленное уточнение и очищает его.
func (c *clarifyRequest) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.text
	c.text = ""
	return t
}

// question — вопрос, дождавшийся своей очереди: состояние таски одно, и
// править его из двух горутин нельзя. Ответ даёт цикл этапов на границе
// этапа или в ожидании; сюда попадают только распознанные вопросы.
type question struct {
	text  string
	usage protocol.Usage
}

// serveMessages разбирает сообщения, пока идёт задание. Здесь только эхо и
// триаж (он состояние не трогает): правка прерывает этап сразу, вопрос
// встаёт в очередь к циклу этапов — у того в руках состояние таски.
func (r *run) serveMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-r.job.Messages():
			if r.clar.running() {
				// Идёт агентный шаг: сообщение — уточнение к нему, без
				// триажа и без нового раунда.
				r.job.Emit("", "user_message", map[string]any{"text": m.Text})
				r.clar.request(m.Text)
				r.log("", "Уточнение принято — передаю агенту в текущую сессию.")
				continue
			}
			mode, pending := r.classify(ctx, m.Text, m.Mode)
			if mode == "question" {
				select {
				case r.questions <- question{m.Text, pending}:
					r.log("", "Вопрос принят — отвечу, как только освобожусь от текущего этапа.")
				default:
					r.log("", "Очередь вопросов переполнена — повторите вопрос позже.")
				}
				continue
			}
			r.requestChange(m.Text, pending)
		}
	}
}

// classify — эхо сообщения в чат и триаж, если режим не задан явно.
func (r *run) classify(ctx context.Context, text, mode string) (string, protocol.Usage) {
	r.job.Emit("", "user_message", map[string]any{"text": text})
	var pending protocol.Usage
	if mode != "question" && mode != "change" {
		mode, pending = r.triage(ctx, text)
		// Чип в чате: как распознали (для question — кнопка «это правка»).
		r.job.Emit("", "triage", map[string]any{"mode": mode, "text": text})
	}
	return mode, pending
}

// handleMessage — сообщение, с которым задание запущено: разбирается в
// горутине цикла этапов, поэтому ответ идёт сразу.
func (r *run) handleMessage(ctx context.Context, text, mode string) {
	mode, pending := r.classify(ctx, text, mode)
	if mode == "question" {
		r.answerQuestionRound(ctx, text, pending)
		return
	}
	r.requestChange(text, pending)
}

// answerQueued отвечает на вопросы, накопившиеся, пока шёл этап.
func (r *run) answerQueued(ctx context.Context) {
	for {
		select {
		case q := <-r.questions:
			r.answerQuestionRound(ctx, q.text, q.usage)
		default:
			return
		}
	}
}

// triage классифицирует сообщение дешёвой моделью. Всё, кроме уверенного
// «вопрос», — правка: принять правку за вопрос значило бы молча выбросить
// просьбу человека, и это худшая из ошибок.
func (r *run) triage(ctx context.Context, text string) (string, protocol.Usage) {
	cwd := r.st.WorktreeDir
	if cwd == "" {
		cwd = r.st.TaskDir
	}
	prompt := "Классифицируй сообщение пользователя по уже выполненной задаче разработки.\n" +
		"Если это ВОПРОС о проделанной работе (как запустить, что сделано, почему так, объясни) — ответь ровно `TRIAGE: question`.\n" +
		"Если это запрос на ИЗМЕНЕНИЕ кода (доработать, исправить, добавить, продолжить разработку) — ответь ровно `TRIAGE: change`.\n" +
		"При любых сомнениях выбирай change. Ничего кроме маркера не пиши.\n\n" +
		"Краткая сводка задачи — в файле " + filepath.Join(r.st.TaskDir, "task.md") + "\n\n" +
		"Сообщение пользователя:\n" + text
	tctx, tcancel := context.WithTimeout(ctx, 3*time.Minute)
	defer tcancel()
	res, err := agent.Run(tctx, agent.RunOpts{
		Prompt: prompt, Model: agent.ModelID("haiku"), Effort: "low",
		CWD: cwd, AddDirs: []string{r.st.TaskDir},
		AllowedTools: []string{"Read", "Glob", "Grep"},
	}, func(agent.StreamEvent) {}) // тихо: триаж не засоряет чат
	if res == nil {
		return "change", protocol.Usage{}
	}
	u := usageOf(res)
	if err != nil {
		return "change", u
	}
	if strings.EqualFold(agent.Triage(res.FullText), "question") {
		return "question", u
	}
	return "change", u
}

// answerQuestionRound заводит системный этап «answer» новым раундом и даёт
// агенту ответить в чате без изменения кода. Статус таски не меняется:
// вопрос не возвращает готовую таску в работу.
func (r *run) answerQuestionRound(ctx context.Context, text string, pending protocol.Usage) {
	round := r.st.addRound([]string{"answer"})
	st := r.st.stage("answer")
	st.Usage = pending // расход триажа
	st.Status = "running"
	r.emitStage(st, "running")
	r.job.SaveState()

	cwd := r.st.WorktreeDir
	if cwd == "" {
		cwd = r.st.TaskDir
	}
	prompt := "Пользователь задал вопрос о уже выполненной задаче. Ответь ему в чате: " +
		"по-человечески, по существу, без служебных маркеров и без изменения кода. " +
		"Опирайся на артефакты задачи (" + r.st.TaskDir + ": task.md, step*.md) и код ветки " + r.st.BranchName + ".\n\n" +
		"Вопрос пользователя:\n" + text
	model, effort := "fable", ""
	tools := []string{"Read", "Glob", "Grep", "Task", "WebFetch"} // только чтение
	if qa := r.plan.QA; qa != nil {
		// Скилл ответа из плана: промпт по его привязкам и манифесту.
		model, effort = qa.Model, qa.Effort
		r.question = text
		if m := r.manifest(qa.Skill); m != nil {
			if p, err := r.buildPrompt(qa, m); err == nil {
				prompt = p
			}
			if len(m.Tools) > 0 {
				tools = m.Tools
			}
		}
		r.question = ""
	} else if def := r.plan.Step("execute"); def != nil && def.Model != "" {
		model, effort = def.Model, def.Effort
	}
	actx, acancel := context.WithTimeout(ctx, r.plan.StageTimeout.Duration())
	defer acancel()
	res, err := agent.Run(actx, agent.RunOpts{
		Prompt: prompt, Model: agent.ModelID(model), Effort: effort,
		CWD: cwd, AddDirs: []string{r.st.TaskDir},
		AllowedTools: tools,
	}, func(ev agent.StreamEvent) { r.job.Emit("answer", ev.Type, ev.Payload) })
	r.recordUsage(st, res)
	if ctx.Err() != nil {
		// Пауза посреди ответа — не ошибка: этап остаётся на паузе, ответ
		// дадут заново при возобновлении.
		r.log("answer", "Ответ прерван: таска на паузе.")
		st.Status = "paused"
		r.emitStage(st, "paused")
		r.job.SaveState()
		return
	}
	if err != nil || res == nil {
		msg := "не удалось получить ответ"
		if err != nil {
			msg = err.Error()
		}
		r.log("answer", "Ошибка обработки запроса: "+msg)
		st.Status = "error"
		r.emitStage(st, "error")
		r.job.SaveState()
		return
	}
	st.Status = "done"
	r.emitStage(st, "done")
	_ = round
	r.job.SaveState()
}

// requestChange записывает отзыв и просит цикл этапов начать новый раунд.
// Текущий этап прерывается; сам новый раунд заводит цикл, когда до него
// дойдёт, — заводить его отсюда значило бы гонку с идущим этапом.
func (r *run) requestChange(text string, pending protocol.Usage) {
	path := filepath.Join(r.st.TaskDir, "user-feedback.md")
	r.st.Feedback = strings.TrimSpace(text)
	entry := "\n## Правка от пользователя\n" + strings.TrimSpace(text) + "\n"
	// Повторный разбор (раунд не завёлся, задание возобновили) не дублирует
	// запись, которая уже стоит последней.
	if prev, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(prev), entry) {
		r.change.addUsage(pending)
		r.change.request()
		return
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(entry)
		_ = f.Close()
	}
	r.change.addUsage(pending)
	if r.change.request() {
		r.log("", "Правка принята — текущий этап прерван, начинаю новый раунд с шага «"+r.reworkTitle()+"».")
	}
}

// reworkTitle — заголовок шага, с которого начинается раунд правки.
func (r *run) reworkTitle() string {
	if s := r.plan.Step(r.pipe().ReworkKey()); s != nil {
		return s.Title
	}
	return r.pipe().ReworkKey()
}

// reworkKeys — шаги раунда правки: от шага rework до конца, без финиша.
func (r *run) reworkKeys() []string {
	pipe := r.pipe()
	rework := pipe.ReworkKey()
	var keys []string
	started := rework == ""
	for _, s := range r.plan.Steps {
		if s.Key == rework {
			started = true
		}
		if !started || s.Kind == protocol.KindFinish || strings.HasPrefix(s.Kind, "start.") {
			continue
		}
		keys = append(keys, s.Key)
	}
	return keys
}

// roundArtifacts — файлы, которые пишут скиллы шагов раунда правки: их и
// уносим в папку прошлого раунда.
func (r *run) roundArtifacts(keys []string) []string {
	var names []string
	seen := map[string]bool{}
	for _, k := range keys {
		s := r.plan.Step(k)
		if s == nil || s.Skill == "" {
			continue
		}
		m := r.manifest(s.Skill)
		if m == nil {
			continue
		}
		for _, a := range m.Outputs.Artifacts {
			if !seen[a.Name] {
				seen[a.Name] = true
				names = append(names, a.Name)
			}
		}
	}
	if len(names) == 0 {
		names = []string{"step02-analyze.md", "step03-refined-plan.md", "step04-execution.md", "step05-review.md", "step06-handoff.md"}
	}
	return names
}

// startChangeRound заводит новый раунд с шага rework. База раунда — текущий
// HEAD, иначе пустой раунд прошёл бы проверку. Незакоммиченные изменения в
// рабочей копии — отказ до первого этапа.
func (r *run) startChangeRound() error {
	keys := r.reworkKeys()
	if len(keys) == 0 {
		return nil
	}
	if r.st.WorktreeDir != "" {
		dirty, err := gitops.DirtyFiles(r.st.WorktreeDir)
		if err != nil {
			return fmt.Errorf("проверка рабочей копии: %w", err)
		}
		if len(dirty) > 0 {
			return fmt.Errorf("в рабочей копии есть незакоммиченные изменения (%s) — закоммитьте, спрячьте (git stash) или отмените их и повторите правку", strings.Join(dirty, ", "))
		}
	}
	// Артефакты прошлого раунда — в его папку: этапы нового раунда должны
	// видеть свои step-файлы, а не прошлогодний план. Не перенеслись —
	// раунд не начинается: иначе выполнение взяло бы прошлый план.
	if err := archiveRound(r.st.TaskDir, r.st.round(), r.roundArtifacts(keys)); err != nil {
		return fmt.Errorf("перенос артефактов прошлого раунда: %w", err)
	}
	round := r.st.addRound(keys)
	r.markDisabled()
	if r.st.WorktreeDir != "" {
		if head, err := gitops.HeadSHA(r.st.WorktreeDir); err == nil {
			r.st.RoundBase = head
			r.job.Emit("", "task_field", map[string]any{"round_base": head})
		}
	}
	for _, key := range keys {
		r.emitStage(r.st.stage(key), "pending")
	}
	// Расход триажа — на первый этап нового раунда.
	if u := r.change.takeUsage(); !u.Zero() {
		if st := r.st.stage(keys[0]); st != nil {
			st.Usage = st.Usage.Add(u)
		}
	}
	r.log("", fmt.Sprintf("Правка принята — раунд %d: продолжаю с шага «%s».", round, r.reworkTitle()))
	r.job.SaveState()
	return nil
}
