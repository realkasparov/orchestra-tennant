package gitops

import (
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/realkasparov/orchestra-tennant/protocol"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckRef is a defense-in-depth guard: even though callers sanitize branch
// and base-ref names, a leading '-' would be parsed by git as an option
// (argument injection). Reject it before the name reaches an argv slot.
func CheckRef(name string) error { return protocol.CheckRef(name) }

// HasRemote — у репозитория настроен remote с таким именем.
func HasRemote(repo, name string) bool {
	_, err := run(repo, "remote", "get-url", name)
	return err == nil
}

// FetchBase fetches the base branch from origin.
func FetchBase(repo, baseBranch string) error {
	if err := CheckRef(baseBranch); err != nil {
		return err
	}
	_, err := run(repo, "fetch", "origin", baseBranch)
	return err
}

// AddWorktree creates a worktree on a new branch from origin/<base> (falls
// back to the local base branch for repos without a remote) and returns the
// base commit SHA.
func AddWorktree(repo, dir, branch, baseBranch string) (string, error) {
	if err := CheckRef(branch); err != nil {
		return "", err
	}
	if err := CheckRef(baseBranch); err != nil {
		return "", err
	}
	ref := "origin/" + baseBranch
	if _, err := run(repo, "rev-parse", "--verify", ref); err != nil {
		ref = baseBranch
	}
	sha, err := run(repo, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	if _, err := run(repo, "worktree", "add", dir, "-b", branch, ref); err != nil {
		return "", err
	}
	return sha, nil
}

// CheckoutNewBranch creates a new branch from origin/<base> (or local <base>)
// and checks it out IN the given repo working tree (folder workspace mode).
// Returns the base commit SHA. Fails if the working tree is dirty.
func CheckoutNewBranch(repo, branch, baseBranch string) (string, error) {
	if err := CheckRef(branch); err != nil {
		return "", err
	}
	if err := CheckRef(baseBranch); err != nil {
		return "", err
	}
	if clean, err := IsClean(repo); err != nil {
		return "", err
	} else if !clean {
		return "", fmt.Errorf("в папке проекта есть незакоммиченные изменения — закоммитьте или спрячьте их (git stash) перед запуском в режиме «в папке»")
	}
	ref := "origin/" + baseBranch
	if _, err := run(repo, "rev-parse", "--verify", ref); err != nil {
		ref = baseBranch
	}
	sha, err := run(repo, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	if _, err := run(repo, "checkout", "-b", branch, ref); err != nil {
		return "", err
	}
	return sha, nil
}

// RenameBranch renames the branch checked out in the worktree.
func RenameBranch(worktree, oldName, newName string) error {
	if err := CheckRef(newName); err != nil {
		return err
	}
	_, err := run(worktree, "branch", "-m", oldName, newName)
	return err
}

// RemoveWorktree removes the worktree directory but keeps the feature branch,
// so the task's commits survive deletion of the task.
func RemoveWorktree(repo, dir string) error {
	_, err := run(repo, "worktree", "remove", "--force", dir)
	return err
}

// CurrentBranch — ветка, выставленная в рабочей копии ("HEAD" при
// отсоединённом HEAD).
func CurrentBranch(dir string) (string, error) {
	return run(dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// Checkout выставляет существующую ветку в рабочей копии.
func Checkout(dir, branch string) error {
	if err := CheckRef(branch); err != nil {
		return err
	}
	_, err := run(dir, "checkout", "--", branch)
	if err != nil {
		// «--» после имени ветки git читает как разделитель путей; форма
		// без него — на случай старых версий.
		_, err = run(dir, "checkout", branch)
	}
	return err
}

// CheckoutDetached выставляет коммит ветки отсоединённым HEAD: папка
// показывает код ветки, а сама ветка остаётся там, где выложена (в
// worktree таски), и её можно продолжать.
func CheckoutDetached(dir, branch string) error {
	if err := CheckRef(branch); err != nil {
		return err
	}
	_, err := run(dir, "checkout", "--detach", branch)
	return err
}

// PruneWorktrees снимает записи о рабочих копиях, папок которых больше нет.
func PruneWorktrees(repo string) error {
	_, err := run(repo, "worktree", "prune")
	return err
}

func HeadSHA(dir string) (string, error) { return run(dir, "rev-parse", "HEAD") }

// HasRef reports whether the branch exists locally or on origin.
func HasRef(dir, branch string) bool {
	if CheckRef(branch) != nil {
		return false
	}
	if _, err := run(dir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		return true
	}
	_, err := run(dir, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
	return err == nil
}

func IsClean(dir string) (bool, error) {
	out, err := run(dir, "status", "--porcelain")
	return out == "", err
}

// DirtyFiles — незакоммиченные пути (изменённые, добавленные, неотслеживаемые):
// ошибка «дерево не чистое» должна называть, что именно.
func DirtyFiles(dir string) ([]string, error) {
	out, err := run(dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 3 {
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	return files, nil
}

// InitRepo creates dir (if needed) and initializes a git repository on the
// given branch with an initial empty commit — without a commit the base ref
// doesn't exist and worktree/branch creation would fail. Commit identity falls
// back to a local config entry when git has no global user configured.
func InitRepo(dir, branch string) error {
	if err := CheckRef(branch); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := run(dir, "init", "-b", branch); err != nil {
		return err
	}
	if _, err := run(dir, "var", "GIT_AUTHOR_IDENT"); err != nil {
		if _, err := run(dir, "config", "user.name", "agent-service"); err != nil {
			return err
		}
		if _, err := run(dir, "config", "user.email", "agent-service@localhost"); err != nil {
			return err
		}
	}
	_, err := run(dir, "commit", "--allow-empty", "-m", "init")
	return err
}

// FileDiff describes one changed file between the task's base commit and HEAD.
type FileDiff struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // added | modified | deleted
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"` // empty when the total patch budget is exhausted
}

// maxPatchTotal bounds the summed size of per-file patches returned by Diff, so
// a huge branch diff can't blow up the API response; files past the budget come
// back with stats only.
const maxPatchTotal = 200 << 10

// Diff returns the files changed between base and HEAD of the worktree.
// Renames are reported as delete+add (--no-renames) to keep parsing simple.
func Diff(worktree, base string) ([]FileDiff, error) {
	if err := CheckRef(base); err != nil {
		return nil, err
	}
	names, err := run(worktree, "diff", "--name-status", "--no-renames", base, "HEAD")
	if err != nil {
		return nil, err
	}
	numstat, err := run(worktree, "diff", "--numstat", "--no-renames", base, "HEAD")
	if err != nil {
		return nil, err
	}
	counts := map[string][2]int{}
	for _, line := range strings.Split(numstat, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		a, _ := strconv.Atoi(parts[0]) // "-" (binary) → 0
		d, _ := strconv.Atoi(parts[1])
		counts[parts[2]] = [2]int{a, d}
	}
	files := []FileDiff{}
	total := 0
	for _, line := range strings.Split(names, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		status := "modified"
		switch parts[0] {
		case "A":
			status = "added"
		case "D":
			status = "deleted"
		}
		c := counts[parts[1]]
		fd := FileDiff{Path: parts[1], Status: status, Additions: c[0], Deletions: c[1]}
		if total < maxPatchTotal {
			patch, perr := run(worktree, "diff", "--no-renames", base, "HEAD", "--", parts[1])
			if perr == nil && total+len(patch) <= maxPatchTotal {
				fd.Patch = patch
				total += len(patch)
			}
		}
		files = append(files, fd)
	}
	return files, nil
}

// CloneRepo клонирует repoPath (namespace/project) с хоста в dst. Токен
// передаётся через одноразовый http.extraHeader-заголовок и в конфиге клона
// не сохраняется.
func CloneRepo(hostURL, token, repoPath, dst string) error {
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		entries, _ := os.ReadDir(dst)
		if len(entries) > 0 {
			return fmt.Errorf("папка не пуста: %s", dst)
		}
	}
	src := strings.TrimRight(hostURL, "/") + "/" + strings.Trim(repoPath, "/") + ".git"
	args := []string{}
	if token != "" {
		// basic oauth2:<token>, только на процесс клона
		basic := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
		args = append(args, "-c", "http.extraHeader=Authorization: Basic "+basic)
	}
	args = append(args, "clone", "--", src, dst)
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// DefaultBranch — главная ветка репозитория: та, на которую указывает
// origin/HEAD; без origin — текущая ветка; при отсоединённом HEAD — первая
// из веток репозитория. У свежего клона это ветка по умолчанию хоста.
func DefaultBranch(dir string) (string, error) {
	if out, err := run(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		// origin/HEAD может указывать на ветку, которой уже нет (её удалили
		// на хосте и вычистили локально) — такая главной не считается.
		if b := strings.TrimPrefix(out, "origin/"); b != "" && HasRef(dir, b) {
			return b, nil
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	if cur := strings.TrimSpace(string(out)); cur != "" && cur != "HEAD" {
		return cur, nil
	}
	if bs, berr := Branches(dir); berr == nil && len(bs) > 0 {
		return bs[0], nil
	}
	return "", errors.New("в репозитории нет ни одной ветки")
}

// Branches — ветки репозитория: локальные и из origin, без дублей и без
// служебной origin/HEAD, в алфавитном порядке. По ним человек выбирает
// базовую ветку проекта, поэтому список должен быть тем, что есть на
// самом деле, а не тем, что он помнит.
func Branches(dir string) ([]string, error) {
	out, err := run(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var list []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, "origin/") {
			name = strings.TrimPrefix(name, "origin/")
		}
		if name == "" || name == "HEAD" || seen[name] {
			continue
		}
		seen[name] = true
		list = append(list, name)
	}
	sort.Strings(list)
	return list, nil
}

// ChangedFiles возвращает пути файлов, изменившихся между двумя коммитами
// (относительно корня репозитория). Пустой from означает «весь diff с HEAD~1».
func ChangedFiles(dir, from, to string) ([]string, error) {
	if from == "" || to == "" || from == to {
		return nil, nil
	}
	out, err := run(dir, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
