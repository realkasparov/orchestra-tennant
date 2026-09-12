// Package mcpserver отдаёт агенту инструмент семантического поиска по коду
// через MCP (stdio, JSON-RPC 2.0).
//
// Почему MCP, а не консольная обёртка: этапы, читающие недоверенное содержимое
// (анализ, ревью плана), намеренно работают без Bash — иначе внедрённая в
// задачу инструкция смогла бы выполнить произвольную команду. MCP-инструмент
// добавляется в allowlist точечно, не открывая shell.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/realkasparov/orchestra-tennant/codeindex"
)

// ToolName — как инструмент виден агенту (claude CLI префиксует MCP-имена).
const (
	ServerName = "codesearch"
	ToolName   = "mcp__codesearch__code_search"
)

// description — это и есть роутер: агент сам решает, звать ли поиск, поэтому
// текст описывает не «что делает», а «когда звать» (design D1).
const description = `Семантический поиск по индексу кода проекта.

Когда звать:
- запрос концептуальный («где обрабатываются возвраты», «как устроена авторизация»);
- неизвестно точное ключевое слово или имя символа;
- Grep вернул слишком много результатов или ничего.

Когда НЕ звать: известно точное имя символа, строка ошибки или путь — Grep быстрее и точнее.

Результат — фрагменты кода с путями и номерами строк. Фрагмент, помеченный
"stale", устарел (файл изменился после индексации) — перечитай файл через Read.`

// Searcher — то, что умеет искать по индексу. Настоящая реализация — индекс,
// открытый с диска; в тестах — заглушка.
type Searcher interface {
	Search(ctx context.Context, query string, limit int, module string) ([]codeindex.Result, error)
}

// OpenIndex открывает индекс проекта с диска. Сервер запускается как дочерний
// процесс `claude` рядом с исполнителем, и индекс лежит на том же диске:
// ходить за поиском в HTTP API оркестратора ему нечем — сессии у него нет, а
// на другой машине и адреса.
func OpenIndex(dataDir string, projectID int64, root string) (*codeindex.Index, error) {
	dbPath := filepath.Join(dataDir, "index", fmt.Sprintf("project-%d.db", projectID))
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("индекс проекта не найден: %s", dbPath)
	}
	return codeindex.Open(dbPath, root, codeindex.NewEmbedder(""))
}

// Serve обслуживает MCP-сессию на stdio: читает JSON-RPC запросы и выполняет
// поиск по индексу.
func Serve(in io.Reader, out io.Writer, idx Searcher) error {
	c := &client{idx: idx}
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue // битую строку молча пропускаем: протокол потоковый
		}
		resp, send := c.handle(&req)
		if !send {
			continue // уведомления ответа не требуют
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type client struct {
	idx Searcher
}

func (c *client) handle(req *request) (*response, bool) {
	// Уведомления (без id) ответа не требуют.
	if len(req.ID) == 0 {
		return nil, false
	}
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": "1"},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{map[string]any{
			"name":        "code_search",
			"description": description,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":  map[string]any{"type": "string", "description": "Что искать — своими словами или фрагментом кода"},
					"limit":  map[string]any{"type": "integer", "description": "Сколько фрагментов вернуть (по умолчанию 8)"},
					"module": map[string]any{"type": "string", "description": "Необязательный фильтр по каталогу/модулю, например internal/api"},
				},
				"required": []string{"query"},
			},
		}}}
	case "tools/call":
		text, err := c.call(req.Params)
		if err != nil {
			// Ошибку возвращаем как результат с isError: агент должен увидеть
			// причину и продолжить grep'ом, а не получить обрыв сессии.
			resp.Result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "code_search недоступен: " + err.Error()}},
				"isError": true,
			}
			break
		}
		resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, true
}

func (c *client) call(raw json.RawMessage) (string, error) {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Module string `json:"module"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	if strings.TrimSpace(params.Arguments.Query) == "" {
		return "", fmt.Errorf("пустой запрос")
	}
	limit := params.Arguments.Limit
	if limit <= 0 {
		limit = 8
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	results, err := c.idx.Search(ctx, params.Arguments.Query, limit, params.Arguments.Module)
	if err != nil {
		return "", err
	}
	return render(results), nil
}

func render(results []codeindex.Result) string {
	if len(results) == 0 {
		return "Ничего не найдено. Попробуй Grep по конкретному имени или другую формулировку."
	}
	var sb strings.Builder
	for i, r := range results {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s:%d-%d", r.Path, r.StartLine, r.EndLine)
		if len(r.Symbols) > 0 {
			fmt.Fprintf(&sb, "  [%s]", strings.Join(r.Symbols, ", "))
		}
		if r.Stale {
			sb.WriteString("  ⚠ stale — файл изменился после индексации, перечитай через Read")
		}
		sb.WriteString("\n")
		sb.WriteString(strings.TrimRight(r.Body, "\n"))
		sb.WriteString("\n")
	}
	return sb.String()
}
