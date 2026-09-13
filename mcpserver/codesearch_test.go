package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/realkasparov/orchestra-tennant/codeindex"
)

// fakeIndex — заглушка индекса: запоминает запрос и отдаёт заготовленное.
type fakeIndex struct {
	results []codeindex.Result
	err     error
	query   string
}

func (f *fakeIndex) Search(_ context.Context, query string, _ int, _ string) ([]codeindex.Result, error) {
	f.query = query
	return f.results, f.err
}

// runSession прогоняет строки запросов через Serve и возвращает ответы.
func runSession(t *testing.T, idx Searcher, requests ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := Serve(strings.NewReader(strings.Join(requests, "\n")+"\n"), &out, idx); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("невалидный JSON-RPC ответ %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// Хендшейк и список инструментов; уведомления ответа не порождают.
func TestProtocolHandshake(t *testing.T) {
	resps := runSession(t, &fakeIndex{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`не json — не должен ронять сессию`,
		`{"jsonrpc":"2.0","id":3,"method":"unknown/method"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("ожидалось 3 ответа (уведомление и мусор — без ответа), got %d", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] == "" {
		t.Error("нет версии протокола")
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "code_search" {
		t.Fatalf("имя инструмента = %v", tool["name"])
	}
	// Описание — это роутер: агент по нему решает, звать ли поиск.
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "Когда звать") || !strings.Contains(desc, "Grep") {
		t.Errorf("описание не объясняет, когда звать инструмент:\n%s", desc)
	}
	if resps[2]["error"] == nil {
		t.Error("неизвестный метод должен возвращать ошибку JSON-RPC")
	}
}

// Успешный вызов форматирует находки; stale помечается для агента.
func TestToolCallRendersResults(t *testing.T) {
	idx := &fakeIndex{results: []codeindex.Result{
		{Path: "mail/rate.go", StartLine: 10, EndLine: 20, Symbols: []string{"RateGate"}, Body: "func RateGate() {}"},
		{Path: "mail/old.go", StartLine: 1, EndLine: 5, Body: "func Old() {}", Stale: true},
	}}
	resps := runSession(t, idx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_search","arguments":{"query":"троттлинг писем"}}}`)
	if idx.query != "троттлинг писем" {
		t.Errorf("запрос не доехал: %q", idx.query)
	}
	res := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("неожиданная ошибка: %+v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "mail/rate.go:10-20") || !strings.Contains(text, "RateGate") {
		t.Errorf("находка отрисована неверно:\n%s", text)
	}
	if !strings.Contains(text, "stale") {
		t.Errorf("устаревший фрагмент не помечен:\n%s", text)
	}
}

// Недоступный индекс и пустой запрос возвращаются как isError, а не как обрыв:
// агент должен увидеть причину и продолжить работу grep'ом.
func TestToolCallDegradesGracefully(t *testing.T) {
	resps := runSession(t, &fakeIndex{err: errors.New("индекс недоступен")},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_search","arguments":{"query":"что-нибудь"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"code_search","arguments":{"query":"  "}}}`)
	for _, r := range resps {
		res := r["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("ожидался isError: %+v", res)
		}
		if r["error"] != nil {
			t.Errorf("ошибка инструмента не должна быть ошибкой протокола: %+v", r["error"])
		}
	}
}

// Пустая выдача подсказывает агенту следующий шаг, а не молчит.
func TestRenderEmpty(t *testing.T) {
	if got := render(nil); !strings.Contains(got, "Grep") {
		t.Errorf("пустая выдача = %q", got)
	}
}
