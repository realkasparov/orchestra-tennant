package codeindex

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEmbedder — детерминированный «мешок слов»: слово попадает в фиксированную
// координату вектора. Тексты с общими словами оказываются близки, как и у
// настоящей модели, но без сети и без Ollama.
type fakeEmbedder struct{ calls int }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float64, Dims)
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[int(h.Sum32())%Dims] += 1
		}
		out[i] = truncateNormalize(v)
	}
	return out, nil
}

func (f *fakeEmbedder) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	v, err := f.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func newTestIndex(t *testing.T) (*Index, string, *fakeEmbedder) {
	t.Helper()
	root := t.TempDir()
	emb := &fakeEmbedder{}
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"), root, emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix, root, emb
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Полный цикл: индексация, поиск (плотный + лексический), фильтр по модулю.
func TestIndexAndSearch(t *testing.T) {
	ix, root, _ := newTestIndex(t)
	ctx := context.Background()

	writeFile(t, root, "internal/mail/throttle.go", `package mail

// RateGate ограничивает частоту исходящих писем.
func RateGate(limit int) bool {
	return limit > 0
}
`)
	writeFile(t, root, "internal/orders/checkout.go", `package orders

func Checkout(cart string) error {
	return nil
}
`)
	n, err := ix.IndexFiles(ctx, []string{"internal/mail/throttle.go", "internal/orders/checkout.go"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("проиндексировано %d файлов, want 2", n)
	}
	st, err := ix.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Chunks < 2 {
		t.Fatalf("stats = %+v", st)
	}

	// Лексическая половина: точное имя символа обязано находиться.
	res, err := ix.Search(ctx, "RateGate", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].Path != "internal/mail/throttle.go" {
		t.Fatalf("поиск по имени символа: %+v", res)
	}
	if res[0].Stale {
		t.Error("свежий файл помечен устаревшим")
	}
	if res[0].StartLine < 1 || res[0].EndLine < res[0].StartLine {
		t.Errorf("границы строк: %d..%d", res[0].StartLine, res[0].EndLine)
	}

	// Фильтр по модулю отсекает чужие каталоги.
	res, err = ix.Search(ctx, "Checkout", 5, "internal/mail")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if !strings.HasPrefix(r.Path, "internal/mail") {
			t.Errorf("фильтр модуля пропустил %s", r.Path)
		}
	}
}

// Повторная индексация без изменений бесплатна; изменение файла — переиндексирует.
func TestIncrementalReindex(t *testing.T) {
	ix, root, emb := newTestIndex(t)
	ctx := context.Background()
	writeFile(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")

	if _, err := ix.IndexFiles(ctx, []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := emb.calls

	if n, err := ix.IndexFiles(ctx, []string{"a.go"}); err != nil || n != 0 {
		t.Fatalf("повтор без изменений: n=%d err=%v", n, err)
	}
	if emb.calls != callsAfterFirst {
		t.Errorf("неизменённый файл всё равно эмбеддился: %d → %d", callsAfterFirst, emb.calls)
	}

	writeFile(t, root, "a.go", "package a\n\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	if n, err := ix.IndexFiles(ctx, []string{"a.go"}); err != nil || n != 1 {
		t.Fatalf("изменённый файл: n=%d err=%v", n, err)
	}
	res, err := ix.Search(ctx, "Beta", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("новый символ не найден после переиндексации")
	}

	// Удаление файла вычищает его из индекса.
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IndexFiles(ctx, []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	st, _ := ix.Stats()
	if st.Files != 0 || st.Chunks != 0 {
		t.Fatalf("после удаления файла индекс не очищен: %+v", st)
	}
}

// Изменение файла после индексации помечает выдачу устаревшей.
func TestStaleMarking(t *testing.T) {
	ix, root, _ := newTestIndex(t)
	ctx := context.Background()
	writeFile(t, root, "svc.go", "package svc\n\nfunc Handler() {}\n")
	if _, err := ix.IndexFiles(ctx, []string{"svc.go"}); err != nil {
		t.Fatal(err)
	}
	// Правка без переиндексации — ровно ситуация «фон не успел».
	writeFile(t, root, "svc.go", "package svc\n\nfunc Handler() { changed() }\n")

	res, err := ix.Search(ctx, "Handler", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("ничего не найдено")
	}
	if !res[0].Stale {
		t.Error("изменённый после индексации файл должен помечаться Stale")
	}
}

// Запрос с символами синтаксиса FTS5 не должен ронять поиск.
func TestSearchHandlesPunctuation(t *testing.T) {
	ix, root, _ := newTestIndex(t)
	ctx := context.Background()
	writeFile(t, root, "x.go", "package x\n\nfunc Refund(order string) {}\n")
	if _, err := ix.IndexFiles(ctx, []string{"x.go"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`где "обрабатывается" refund (возврат)?`, "NEAR OR AND", "", "-"} {
		if _, err := ix.Search(ctx, q, 5, ""); err != nil {
			t.Errorf("запрос %q уронил поиск: %v", q, err)
		}
	}
}

func TestModuleOf(t *testing.T) {
	for rel, want := range map[string]string{
		"internal/api/server.go": "internal/api",
		"web/src/App.tsx":        "web/src",
		"main.go":                "",
		"cmd/main.go":            "cmd",
	} {
		if got := moduleOf(rel); got != want {
			t.Errorf("moduleOf(%q) = %q, want %q", rel, got, want)
		}
	}
}

// Лексическая половина гибрида обязана работать с кириллицей: комментарии в
// проекте по-русски, и ASCII-разбор запроса обнулял бы половину поиска.
func TestFtsQueryTokenizesCyrillic(t *testing.T) {
	got := ftsQuery("вотчдог зависшего процесса (timeout)")
	for _, want := range []string{`"вотчдог"`, `"зависшего"`, `"timeout"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ftsQuery потерял термин %s: %q", want, got)
		}
	}
	if strings.Contains(got, "(") {
		t.Errorf("синтаксис MATCH просочился: %q", got)
	}
	if got := ftsQuery("!!! ??"); got != "" {
		t.Errorf("запрос без слов = %q, want пусто", got)
	}
}
