package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/realkasparov/orchestra-tennant/protocol"
)

// ModelID и таблица моделей живут в protocol: оркестратору они нужны для
// плана, а тянуть ради них пакет запуска claude ему незачем.
func ModelID(key string) string { return protocol.ModelID(key) }

// ModelIDs — все сборки, которые умеет запускать этот исполнитель.
func ModelIDs() []string { return protocol.ModelIDs() }

type RunOpts struct {
	Prompt  string // required — also for resume (bare --resume exits silently)
	Resume  string // session id to resume, empty for a fresh session
	Model   string // claude model id
	Effort  string // reasoning effort level: low|medium|high|max (empty = CLI default)
	CWD     string
	AddDirs []string
	// AllowedTools scopes the agent's tools (--allowedTools) for stages that
	// ingest untrusted GitLab content. Empty means bypassPermissions with no
	// restriction (used for execute/review, which need to edit and run freely).
	AllowedTools []string
	// MCPConfig — JSON конфигурации MCP-серверов (--mcp-config). Пусто —
	// внешние инструменты не подключаются.
	MCPConfig string
	// Markers — маркеры скилла (с двоеточием, как `RESULT:`): вырезаются из
	// текста для чата вместе со встроенными.
	Markers []string
}

// StreamEvent is one normalized event for the UI log.
type StreamEvent struct {
	Type    string // agent_text | tool_use | tool_result | log
	Payload map[string]any
}

// Usage is the token/cost accounting of one claude run, taken from the final
// "result" event (нули, если CLI его не прислал — например, при обрыве).
type Usage struct {
	InputTokens  int64   `json:"tok_in"`
	OutputTokens int64   `json:"tok_out"`
	CacheWrite   int64   `json:"cache_write"`
	CacheRead    int64   `json:"cache_read"`
	CostUSD      float64 `json:"cost_usd"`
}

type Result struct {
	SessionID string
	FullText  string // all assistant text, concatenated
	GotResult bool   // a "result" event was seen
	IsError   bool
	ErrText   string
	Usage     Usage // totals of the whole run
}

// Run spawns one headless claude turn and streams normalized events to onEvent.
// stdin is closed immediately; the turn ends when the process exits.
func Run(ctx context.Context, opts RunOpts, onEvent func(StreamEvent)) (*Result, error) {
	args := []string{"-p"}
	if opts.Resume != "" {
		args = append(args, "--resume", opts.Resume)
	}
	args = append(args, opts.Prompt,
		"--output-format", "stream-json", "--verbose",
		"--model", opts.Model,
	)
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.MCPConfig != "" {
		// --strict-mcp-config: подключаем ровно наши серверы, игнорируя
		// пользовательские ~/.claude конфигурации — состав инструментов этапа
		// должен быть предсказуемым.
		args = append(args, "--mcp-config", opts.MCPConfig, "--strict-mcp-config")
	}
	if len(opts.AllowedTools) > 0 {
		// Restricted stage: acceptEdits auto-approves edits (headless has no
		// interactive prompt) while --allowedTools confines which tools —
		// notably Bash — the agent may use, so injected content can't run
		// arbitrary shell commands.
		args = append(args, "--permission-mode", "acceptEdits",
			"--allowedTools", strings.Join(opts.AllowedTools, ","))
	} else {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	for _, d := range opts.AddDirs {
		args = append(args, "--add-dir", d)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = opts.CWD
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Pause = context cancel → SIGINT, then SIGKILL if it doesn't die.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGINT)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()

	// stderr дочитывается до Wait: Wait закрывает трубу, и читатель, не
	// успевший до конца, оставил бы причину падения обрезанной — а то и
	// гонку с чтением буфера ниже.
	var errBuf strings.Builder
	var errMu sync.Mutex
	stderrText := func() string {
		errMu.Lock()
		defer errMu.Unlock()
		return strings.TrimSpace(errBuf.String())
	}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			errMu.Lock()
			errBuf.WriteString(sc.Text() + "\n")
			errMu.Unlock()
		}
	}()

	res := &Result{}
	var text strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			// После SIGINT по паузе CLI отчитывается «инструмент отклонён» и
			// «ошибка выполнения» — это не провал: результат помечается
			// прерванным, чтобы чат не показывал ошибку. Лимит времени этапа
			// (DeadlineExceeded) — провал, и помечать его нечем.
			msg["_interrupted"] = true
		}
		handleLine(msg, res, &text, onEvent, opts.Markers)
	}
	select {
	case <-stderrDone:
	case <-time.After(5 * time.Second): // трубу мог унаследовать потомок агента
	}
	waitErr := cmd.Wait()
	close(done)
	res.FullText = text.String()

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if waitErr != nil {
		// Причину CLI чаще пишет в итоговое событие, а не в stderr: «не удалось
		// войти», «модель недоступна». Без неё код выхода ничего не объясняет.
		if res.IsError && res.ErrText != "" {
			return res, fmt.Errorf("claude exited: %w: %s", waitErr, res.ErrText)
		}
		return res, fmt.Errorf("claude exited: %w: %s", waitErr, stderrText())
	}
	if !res.GotResult {
		// exit 0 without a result event = stage error (e.g. bare resume quirk)
		return res, fmt.Errorf("claude exited without a result event: %s", stderrText())
	}
	if res.IsError {
		return res, fmt.Errorf("claude reported error: %s", res.ErrText)
	}
	return res, nil
}

