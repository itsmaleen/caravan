package provision_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"caravan/internal/manifest"
	"caravan/internal/provision"
	"caravan/internal/secrets"
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

// makeTestManifest creates a manifest in dir, with repos at dirs under wsRoot.
func makeTestManifest(t *testing.T, dir, wsRoot string, repos []manifest.Repo) string {
	t.Helper()
	path := filepath.Join(dir, "caravan.toml")
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Repos:     repos,
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save manifest: %v", err)
	}
	return path
}

// initGitRepo creates an initialized git repo (with an empty commit) at dir.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "init")
}

// ── CmdUp dry-run tests ────────────────────────────────────────────────────

func TestCmdUpDryRunMissing(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: "https://example.com/myrepo.git"},
	})

	out := captureStdout(t, func() {
		code := provision.CmdUp([]string{"--dry-run", "-f", manifestPath})
		if code != 0 {
			t.Errorf("CmdUp --dry-run returned %d", code)
		}
	})

	if !strings.Contains(out, "would clone") {
		t.Errorf("expected 'would clone' in output; got:\n%s", out)
	}

	// Verify nothing was created.
	if _, err := os.Stat(filepath.Join(wsRoot, "myrepo")); !os.IsNotExist(err) {
		t.Error("dry-run should not create the repo dir")
	}
}

func TestCmdUpDryRunExisting(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")

	// Pre-create the repo.
	repoDir := filepath.Join(wsRoot, "myrepo")
	initGitRepo(t, repoDir)

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: "https://example.com/myrepo.git"},
	})

	out := captureStdout(t, func() {
		code := provision.CmdUp([]string{"--dry-run", "-f", manifestPath})
		if code != 0 {
			t.Errorf("CmdUp --dry-run returned %d", code)
		}
	})

	if !strings.Contains(out, "would pull") {
		t.Errorf("expected 'would pull' in output; got:\n%s", out)
	}
}

func TestCmdUpDryRunOccupied(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")

	// Create a non-git directory.
	occupied := filepath.Join(wsRoot, "myrepo")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: "https://example.com/myrepo.git"},
	})

	out := captureStdout(t, func() {
		code := provision.CmdUp([]string{"--dry-run", "-f", manifestPath})
		// Should exit 1 for the occupied path.
		if code == 0 {
			t.Error("expected non-zero exit for occupied path")
		}
	})

	if !strings.Contains(out, "path occupied") {
		t.Errorf("expected 'path occupied' in output; got:\n%s", out)
	}
}

func TestCmdUpOnly(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "alpha", URL: "https://example.com/alpha.git"},
		{Name: "beta", URL: "https://example.com/beta.git"},
	})

	out := captureStdout(t, func() {
		provision.CmdUp([]string{"--dry-run", "--only", "alpha", "-f", manifestPath})
	})

	if !strings.Contains(out, "alpha") {
		t.Errorf("expected 'alpha' in output; got:\n%s", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("unexpected 'beta' in --only alpha output; got:\n%s", out)
	}
}

// ── Clone from local bare repo ─────────────────────────────────────────────

func TestCmdUpCloneLocal(t *testing.T) {
	dir := t.TempDir()

	// Create a source repo we can clone from.
	sourceDir := filepath.Join(dir, "source")
	initGitRepo(t, sourceDir)

	wsRoot := filepath.Join(dir, "ws")
	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: sourceDir},
	})

	var code int
	out := captureStdout(t, func() {
		code = provision.CmdUp([]string{"-f", manifestPath})
	})
	if code != 0 {
		t.Errorf("CmdUp returned %d; output:\n%s", code, out)
	}

	cloneDir := filepath.Join(wsRoot, "myrepo")
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		t.Errorf("expected cloned repo at %s: %v", cloneDir, err)
	}
	if !strings.Contains(out, "cloned") {
		t.Errorf("expected 'cloned' in output; got:\n%s", out)
	}
}

func TestCmdUpPullLocal(t *testing.T) {
	dir := t.TempDir()

	// Create source and clone it.
	sourceDir := filepath.Join(dir, "source")
	initGitRepo(t, sourceDir)

	wsRoot := filepath.Join(dir, "ws")
	cloneDir := filepath.Join(wsRoot, "myrepo")

	// Pre-clone.
	if out, err := exec.Command("git", "clone", sourceDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("pre-clone failed: %v\n%s", err, out)
	}

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: sourceDir},
	})

	var code int
	out := captureStdout(t, func() {
		code = provision.CmdUp([]string{"-f", manifestPath})
	})
	if code != 0 {
		t.Errorf("CmdUp returned %d; output:\n%s", code, out)
	}

	if !strings.Contains(out, "up-to-date") {
		t.Errorf("expected 'up-to-date' in output; got:\n%s", out)
	}
}

// ── .env writing ──────────────────────────────────────────────────────────

