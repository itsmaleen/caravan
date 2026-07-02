package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"caravan/internal/manifest"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos: []manifest.Repo{
			{Name: "hello", URL: "https://github.com/octocat/Hello-World.git", Branch: "master"},
			{Name: "world", URL: "https://github.com/example/world.git", Sparse: true},
		},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
		Toolchain: manifest.Toolchain{Mise: true},
	}

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Version != m.Version {
		t.Errorf("Version: got %d, want %d", got.Version, m.Version)
	}
	if got.Workspace.Root != m.Workspace.Root {
		t.Errorf("Workspace.Root: got %q, want %q", got.Workspace.Root, m.Workspace.Root)
	}
	if len(got.Repos) != len(m.Repos) {
		t.Fatalf("Repos len: got %d, want %d", len(got.Repos), len(m.Repos))
	}
	for i, r := range m.Repos {
		g := got.Repos[i]
		if g.Name != r.Name || g.URL != r.URL || g.Branch != r.Branch || g.Sparse != r.Sparse {
			t.Errorf("Repos[%d]: got %+v, want %+v", i, g, r)
		}
	}
	if got.Secrets.File != m.Secrets.File {
		t.Errorf("Secrets.File: got %q, want %q", got.Secrets.File, m.Secrets.File)
	}
	if got.Toolchain.Mise != m.Toolchain.Mise {
		t.Errorf("Toolchain.Mise: got %v, want %v", got.Toolchain.Mise, m.Toolchain.Mise)
	}
	// Dir should be set to the directory of the loaded file.
	if got.Dir != dir {
		t.Errorf("Dir: got %q, want %q", got.Dir, dir)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir:", err)
	}
	tests := []struct {
		in   string
		want string
	}{
		{"~/code", filepath.Join(home, "code")},
		{"~/", home},
		{"~", home},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~/a/b/c", filepath.Join(home, "a/b/c")},
	}
	for _, tt := range tests {
		got := manifest.ExpandPath(tt.in)
		if got != tt.want {
			t.Errorf("ExpandPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateDuplicateRepo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos: []manifest.Repo{
			{Name: "dup", URL: "https://example.com/a.git"},
			{Name: "dup", URL: "https://example.com/b.git"},
		},
	}

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := manifest.Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate repo name, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate repo") {
		t.Errorf("error should mention 'duplicate repo': %v", err)
	}
}

func TestValidateMissingRepoFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos: []manifest.Repo{
			{Name: "no-url"},
		},
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := manifest.Load(path)
	if err == nil {
		t.Fatal("expected error for missing URL, got nil")
	}
}

func TestValidateDuplicateSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Sync: []manifest.Sync{
			{Name: "dup", Local: "~/a", Remote: "h:~/a"},
			{Name: "dup", Local: "~/b", Remote: "h:~/b"},
		},
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := manifest.Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate sync name, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate sync") {
		t.Errorf("error should mention 'duplicate sync': %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	defaultPath := filepath.Join(home, ".config/caravan/caravan.toml")

	// No flag, no env → default.
	t.Setenv("CARAVAN_MANIFEST", "")
	got := manifest.ResolvePath("")
	if got != defaultPath {
		t.Errorf("ResolvePath(\"\") = %q, want %q", got, defaultPath)
	}

	// Env var wins over default.
	t.Setenv("CARAVAN_MANIFEST", "/tmp/env-manifest.toml")
	got = manifest.ResolvePath("")
	if got != "/tmp/env-manifest.toml" {
		t.Errorf("ResolvePath with env = %q, want /tmp/env-manifest.toml", got)
	}

	// Flag wins over env.
	got = manifest.ResolvePath("/tmp/flag-manifest.toml")
	if got != "/tmp/flag-manifest.toml" {
		t.Errorf("ResolvePath(flag) = %q, want /tmp/flag-manifest.toml", got)
	}
}

func TestSecretsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos:     []manifest.Repo{{Name: "x", URL: "https://example.com/x.git"}},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := filepath.Join(dir, "secrets.enc.json")
	if got := loaded.SecretsPath(); got != want {
		t.Errorf("SecretsPath() = %q, want %q", got, want)
	}
}

func TestRepoDirDefault(t *testing.T) {
	m := &manifest.Manifest{
		Workspace: manifest.Workspace{Root: "/workspace"},
	}
	r := manifest.Repo{Name: "hello", URL: "u"}
	got := m.RepoDir(r)
	if got != "/workspace/hello" {
		t.Errorf("RepoDir = %q, want /workspace/hello", got)
	}
}

func TestRepoDirCustomPath(t *testing.T) {
	m := &manifest.Manifest{
		Workspace: manifest.Workspace{Root: "/workspace"},
	}
	r := manifest.Repo{Name: "hello", URL: "u", Path: "sub/hello"}
	got := m.RepoDir(r)
	if got != "/workspace/sub/hello" {
		t.Errorf("RepoDir = %q, want /workspace/sub/hello", got)
	}
}
