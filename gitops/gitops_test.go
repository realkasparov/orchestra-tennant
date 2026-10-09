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

func TestDirtyFilesAndCheckout(t *testing.T) {
	dir, _ := initRepo(t)
	const base = "main"
	// Изменённый отслеживаемый файл с именем в один символ: у « M a» ведущий
	// пробел значим, и такая запись не должна теряться.
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(dir, "add", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(dir, "commit", "-m", "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := DirtyFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dirty, ",") != "a,new.txt" {
		t.Fatalf("dirty = %v", dirty)
	}
	if _, err := run(dir, "checkout", "."); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "new.txt"))

	// Папка с именем базовой ветки: checkout должен переключать ветку, а не
	// трактовать имя как путь.
	if _, err := run(dir, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base, "f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(dir, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := run(dir, "commit", "-m", "dir"); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(dir, base); err != nil {
		t.Fatal(err)
	}
	if cur, _ := CurrentBranch(dir); cur != base {
		t.Fatalf("branch = %q, want %q", cur, base)
	}
}

func TestSameRepo(t *testing.T) {
	cases := []struct {
		remote, host, path string
		want               bool
	}{
		{"git@gitlab.com:nov_aleks/snake-game.git", "https://gitlab.com", "nov_aleks/snake-game", true},
		{"https://gitlab.com/nov_aleks/snake-game.git", "https://gitlab.com/", "/nov_aleks/snake-game/", true},
		{"ssh://git@gitlab.com:2222/nov_aleks/snake-game", "https://gitlab.com", "Nov_Aleks/Snake-Game", true},
		{"https://oauth2:tok@gitlab.com/nov_aleks/snake-game.git", "https://gitlab.com", "nov_aleks/snake-game", true},
		{"git@github.com:nov_aleks/snake-game.git", "https://gitlab.com", "nov_aleks/snake-game", false},
		{"git@gitlab.com:nov_aleks/other.git", "https://gitlab.com", "nov_aleks/snake-game", false},
		{"", "https://gitlab.com", "nov_aleks/snake-game", false},
		// Хост под подпутём: клон лежит по hostURL + repoPath.
		{"https://example.com/gitlab/a/b.git", "https://example.com/gitlab", "a/b", true},
		{"https://example.com/a/b.git", "https://example.com/gitlab", "a/b", false},
		// IPv6 в скобках: хосты различаются, порт не мешает.
		{"https://[::1]:8080/a/b", "https://[::1]", "a/b", true},
		{"https://[2001:db8::1]/a/b", "https://[::1]", "a/b", false},
		{"git@[::1]:a/b.git", "https://[::1]", "a/b", true},
	}
	for _, c := range cases {
		if got := SameRepo(c.remote, c.host, c.path); got != c.want {
			t.Errorf("SameRepo(%q, %q, %q) = %v, want %v", c.remote, c.host, c.path, got, c.want)
		}
	}
}

func TestRedactRemote(t *testing.T) {
	if got := RedactRemote("https://oauth2:secret@gitlab.com/g/x.git"); got != "gitlab.com/g/x" {
		t.Fatalf("redact = %q", got)
	}
	if got := RedactRemote("git@gitlab.com:g/x.git"); got != "gitlab.com/g/x" {
		t.Fatalf("redact scp = %q", got)
	}
}

// Ветка таски от origin/<base> пушится в одноимённую ветку, а не в базу:
// при создании worktree, после переименования и для старой ветки,
// отслеживающей базу.
func TestTaskBranchPushesToOwnName(t *testing.T) {
	dir, _ := initRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	git := func(d string, args ...string) string {
		t.Helper()
		out, err := run(d, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git(dir, "init", "--bare", remote)
	git(dir, "remote", "add", "origin", remote)
	git(dir, "push", "-u", "origin", "main")
	git(dir, "fetch", "origin")

	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := AddWorktree(dir, wt, "task/1-wip", "main"); err != nil {
		t.Fatal(err)
	}
	check := func(branch string) {
		t.Helper()
		if m := git(dir, "config", "--get", "branch."+branch+".merge"); m != "refs/heads/"+branch {
			t.Fatalf("%s: merge = %q", branch, m)
		}
		if r := git(dir, "config", "--get", "branch."+branch+".remote"); r != "origin" {
			t.Fatalf("%s: remote = %q", branch, r)
		}
	}
	check("task/1-wip")
	if err := RenameBranch(wt, "task/1-wip", "PROJ-1-fix"); err != nil {
		t.Fatal(err)
	}
	check("PROJ-1-fix")
	git(wt, "commit", "--allow-empty", "-m", "agent")
	git(wt, "-c", "push.default=simple", "push")
	if got, want := git(dir, "ls-remote", "origin", "refs/heads/PROJ-1-fix"), git(wt, "rev-parse", "HEAD"); !strings.HasPrefix(got, want) {
		t.Fatalf("push не создал одноимённую ветку: %q", got)
	}
	if got := git(dir, "rev-parse", "origin/main"); got == git(wt, "rev-parse", "HEAD") {
		t.Fatal("push ушёл в базу")
	}

	// Старая ветка, отслеживающая базу, перенаправляется; чужой выбор — нет.
	git(dir, "branch", "--track", "PROJ-2-old", "origin/main")
	if err := TrackOwnName(dir, "PROJ-2-old", "main"); err != nil {
		t.Fatal(err)
	}
	check("PROJ-2-old")
	git(dir, "branch", "--track", "PROJ-3-mine", "origin/main")
	git(dir, "config", "branch.PROJ-3-mine.merge", "refs/heads/release")
	if err := TrackOwnName(dir, "PROJ-3-mine", "main"); err != nil {
		t.Fatal(err)
	}
	if m := git(dir, "config", "--get", "branch.PROJ-3-mine.merge"); m != "refs/heads/release" {
		t.Fatalf("выбор человека перезаписан: %q", m)
	}
}

// Upstream своего имени на другом remote (форк человека) не перенаправляется
// на origin.
func TestTrackOwnNameKeepsFork(t *testing.T) {
	dir, _ := initRepo(t)
	for _, args := range [][]string{
		{"remote", "add", "origin", "https://example.invalid/o.git"},
		{"remote", "add", "fork", "https://example.invalid/f.git"},
		{"branch", "PROJ-1-fix"},
		{"config", "branch.PROJ-1-fix.remote", "fork"},
		{"config", "branch.PROJ-1-fix.merge", "refs/heads/PROJ-1-fix"},
	} {
		if _, err := run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := TrackOwnName(dir, "PROJ-1-fix", "main"); err != nil {
		t.Fatal(err)
	}
	if r, _ := run(dir, "config", "--get", "branch.PROJ-1-fix.remote"); r != "fork" {
		t.Fatalf("форк перезаписан: %q", r)
	}
	// Базу на другом remote человек выбрал сам — тоже не трогаем.
	for _, args := range [][]string{
		{"branch", "PROJ-2-up"},
		{"config", "branch.PROJ-2-up.remote", "fork"},
		{"config", "branch.PROJ-2-up.merge", "refs/heads/main"},
	} {
		if _, err := run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := TrackOwnName(dir, "PROJ-2-up", "main"); err != nil {
		t.Fatal(err)
	}
	if m, _ := run(dir, "config", "--get", "branch.PROJ-2-up.merge"); m != "refs/heads/main" {
		t.Fatalf("upstream другого remote перезаписан: %q", m)
	}
}
