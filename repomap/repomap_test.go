package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Карта извлекает объявления, ранжирует файлы по внешним ссылкам и не
// заглядывает в каталоги вендоренного/сгенерированного кода.
func TestBuildRanksAndFilters(t *testing.T) {
	dir := t.TempDir()
	// store.go объявляет символы, на которые ссылаются два других файла.
	write(t, dir, "store/store.go", "package store\n\ntype Store struct{}\n\nfunc Open(path string) *Store { return nil }\n")
	write(t, dir, "api/server.go", "package api\n\nimport \"x/store\"\n\nfunc New(s *store.Store) {}\n\nfunc handle() { Open(\"db\") }\n")
	write(t, dir, "cmd/main.go", "package main\n\nfunc main() { Open(\"db\") }\n")
	// Лист без входящих ссылок.
	write(t, dir, "util/lonely.go", "package util\n\nfunc Unreferenced() {}\n")
	// Не должно попасть в карту.
	write(t, dir, "node_modules/pkg/index.js", "export function shouldNotAppear() {}\n")
	write(t, dir, "README.md", "# not code\n")

	m, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalFiles != 4 {
		t.Fatalf("TotalFiles = %d, want 4 (только файлы с кодом)", m.TotalFiles)
	}
	if got := m.Files[0].Path; got != filepath.Join("store", "store.go") {
		t.Fatalf("самый ссылаемый файл = %q, want store/store.go", got)
	}
	if m.Files[0].Rank != 2 {
		t.Fatalf("rank store.go = %d, want 2", m.Files[0].Rank)
	}
	if last := m.Files[len(m.Files)-1]; last.Rank != 0 {
		t.Fatalf("файл без ссылок должен иметь ранг 0, got %+v", last)
	}

	out := m.Render(0)
	if !strings.Contains(out, "Store") || !strings.Contains(out, "Open") {
		t.Fatalf("символы потерялись:\n%s", out)
	}
	if strings.Contains(out, "shouldNotAppear") {
		t.Errorf("node_modules просочился в карту:\n%s", out)
	}
	if strings.Contains(out, "README") {
		t.Errorf("не-код просочился в карту:\n%s", out)
	}
}

// Бюджет соблюдается: важные файлы остаются, карта помечается усечённой.
func TestRenderBudget(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "core.go", "package core\n\nfunc Central() {}\n")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, dir, n+".go", "package x\n\nfunc "+strings.ToUpper(n)+"unc() { Central() }\n")
	}

	m, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Бюджет считаем от заголовка, чтобы тест не ломался от правки его текста:
	// хватает на заголовок и одну строку файла.
	budget := len(header(1, m.TotalFiles)) + 20
	out := m.Render(budget)
	if len(out) > budget {
		t.Fatalf("бюджет не соблюдён: %d > %d байт\n%s", len(out), budget, out)
	}
	if !strings.Contains(out, "core.go") {
		t.Errorf("самый важный файл должен попасть в усечённую карту:\n%s", out)
	}
	if !m.Truncated {
		t.Error("Truncated не выставлен")
	}
	// Пустая карта не выдаёт заголовок «в никуда».
	if got := (&Map{}).Render(0); got != "" {
		t.Errorf("пустая карта = %q, want \"\"", got)
	}
	// Бюджет меньше заголовка — честнее отдать пусто, чем превысить лимит.
	if got := m.Render(50); got != "" {
		t.Errorf("бюджет меньше заголовка = %q, want \"\"", got)
	}
}

// Символы разных языков распознаются.
func TestExtractSymbolsLanguages(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"a.ts":  {"export class UserService {}\nexport const API_URL = '/x';\n", "UserService"},
		"b.py":  {"class Widget:\n    def render(self):\n        pass\n", "Widget"},
		"c.rb":  {"class Invoice\n  def total\n  end\nend\n", "Invoice"},
		"d.rs":  {"pub fn parse_config() {}\nstruct Config {}\n", "parse_config"},
		"e.php": {"<?php\nclass Cart {}\npublic function checkout() {}\n", "Cart"},
		"f.txt": {"nothing here", ""},
	}
	for name, c := range cases {
		got := extractSymbols(name, []byte(c.body))
		if c.want == "" {
			if got != nil {
				t.Errorf("%s: ожидались нулевые символы, got %v", name, got)
			}
			continue
		}
		if !contains(got, c.want) {
			t.Errorf("%s: %v не содержит %q", name, got, c.want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
