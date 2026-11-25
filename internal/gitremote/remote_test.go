package gitremote_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"caravan/internal/gitremote"
	"caravan/internal/manifest"
)

// captureStdout redirects os.Stdout during f and returns the captured output.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	f()

	w.Close()
	os.Stdout = orig
	out := <-outC
	r.Close()
	return out
}

// gitRun runs a git command and fatals on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initBareRepo creates a bare git repo.
func initBareRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cmd := exec.Command("git", "init", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", dir, err, out)
	}
}

// initWorkRepo creates a working repo with an initial commit and a remote.
func initWorkRepo(t *testing.T, dir, publicBareDir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	run := func(args ...string) {
		gitRun(t, dir, args...)
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("remote", "add", "origin", publicBareDir)
	// Create initial commit on main.
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	run("push", "-u", "origin", "main")
}

// writeManifest creates a caravan.toml with the given repo.
func writeManifest(t *testing.T, dir string, r manifest.Repo, wsRoot string) string {
	t.Helper()
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Repos:     []manifest.Repo{r},
	}
	path := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save manifest: %v", err)
	}
	return path
}

// TestRemoteSetupAddsRemoteAndHook verifies that `caravan remote setup` adds
// the private git remote and installs the pre-push hook.
func TestRemoteSetupAddsRemoteAndHook(t *testing.T) {
	tmp := t.TempDir()
	publicBare := filepath.Join(tmp, "public.git")
	privateBare := filepath.Join(tmp, "private.git")
	workDir := filepath.Join(tmp, "ws", "myrepo")

	initBareRepo(t, publicBare)
	initBareRepo(t, privateBare)
	initWorkRepo(t, workDir, publicBare)

	repo := manifest.Repo{
		Name:          "myrepo",
		URL:           publicBare,
		PrivateRemote: "private",
		PrivateURL:    privateBare,
	}
	manifestPath := writeManifest(t, tmp, repo, filepath.Join(tmp, "ws"))
	t.Setenv("CARAVAN_MANIFEST", manifestPath)

	code := gitremote.CmdRemote([]string{"setup"})
	if code != 0 {
		t.Fatalf("CmdRemote setup returned %d", code)
	}

	// Assert private remote was added.
	out := gitRun(t, workDir, "remote", "get-url", "private")
	if out != privateBare {
		t.Errorf("private remote URL = %q, want %q", out, privateBare)
	}

	// Assert hook was installed.
	hookPath := filepath.Join(workDir, ".git", "hooks", "pre-push")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("pre-push hook not written: %v", err)
	}
	if !strings.Contains(string(data), "caravan guard-prepush") {
		t.Errorf("hook doesn't call caravan guard-prepush; got:\n%s", data)
	}
}

// TestDecisionBlocksPrivatePushToPublic tests the pure decision logic directly.
func TestDecisionBlocksPrivatePushToPublic(t *testing.T) {
	globs := []string{"private/*", "embargo/*"}
	block, reason := gitremote.Decision("embargo/cve-2026-0001", "origin", "origin", globs, "")
	if !block {
		t.Error("expected Decision to block private→public push")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestPublishPromotesPrivateBranch verifies that `caravan publish` pushes a
// private branch to the public bare repo.
func TestPublishPromotesPrivateBranch(t *testing.T) {
	tmp := t.TempDir()
	publicBare := filepath.Join(tmp, "public.git")
	privateBare := filepath.Join(tmp, "private.git")
	workDir := filepath.Join(tmp, "ws", "myrepo")

	initBareRepo(t, publicBare)
	initBareRepo(t, privateBare)
	initWorkRepo(t, workDir, publicBare)

	// Add private remote.
	gitRun(t, workDir, "remote", "add", "private", privateBare)

	// Create a private branch with a commit and push to private bare.
	gitRun(t, workDir, "checkout", "-b", "embargo/cve-2026-0001")
	fixFile := filepath.Join(workDir, "fix.md")
	if err := os.WriteFile(fixFile, []byte("security fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workDir, "add", "fix.md")
	gitRun(t, workDir, "commit", "-m", "apply security fix")
	gitRun(t, workDir, "push", "private", "embargo/cve-2026-0001")

	// Override publish-schedule dir for this test.
	schedDir := filepath.Join(tmp, "publish-schedule")
	orig := gitremote.PublishScheduleDir
	gitremote.PublishScheduleDir = schedDir
	t.Cleanup(func() { gitremote.PublishScheduleDir = orig })

	repo := manifest.Repo{
		Name:          "myrepo",
		URL:           publicBare,
		PrivateRemote: "private",
		PrivateURL:    privateBare,
	}
	manifestPath := writeManifest(t, tmp, repo, filepath.Join(tmp, "ws"))
	t.Setenv("CARAVAN_MANIFEST", manifestPath)

	code := gitremote.CmdPublish([]string{"embargo/cve-2026-0001", "--onto", "main"})
	if code != 0 {
		t.Fatalf("CmdPublish returned %d", code)
	}

	// Verify the fix appears on public main.
	out := gitRun(t, publicBare, "log", "--oneline", "main")
	if !strings.Contains(out, "apply security fix") {
		t.Errorf("security fix not found on public main; log:\n%s", out)
	}
}

// TestScheduleAndRunDue verifies that scheduling a publish record and then
// firing --run-due (with a backdated record) actually publishes.
func TestScheduleAndRunDue(t *testing.T) {
	tmp := t.TempDir()
	publicBare := filepath.Join(tmp, "public.git")
	privateBare := filepath.Join(tmp, "private.git")
	workDir := filepath.Join(tmp, "ws", "myrepo")

	initBareRepo(t, publicBare)
	initBareRepo(t, privateBare)
	initWorkRepo(t, workDir, publicBare)
	gitRun(t, workDir, "remote", "add", "private", privateBare)

	// Create private branch and push it.
	gitRun(t, workDir, "checkout", "-b", "embargo/scheduled")
	f := filepath.Join(workDir, "sched.md")
	if err := os.WriteFile(f, []byte("scheduled fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workDir, "add", "sched.md")
	gitRun(t, workDir, "commit", "-m", "scheduled security fix")
	gitRun(t, workDir, "push", "private", "embargo/scheduled")

	schedDir := filepath.Join(tmp, "publish-schedule")
	orig := gitremote.PublishScheduleDir
	gitremote.PublishScheduleDir = schedDir
	t.Cleanup(func() { gitremote.PublishScheduleDir = orig })

	repo := manifest.Repo{
		Name:          "myrepo",
		URL:           publicBare,
		PrivateRemote: "private",
		PrivateURL:    privateBare,
	}
	manifestPath := writeManifest(t, tmp, repo, filepath.Join(tmp, "ws"))
	t.Setenv("CARAVAN_MANIFEST", manifestPath)

	// Schedule with a past timestamp (to test run-due fires it).
	code := gitremote.CmdPublish([]string{"embargo/scheduled", "--onto", "main", "--at", "2020-01-01T00:00:00Z"})
	if code != 0 {
		t.Fatalf("CmdPublish (schedule past) returned %d", code)
	}

	// run-due should pick it up and fire.
	code = gitremote.CmdPublish([]string{"--run-due"})
	if code != 0 {
		t.Fatalf("CmdPublish --run-due returned %d", code)
	}

	out := gitRun(t, publicBare, "log", "--oneline", "main")
	if !strings.Contains(out, "scheduled security fix") {
		t.Errorf("scheduled fix not found on public main; log:\n%s", out)
	}
}