func TestWriteEnvMerge(t *testing.T) {
	// This test exercises the writeEnv logic indirectly via CmdUp.
	// We need a secrets file; skip if secrets aren't set up for this test.
	// Instead test the env file directly via a simulated scenario:
	// create existing .env, run up against a local repo, expect merge.
	//
	// For simplicity, verify the correct output for an already-cloned repo
	// that has no secrets configured (secretsPath = "").

	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	initGitRepo(t, sourceDir)

	wsRoot := filepath.Join(dir, "ws")
	cloneDir := filepath.Join(wsRoot, "myrepo")
	if out, err := exec.Command("git", "clone", sourceDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("pre-clone: %v\n%s", err, out)
	}

	// Pre-write a .env.
	envPath := filepath.Join(cloneDir, ".env")
	if err := os.WriteFile(envPath, []byte("EXISTING=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: sourceDir},
	})

	captureStdout(t, func() {
		provision.CmdUp([]string{"-f", manifestPath})
	})

	// .env should still have EXISTING (no secrets to overwrite it).
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("reading .env: %v", err)
	}
	if !strings.Contains(string(data), "EXISTING=value") {
		t.Errorf("EXISTING key was lost from .env; contents:\n%s", data)
	}
}

// ── CmdStatus tests ────────────────────────────────────────────────────────

func TestCmdStatusMissingRepo(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "missing", URL: "https://example.com/x.git"},
	})

	out := captureStdout(t, func() {
		code := provision.CmdStatus([]string{"-f", manifestPath})
		if code != 0 {
			t.Errorf("CmdStatus returned %d", code)
		}
	})

	if !strings.Contains(out, "missing") || !strings.Contains(out, "✗") {
		t.Errorf("expected missing repo to show ✗; got:\n%s", out)
	}
}

func TestCmdStatusExistingRepo(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	repoDir := filepath.Join(wsRoot, "myrepo")
	initGitRepo(t, repoDir)

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: "https://example.com/myrepo.git"},
	})

	out := captureStdout(t, func() {
		code := provision.CmdStatus([]string{"-f", manifestPath})
		if code != 0 {
			t.Errorf("CmdStatus returned %d", code)
		}
	})

	if !strings.Contains(out, "myrepo") {
		t.Errorf("expected myrepo in output; got:\n%s", out)
	}
}

func TestCmdStatusOccupied(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	// Create a directory that is not a git repo.
	notGit := filepath.Join(wsRoot, "notgit")
	if err := os.MkdirAll(notGit, 0o755); err != nil {
		t.Fatal(err)
	}

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "notgit", URL: "https://example.com/x.git"},
	})

	out := captureStdout(t, func() {
		provision.CmdStatus([]string{"-f", manifestPath})
	})

	if !strings.Contains(out, "✗") {
		t.Errorf("expected ✗ for non-git path; got:\n%s", out)
	}
}

// ── readLastSync tests ─────────────────────────────────────────────────────

func TestReadLastSyncNever(t *testing.T) {
	stateDir := t.TempDir()
	orig := provision.SyncStateDir
	provision.SyncStateDir = stateDir
	t.Cleanup(func() { provision.SyncStateDir = orig })

	// No state file → status shows "never".
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Sync: []manifest.Sync{
			{Name: "mysync", Local: "~/a", Remote: "host:~/a"},
		},
	}
	manifestPath := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		provision.CmdStatus([]string{"-f", manifestPath})
	})

	if !strings.Contains(out, "never") {
		t.Errorf("expected 'never' for missing state file; got:\n%s", out)
	}
}

func TestReadLastSyncPresent(t *testing.T) {
	stateDir := t.TempDir()
	orig := provision.SyncStateDir
	provision.SyncStateDir = stateDir
	t.Cleanup(func() { provision.SyncStateDir = orig })

	// Write a state file.
	ts := time.Now().UnixNano()
	stateData, _ := json.Marshal(map[string]int64{"lastSync": ts})
	if err := os.WriteFile(filepath.Join(stateDir, "mysync.json"), stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Sync: []manifest.Sync{
			{Name: "mysync", Local: "~/a", Remote: "host:~/a"},
		},
	}
	manifestPath := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		provision.CmdStatus([]string{"-f", manifestPath})
	})

	if strings.Contains(out, "never") {
		t.Errorf("expected a timestamp, not 'never'; got:\n%s", out)
	}
	if !strings.Contains(out, "mysync") {
		t.Errorf("expected 'mysync' in output; got:\n%s", out)
	}
}

// ── pull-fail soft behaviour (change 4) ──────────────────────────────────────

