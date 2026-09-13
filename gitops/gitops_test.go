package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRef(t *testing.T) {
	valid := []string{"tn/core/tradernet#42546-add-x", "task-7-wip", "develop"}
	for _, b := range valid {
		if err := CheckRef(b); err != nil {
			t.Errorf("expected %q valid: %v", b, err)
		}
	}
	// Leading dash would be parsed by git as an option (argument injection).
	invalid := []string{"", "-D", "--force", "-x"}
	for _, b := range invalid {
		if err := CheckRef(b); err == nil {
			t.Errorf("expected %q rejected", b)
		}
	}
}

// initRepo builds a repo with one base commit containing keep.txt and del.txt.
func initRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	for _, f := range []string{"keep.txt", "del.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("a\nb\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-m", "base")
	base, err := HeadSHA(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, base
}

func TestInitRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "fresh")
	if err := InitRepo(dir, "main"); err != nil {
		t.Fatal(err)
	}
	branch, err := run(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}
	// HEAD существует (первый коммит) — иначе worktree от базовой ветки не создать.
	if _, err := HeadSHA(dir); err != nil {
		t.Errorf("no initial commit: %v", err)
	}
	if err := InitRepo(filepath.Join(t.TempDir(), "x"), "--force"); err == nil {
		t.Error("expected rejection of option-like branch")
	}
}

func TestDiff(t *testing.T) {
	dir, base := initRepo(t)
	git := func(args ...string) {
		t.Helper()
		if _, err := run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Empty diff: base == HEAD.
	files, err := Diff(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected empty diff, got %v", files)
	}

	// modify keep.txt, delete del.txt, add new.txt.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("a\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "change")

	files, err = Diff(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
	if f := byPath["keep.txt"]; f.Status != "modified" || f.Additions != 2 || f.Deletions != 1 {
		t.Errorf("keep.txt: %+v", f)
	}
	if f := byPath["del.txt"]; f.Status != "deleted" || f.Deletions != 2 {
		t.Errorf("del.txt: %+v", f)
	}
	if f := byPath["new.txt"]; f.Status != "added" || f.Additions != 1 {
		t.Errorf("new.txt: %+v", f)
	}
	if !strings.Contains(byPath["keep.txt"].Patch, "+c") {
		t.Errorf("keep.txt patch missing +c: %q", byPath["keep.txt"].Patch)
	}

	// Injection guard on base ref.
	if _, err := Diff(dir, "--force"); err == nil {
		t.Error("expected rejection of option-like base ref")
	}
}
