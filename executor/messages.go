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
	return true
}

func (c *changeRequest) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending
	c.pending = false
	return p
}

func (c *changeRequest) arm(cancel context.CancelFunc) {
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
}

// serveMessages разбирает сообщения, пока идёт задание.
func (r *run) serveMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-r.job.Messages():
			r.handleMessage(ctx, m.Text, m.Mode)
		}
	}
}

// handleMessage — одно сообщение: эхо в чат, триаж, затем ответ или правка.
func (r *run) handleMessage(ctx context.Context, text, mode string) {
	r.job.Emit("", "user_message", map[string]any{"text": text})
	var pending protocol.Usage
	if mode != "question" && mode != "change" {
		mode, pending = r.triage(ctx, text)
		// Чип в чате: как распознали (для question — кнопка «это правка»).
		r.job.Emit("", "triage", map[string]any{"mode": mode, "text": text})
	}
	if mode == "question" {
		r.answerQuestionRound(ctx, text, pending)
		return
	}
	r.requestChange(text, pending)
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
	actx, acancel := context.WithTimeout(ctx, r.plan.StageTimeout.Duration())
	defer acancel()
	model, effort := "fable", ""
	if def := r.plan.Stage("answer"); def != nil {
		model, effort = def.Model, def.Effort
	} else if def := r.plan.Stage("execute"); def != nil {
		model, effort = def.Model, def.Effort
	}
	res, err := agent.Run(actx, agent.RunOpts{
		Prompt: prompt, Model: agent.ModelID(model), Effort: effort,
		CWD: cwd, AddDirs: []string{r.st.TaskDir},
		AllowedTools: []string{"Read", "Glob", "Grep", "Task", "WebFetch"}, // только чтение
	}, func(ev agent.StreamEvent) { r.job.Emit("answer", ev.Type, ev.Payload) })
	r.recordUsage(st, res)
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
	entry := "\n## Правка от пользователя\n" + strings.TrimSpace(text) + "\n"
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(entry)
		_ = f.Close()
	}
	r.pendingUsage = r.pendingUsage.Add(pending)
	if r.change.request() {
		r.log("", "Правка принята — текущий этап прерван, начинаю новый раунд с «Анализа задачи».")
	}
}

// startChangeRound заводит новый раунд: все этапы плана, кроме импорта.
// База раунда — текущий HEAD, иначе пустой раунд прошёл бы проверку.
func (r *run) startChangeRound() {
	var keys []string
	for _, s := range r.plan.Stages {
		if s.Key != "import" {
			keys = append(keys, s.Key)
		}
	}
	if len(keys) == 0 {
		return
	}
	round := r.st.addRound(keys)
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
	if !r.pendingUsage.Zero() {
		if st := r.st.stage(keys[0]); st != nil {
			st.Usage = st.Usage.Add(r.pendingUsage)
		}
		r.pendingUsage = protocol.Usage{}
	}
	r.log("", fmt.Sprintf("Правка принята — раунд %d: продолжаю с этапа «Анализ задачи».", round))
	r.job.SaveState()
}