// makeDivergedRepo creates a source repo and a clone that have diverged so
// that git pull --ff-only will fail.
func makeDivergedRepo(t *testing.T, sourceDir, cloneDir string) {
	t.Helper()
	initGitRepo(t, sourceDir)

	if out, err := exec.Command("git", "clone", sourceDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	runIn := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	// Commit to source (origin advances).
	srcFile := filepath.Join(sourceDir, "src.txt")
	if err := os.WriteFile(srcFile, []byte("from-source"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(sourceDir, "config", "user.email", "t@t.com")
	runIn(sourceDir, "config", "user.name", "T")
	runIn(sourceDir, "add", "src.txt")
	runIn(sourceDir, "commit", "-m", "source-commit")

	// Commit to clone (local diverges from origin).
	cloneFile := filepath.Join(cloneDir, "clone.txt")
	if err := os.WriteFile(cloneFile, []byte("from-clone"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(cloneDir, "config", "user.email", "t@t.com")
	runIn(cloneDir, "config", "user.name", "T")
	runIn(cloneDir, "add", "clone.txt")
	runIn(cloneDir, "commit", "-m", "clone-commit")
}

// TestCmdUpPullFailReported verifies that when git pull --ff-only fails the
// glyph is ✗ and the action is "error", and the overall exit code is 1.
func TestCmdUpPullFailReported(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	sourceDir := filepath.Join(dir, "source")
	cloneDir := filepath.Join(wsRoot, "myrepo")
	makeDivergedRepo(t, sourceDir, cloneDir)

	manifestPath := makeTestManifest(t, dir, wsRoot, []manifest.Repo{
		{Name: "myrepo", URL: sourceDir},
	})

	var code int
	out := captureStdout(t, func() {
		code = provision.CmdUp([]string{"-f", manifestPath})
	})

	if code == 0 {
		t.Error("expected non-zero exit for diverged repo")
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("expected ✗ glyph for pull failure; got:\n%s", out)
	}
}

// TestCmdUpPullFailEnvStillWritten verifies that secrets are materialised as
// .env even when the git pull fails (change 4 behaviour).
func TestCmdUpPullFailEnvStillWritten(t *testing.T) {
	dir := t.TempDir()

	// Point the secrets key at a temp location so we don't pollute ~/.config.
	secrets.KeyPath = filepath.Join(dir, "age.key")
	t.Cleanup(func() { secrets.KeyPath = "" })

	wsRoot := filepath.Join(dir, "ws")
	sourceDir := filepath.Join(dir, "source")
	cloneDir := filepath.Join(wsRoot, "myrepo")
	makeDivergedRepo(t, sourceDir, cloneDir)

	// Build manifest with a secrets file.
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Repos:     []manifest.Repo{{Name: "myrepo", URL: sourceDir}},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
	}
	manifestPath := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatalf("Save manifest: %v", err)
	}

	// Initialise and populate the secrets store.
	captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"init", "-f", manifestPath}); code != 0 {
			t.Errorf("secrets init returned %d", code)
		}
	})
	captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"set", "-f", manifestPath, "myrepo", "API_KEY", "topsecret"}); code != 0 {
			t.Errorf("secrets set returned %d", code)
		}
	})

	var code int
	out := captureStdout(t, func() {
		code = provision.CmdUp([]string{"-f", manifestPath})
	})

	// Pull still failed → exit 1.
	if code == 0 {
		t.Error("expected non-zero exit for diverged repo")
	}

	// Output should contain the failure glyph.
	if !strings.Contains(out, "✗") {
		t.Errorf("expected ✗ in output; got:\n%s", out)
	}

	// .env should have been written despite the pull failure.
	envPath := filepath.Join(cloneDir, ".env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf(".env not written after pull failure: %v", err)
	}
	if !strings.Contains(string(data), "API_KEY=topsecret") {
		t.Errorf(".env missing API_KEY; contents:\n%s", data)
	}

	// The detail column should mention .env written.
	if !strings.Contains(out, ".env written") {
		t.Errorf("expected '.env written' in output; got:\n%s", out)
	}
}

// TestCmdUpFreshCloneWritesEnv verifies the FIRST up on a fresh machine
// materialises .env for a repo it just cloned (regression: the clone path
// used to return early, skipping secrets/direnv/mise entirely).
func TestCmdUpFreshCloneWritesEnv(t *testing.T) {
	dir := t.TempDir()

	secrets.KeyPath = filepath.Join(dir, "age.key")
	t.Cleanup(func() { secrets.KeyPath = "" })

	wsRoot := filepath.Join(dir, "ws")
	sourceDir := filepath.Join(dir, "source")
	initGitRepo(t, sourceDir)

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: wsRoot},
		Repos:     []manifest.Repo{{Name: "myrepo", URL: sourceDir}},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
	}
	manifestPath := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatalf("Save manifest: %v", err)
	}

	captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"init", "-f", manifestPath}); code != 0 {
			t.Errorf("secrets init returned %d", code)
		}
	})
	captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"set", "-f", manifestPath, "myrepo", "API_KEY", "fresh"}); code != 0 {
			t.Errorf("secrets set returned %d", code)
		}
	})

	var code int
	out := captureStdout(t, func() {
		code = provision.CmdUp([]string{"-f", manifestPath})
	})
	if code != 0 {
		t.Fatalf("CmdUp returned %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "cloned") {
		t.Errorf("expected 'cloned' in output; got:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(wsRoot, "myrepo", ".env"))
	if err != nil {
		t.Fatalf(".env not written on fresh clone: %v", err)
	}
	if !strings.Contains(string(data), "API_KEY=fresh") {
		t.Errorf(".env missing API_KEY; contents:\n%s", data)
	}
}
