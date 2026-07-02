package manifest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"caravan/internal/manifest"
)

// makeGitRepo creates a git repo at dir with an optional remote origin URL.
func makeGitRepo(t *testing.T, dir, remoteURL string) {
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
	if remoteURL != "" {
		run("remote", "add", "origin", remoteURL)
	}
}

func TestDiscoverRepos(t *testing.T) {
	root := t.TempDir()

	// Create two repos with remotes.
	makeGitRepo(t, filepath.Join(root, "repo1"), "https://github.com/test/repo1.git")
	makeGitRepo(t, filepath.Join(root, "repo2"), "https://github.com/test/repo2.git")

	// Create a non-repo dir (should be ignored).
	if err := os.MkdirAll(filepath.Join(root, "notarepo"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hidden dir should be skipped.
	makeGitRepo(t, filepath.Join(root, ".hidden-repo"), "https://github.com/test/hidden.git")

	// node_modules should be skipped.
	makeGitRepo(t, filepath.Join(root, "node_modules", "pkg"), "")

	repos, err := manifest.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}

	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2: %+v", len(repos), repos)
	}

	found := map[string]bool{}
	for _, r := range repos {
		found[r.Name] = true
		if r.URL == "" {
			t.Errorf("repo %q has empty URL", r.Name)
		}
	}
	if !found["repo1"] || !found["repo2"] {
		t.Errorf("expected repo1 and repo2, got %v", found)
	}
}

func TestDiscoverReposNoRecurseIntoRepo(t *testing.T) {
	root := t.TempDir()

	// Create outer repo.
	outer := filepath.Join(root, "outer")
	makeGitRepo(t, outer, "https://github.com/test/outer.git")

	// Create nested repo inside outer — should NOT be discovered.
	makeGitRepo(t, filepath.Join(outer, "inner"), "https://github.com/test/inner.git")

	repos, err := manifest.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}

	if len(repos) != 1 || repos[0].Name != "outer" {
		t.Errorf("expected only outer, got %+v", repos)
	}
}

func TestDiscoverReposEmptyRoot(t *testing.T) {
	root := t.TempDir()
	repos, err := manifest.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("expected 0 repos, got %d", len(repos))
	}
}

func TestDiscoverReposNonExistentRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nonexistent")
	repos, err := manifest.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos for missing root, got %v", repos)
	}
}

func TestCmdInit(t *testing.T) {
	root := t.TempDir()
	makeGitRepo(t, filepath.Join(root, "alpha"), "https://github.com/test/alpha.git")
	makeGitRepo(t, filepath.Join(root, "beta"), "https://github.com/test/beta.git")

	manifestPath := filepath.Join(t.TempDir(), "caravan.toml")

	code := manifest.CmdInit([]string{
		"--root", root,
		"-f", manifestPath,
	})
	if code != 0 {
		t.Fatalf("CmdInit returned %d", code)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Repos) != 2 {
		t.Errorf("expected 2 repos, got %d: %+v", len(m.Repos), m.Repos)
	}
	if m.Workspace.Root != root {
		t.Errorf("Workspace.Root = %q, want %q", m.Workspace.Root, root)
	}
}

func TestCmdInitForce(t *testing.T) {
	root := t.TempDir()
	makeGitRepo(t, filepath.Join(root, "myrepo"), "https://github.com/test/myrepo.git")

	manifestPath := filepath.Join(t.TempDir(), "caravan.toml")

	// First init.
	if code := manifest.CmdInit([]string{"--root", root, "-f", manifestPath}); code != 0 {
		t.Fatalf("first CmdInit returned %d", code)
	}

	// Second init without --force should fail.
	if code := manifest.CmdInit([]string{"--root", root, "-f", manifestPath}); code == 0 {
		t.Fatal("expected non-zero exit without --force on existing manifest")
	}

	// With --force should succeed.
	if code := manifest.CmdInit([]string{"--root", root, "-f", manifestPath, "--force"}); code != 0 {
		t.Fatalf("CmdInit --force returned %d", code)
	}
}

func TestCmdInitRelativePaths(t *testing.T) {
	root := t.TempDir()
	makeGitRepo(t, filepath.Join(root, "sub", "myrepo"), "https://github.com/test/myrepo.git")

	manifestPath := filepath.Join(t.TempDir(), "caravan.toml")
	if code := manifest.CmdInit([]string{"--root", root, "-f", manifestPath}); code != 0 {
		t.Fatalf("CmdInit returned %d", code)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(m.Repos))
	}
	// Path should be relative to root.
	r := m.Repos[0]
	if strings.HasPrefix(r.Path, "/") {
		t.Errorf("repo path should be relative, got %q", r.Path)
	}
	wantPath := filepath.Join("sub", "myrepo")
	if r.Path != wantPath {
		t.Errorf("repo path = %q, want %q", r.Path, wantPath)
	}
}