func handleLine(msg map[string]any, res *Result, text *strings.Builder, onEvent func(StreamEvent), markers []string) {
	switch msg["type"] {
	case "system":
		if msg["subtype"] == "init" {
			if sid, ok := msg["session_id"].(string); ok {
				res.SessionID = sid
			}
		}
	case "assistant":
		m, _ := msg["message"].(map[string]any)
		content, _ := m["content"].([]any)
		for _, c := range content {
			block, _ := c.(map[string]any)
			switch block["type"] {
			case "text":
				t, _ := block["text"].(string)
				if strings.TrimSpace(t) == "" {
					continue
				}
				text.WriteString(t + "\n")
				// В чат — без служебных маркеров (их читает оркестратор из
				// полного текста выше).
				if shown := StripMarkers(t, markers...); shown != "" {
					onEvent(StreamEvent{Type: "agent_text", Payload: map[string]any{"text": shown}})
				}
			case "tool_use":
				name, _ := block["name"].(string)
				input, _ := json.Marshal(block["input"])
				onEvent(StreamEvent{Type: "tool_use", Payload: map[string]any{
					"name": name, "summary": toolSummary(name, block["input"]), "detail": string(input),
				}})
			}
		}
	case "user":
		m, _ := msg["message"].(map[string]any)
		content, _ := m["content"].([]any)
		for _, c := range content {
			block, _ := c.(map[string]any)
			if block["type"] == "tool_result" {
				detail, _ := json.Marshal(block["content"])
				summary := "ok"
				if isErr, _ := block["is_error"].(bool); isErr {
					summary = "error"
					if msg["_interrupted"] == true {
						summary = "interrupted"
					}
				}
				onEvent(StreamEvent{Type: "tool_result", Payload: map[string]any{
					"summary": summary, "detail": truncate(string(detail), 20000),
				}})
			}
		}
	case "result":
		res.GotResult = true
		if sid, ok := msg["session_id"].(string); ok && sid != "" {
			res.SessionID = sid
		}
		if isErr, _ := msg["is_error"].(bool); isErr {
			res.IsError = true
			res.ErrText, _ = msg["result"].(string)
		}
		if u, ok := msg["usage"].(map[string]any); ok {
			res.Usage.InputTokens = i64(u["input_tokens"])
			res.Usage.OutputTokens = i64(u["output_tokens"])
			res.Usage.CacheWrite = i64(u["cache_creation_input_tokens"])
			res.Usage.CacheRead = i64(u["cache_read_input_tokens"])
		}
		if c, ok := msg["total_cost_usd"].(float64); ok {
			res.Usage.CostUSD = c
		}
	}
}

// i64 reads a JSON number (decoded as float64) as int64; anything else → 0.
func i64(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func toolSummary(name string, input any) string {
	in, _ := input.(map[string]any)
	for _, key := range []string{"file_path", "path", "pattern", "command", "prompt", "description", "url"} {
		if v, ok := in[key].(string); ok && v != "" {
			return truncate(v, 120)
		}
	}
	return name
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
