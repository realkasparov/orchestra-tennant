package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tennant "github.com/realkasparov/orchestra-tennant"
	"github.com/realkasparov/orchestra-tennant/gitops"
	"github.com/realkasparov/orchestra-tennant/protocol"
)

// Кэш скиллов: встроенные заводятся по хэшу, две версии одного скилла
// лежат рядом, выкладка в папку таски — по имени.
func TestSkillCacheVersions(t *testing.T) {
	c, err := NewSkillCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SeedEmbedded(tennant.Skills); err != nil {
		t.Fatal(err)
	}
	hashes := c.BuiltinHashes()
	for _, n := range protocol.BuiltinSkills {
		if hashes[n] == "" || !c.Has(hashes[n]) {
			t.Errorf("встроенный %s не в кэше", n)
		}
	}
	md := []byte("---\nname: solve-task\ndescription: x\n---\nv1\n")
	man := []byte("name: solve-task\nversion: 1\n")
	h1 := protocol.SkillHash(md, man)
	if err := c.Put(h1, "solve-task", md, man); err != nil {
		t.Fatal(err)
	}
	md2 := []byte("---\nname: solve-task\ndescription: x\n---\nv2\n")
	h2 := protocol.SkillHash(md2, man)
	if err := c.Put(h2, "solve-task", md2, man); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(h2, "solve-task", md, man); err == nil {
		t.Fatal("повреждённый скилл принят")
	}
	t1, t2 := t.TempDir(), t.TempDir()
	if err := c.InstallTo(t1, h1, "solve-task"); err != nil {
		t.Fatal(err)
	}
	if err := c.InstallTo(t2, h2, "solve-task"); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(t1, ".claude", "skills", "solve-task", "SKILL.md"))
	b, _ := os.ReadFile(filepath.Join(t2, ".claude", "skills", "solve-task", "SKILL.md"))
	if !strings.HasSuffix(string(a), "v1\n") || !strings.HasSuffix(string(b), "v2\n") {
		t.Errorf("версии перепутаны: %q %q", a, b)
	}
	if n := len(c.Hashes()); n != len(protocol.BuiltinSkills)+2 {
		t.Errorf("хэшей в кэше %d", n)
	}
}

// Уточнение: пока идёт прогон, сообщение не проходит триаж, а прерывает
// прогон resumable-скилла; для нерезюмируемого — ждёт конца.
func TestClarifyRequest(t *testing.T) {
	var c clarifyRequest
	if c.running() {
		t.Fatal("без прогона не должно быть active")
	}
	cancelled := false
	c.arm(func() { cancelled = true }, true)
	if !c.running() {
		t.Fatal("прогон не отмечен")
	}
	c.request("используй хелпер")
	c.request("и тесты")
	if !cancelled {
		t.Fatal("resumable-прогон не прерван")
	}
	if got := c.take(); got != "используй хелпер\n\nи тесты" {
		t.Errorf("текст: %q", got)
	}
	if c.take() != "" {
		t.Error("уточнение не очищено")
	}
	cancelled = false
	c.arm(func() { cancelled = true }, false)
	c.request("ещё")
	if cancelled {
		t.Fatal("нерезюмируемый прогон прерван")
	}
	c.arm(nil, false)
	if c.running() {
		t.Fatal("после прогона active остался")
	}
}

// Скилл с `workspace: self`: движок фиксирует базу и не заводит ветку, а
// маркер git_branch называет ветку таски и даёт дифф.
func TestSelfWorkspace(t *testing.T) {
	repo := t.TempDir()
	if err := gitops.InitRepo(repo, "main"); err != nil {
		t.Fatal(err)
	}
	man, err := protocol.ParseManifest([]byte(`
name: solve-task
version: 1
inputs:
  TASK_TEXT: {type: text, required: true}
workspace: self
outputs:
  markers:
    BRANCH: {type: git_branch}
    RESULT: {type: enum, values: [done, blocked]}
checks:
  - marker_present: BRANCH
  - has_commits: {branch: BRANCH}
`))
	if err != nil {
		t.Fatal(err)
	}
	plan := &protocol.Plan{SchemaVersion: 2, TaskID: 9, Title: "t", Prompt: "x",
		Project:    protocol.Project{ID: 1, Name: "demo", Path: repo, BaseBranch: "main"},
		Continuity: "non_stop", Workspace: "folder", StageTimeout: 60,
		Skills:     []protocol.SkillRef{{Name: "solve-task", Hash: "h"}},
		Steps: []protocol.Step{
			{Key: "start", Kind: protocol.KindStartTask, Title: "Условие"},
			{Key: "solve", Kind: protocol.KindAgent, Title: "Решение", Skill: "solve-task", SkillHash: "h", Model: "claude-opus-5",
				Bind: map[string]string{"TASK_TEXT": "$task.prompt"}},
			{Key: "end", Kind: protocol.KindFinish, Title: "Результат", Result: "branch"},
		}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	r, _ := testRun(t, plan)
	r.manifests = map[string]*protocol.Manifest{"solve-task": man}
	r.st.addRound(r.stageKeys())
	if got := strings.Join(r.stageKeys(), ","); got != "solve" {
		t.Fatalf("этапы: %s", got)
	}
	def := plan.Step("solve")
	if err := r.prepareWorkspace(def); err != nil {
		t.Fatal(err)
	}
	if !r.st.SelfWorkspace || r.st.WorktreeDir != repo || r.st.BranchName != "" || r.st.BaseCommit == "" {
		t.Fatalf("состояние после подготовки: %+v", r.st)
	}
	if !r.folderMode() {
		t.Fatal("self должен считаться работой в папке проекта")
	}
	// Скилл «сам» завёл ветку с коммитом.
	if _, err := gitops.CheckoutNewBranch(repo, "task-9-feature", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(repo, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "feat"); err != nil {
		t.Fatal(err)
	}
	text := "готово\nBRANCH: task-9-feature\nRESULT: done\n"
	if err := r.runChecks(def, text, true); err != nil {
		t.Fatalf("проверки: %v", err)
	}
	if _, err := r.applyMarkers(def, man, text); err != nil {
		t.Fatal(err)
	}
	if r.st.BranchName != "task-9-feature" || r.st.output("solve", "RESULT") != "done" {
		t.Errorf("ветка %q, выход %q", r.st.BranchName, r.st.output("solve", "RESULT"))
	}
	if err := r.runChecks(def, "BRANCH: main\n", true); err == nil {
		t.Error("ветка без коммитов сверх базы прошла has_commits")
	}
	if _, err := r.applyMarkers(def, man, "BRANCH: -bad\n"); err == nil {
		t.Error("недопустимое имя ветки принято")
	}
}

func runGit(dir string, args ...string) (string, error) {
	cmd := shellCommand(context.Background(), "git "+strings.Join(args, " "), dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Раунд правки начинается с шага rework: шаги до него не сбрасываются,
// артефакты шагов раунда уходят в round<N>/.
func TestReworkKeys(t *testing.T) {
	r, _ := testRun(t, fullPlan())
	if got := strings.Join(r.reworkKeys(), ","); got != "analyze,err_work,branch,execute,test_gate,review,handoff" {
		t.Errorf("ключи раунда правки: %s", got)
	}
	r.plan.Rework = "execute"
	if got := strings.Join(r.reworkKeys(), ","); got != "execute,test_gate,review,handoff" {
		t.Errorf("с rework=execute: %s", got)
	}
	names := r.roundArtifacts(r.reworkKeys())
	if strings.Join(names, ",") != "step04-execution.md,step05-review.md,step06-handoff.md" {
		t.Errorf("артефакты раунда: %v", names)
	}
}
