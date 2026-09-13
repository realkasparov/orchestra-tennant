package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir())
	m.newEmb = func() Embedding { return &fakeEmbedder{} }
	m.ThresholdBytes = 50 << 10 // 50 КБ: «большой» репозиторий в тесте компактен
	t.Cleanup(m.Close)
	return m
}

// bigRepo создаёт проект заведомо крупнее порога «авто».
func bigRepo(t *testing.T, files int) string {
	t.Helper()
	root := t.TempDir()
	body := strings.Repeat("\t// строка тела функции с достаточной длиной для объёма\n", 60)
	for i := 0; i < files; i++ {
		writeFile(t, root, filepath.Join("mod", "f"+itoa(i)+".go"),
			"package mod\n\nfunc Handler"+itoa(i)+"() {\n"+body+"}\n")
	}
	return root
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// waitIdle ждёт, пока фоновая очередь опустеет.
func waitIdle(t *testing.T, m *Manager, id int64) *Status {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status(id)
		if !st.Building && st.Queued == 0 && st.Files > 0 {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("индексация не завершилась: %+v", m.Status(id))
	return nil
}

// Режим «авто»: маленький проект индекса не получает, большой — получает.
func TestEnsureAutoThreshold(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	small := t.TempDir()
	writeFile(t, small, "main.go", "package main\n\nfunc main() {}\n")
	st, err := m.Ensure(ctx, 1, small, ModeAuto, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Errorf("маленький проект не должен индексироваться: %+v", st)
	}
	if st.Reason == "" {
		t.Error("причина отказа должна объяснять пользователю, почему индекса нет")
	}

	// Тот же маленький проект с режимом «всегда» — индексируется.
	st, err = m.Ensure(ctx, 2, small, ModeAlways, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled {
		t.Fatalf("режим always должен включать индекс: %+v", st)
	}
	waitIdle(t, m, 2)
}

// Большой проект индексируется в фоне и ищется.
func TestEnsureBuildsAndSearches(t *testing.T) {
	m := newTestManager(t)
	root := bigRepo(t, 40)
	if _, err := m.Ensure(context.Background(), 7, root, ModeAuto, nil); err != nil {
		t.Fatal(err)
	}
	st := waitIdle(t, m, 7)
	if st.Files != 40 || st.Chunks < 40 {
		t.Fatalf("индекс неполон: %+v", st)
	}
	res, err := m.Search(context.Background(), 7, "Handler7", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("поиск по индексу ничего не вернул")
	}
}

// Исключения не попадают в индекс, а изменённые файлы — попадают.
func TestExcludesAndIncrementalEnqueue(t *testing.T) {
	m := newTestManager(t)
	root := bigRepo(t, 30)
	writeFile(t, root, "mod/generated.pb.go", "package mod\n\nfunc Generated() {}\n")

	if _, err := m.Ensure(context.Background(), 3, root, ModeAlways, []string{"*.pb.go"}); err != nil {
		t.Fatal(err)
	}
	st := waitIdle(t, m, 3)
	if st.Files != 30 {
		t.Fatalf("исключённый файл попал в индекс: %+v", st)
	}

	// Изменение обычного файла ставится в очередь, исключённого — нет.
	writeFile(t, root, "mod/f0.go", "package mod\n\nfunc Handler0() { renamedBehaviour() }\n")
	m.EnqueueChanged(3, []string{"mod/f0.go", "mod/generated.pb.go", "README.md"})
	waitIdle(t, m, 3)
	if got := m.Status(3).Files; got != 30 {
		t.Errorf("после инкремента файлов = %d, want 30", got)
	}
	res, err := m.Search(context.Background(), 3, "renamedBehaviour", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Error("изменение не переиндексировалось")
	}
}

// Режим «никогда» закрывает индекс и удаляет его файл.
func TestModeNeverRemovesIndex(t *testing.T) {
	m := newTestManager(t)
	root := bigRepo(t, 30)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, 5, root, ModeAlways, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m, 5)
	dbFile := m.dbPath(5)
	if _, err := os.Stat(dbFile); err != nil {
		t.Fatalf("файл индекса не создан: %v", err)
	}

	if _, err := m.Ensure(ctx, 5, root, ModeNever, nil); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(5); st.Enabled {
		t.Errorf("после выключения индекс активен: %+v", st)
	}
	if _, err := os.Stat(dbFile); !os.IsNotExist(err) {
		t.Errorf("файл индекса не удалён: %v", err)
	}
	if _, err := m.Search(ctx, 5, "Handler1", 5, ""); err == nil {
		t.Error("поиск по выключенному индексу должен возвращать ошибку")
	}
}

func TestExcluded(t *testing.T) {
	cases := []struct {
		rel      string
		patterns []string
		want     bool
	}{
		{"internal/api/server.go", []string{"*.pb.go"}, false},
		{"internal/api/api.pb.go", []string{"*.pb.go"}, true},
		{"db/migrations/001.sql", []string{"migrations"}, true},
		{"web/dist/bundle.js", []string{"web/dist/"}, true},
		{"web/src/App.tsx", []string{"web/dist/"}, false},
		{"a.go", nil, false},
		{"a.go", []string{"", "   "}, false},
	}
	for _, c := range cases {
		if got := excluded(c.rel, c.patterns); got != c.want {
			t.Errorf("excluded(%q, %v) = %v, want %v", c.rel, c.patterns, got, c.want)
		}
	}
}
