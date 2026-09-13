package codeindex

import (
	"strings"
	"testing"
)

// Чанки режутся по объявлениям, нумеруются строками и несут контекст.
func TestChunkFileByDeclarations(t *testing.T) {
	src := `package store

import "database/sql"

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// одна
	// две
	// три
	return &Store{}, nil
}

func (s *Store) Close() error {
	// закрываем
	return nil
}
`
	chunks := ChunkFile("internal/store/store.go", []byte(src))
	if len(chunks) < 2 {
		t.Fatalf("ожидалось несколько чанков, got %d", len(chunks))
	}
	if !hasSymbol(chunks, "Open") || !hasSymbol(chunks, "Close") {
		t.Fatalf("объявления потеряны: %+v", symbolsOf(chunks))
	}
	// Границы чанков не должны терять код: конкатенация тел == исходник.
	var joined strings.Builder
	for _, c := range chunks {
		joined.WriteString(c.Body)
		if c.StartLine < 1 || c.EndLine < c.StartLine {
			t.Errorf("некорректные строки %d..%d для %v", c.StartLine, c.EndLine, c.Symbols)
		}
	}
	if joined.String() != src {
		t.Errorf("файл не покрыт чанками целиком:\n%q", joined.String())
	}

	text := chunks[0].Text()
	if !strings.Contains(text, "file: internal/store/store.go") {
		t.Errorf("в заголовке нет пути:\n%s", text)
	}
	if !strings.Contains(text, "declares:") {
		t.Errorf("в заголовке нет объявлений:\n%s", text)
	}
}

// Длинная функция режется на части, каждая — в пределах потолка.
func TestChunkFileSplitsLongBodies(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package big\n\nfunc Huge() {\n")
	for i := 0; i < 400; i++ {
		sb.WriteString("\tdoSomethingUseful(iterationNumber)\n")
	}
	sb.WriteString("}\n")

	chunks := ChunkFile("big.go", []byte(sb.String()))
	if len(chunks) < 2 {
		t.Fatalf("длинная функция должна быть разрезана, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Body) > maxChunkBytes+200 {
			t.Errorf("чанк превысил потолок: %d байт", len(c.Body))
		}
	}
}

// Файл без распознанных объявлений режется по размеру, а не теряется.
func TestChunkFileFallback(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("строка документации проекта с описанием поведения\n")
	}
	chunks := ChunkFile("docs/guide.md", []byte(sb.String()))
	if len(chunks) == 0 {
		t.Fatal("markdown потерялся целиком")
	}
	if got := ChunkFile("empty.go", nil); got != nil {
		t.Errorf("пустой файл = %v, want nil", got)
	}
}

func symbolsOf(chunks []Chunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, c.Symbols...)
	}
	return out
}

func hasSymbol(chunks []Chunk, want string) bool {
	for _, s := range symbolsOf(chunks) {
		if s == want {
			return true
		}
	}
	return false
}
