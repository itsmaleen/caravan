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

// TestDeltaThreshold verifies the DeltaThreshold() helper encodes the three
// value cases: 0 → default 8 MiB, -1 → disabled (MaxInt64), positive → as-is.
func TestDeltaThreshold(t *testing.T) {
	const eightMiB = int64(8 * 1024 * 1024)
	const maxInt64 = int64(^uint64(0) >> 1)

	tests := []struct {
		name          string
		deltaMinBytes int64
		want          int64
	}{
		{"default (0)", 0, eightMiB},
		{"disabled (-1)", -1, maxInt64},
		{"custom positive", 1024 * 1024, 1024 * 1024},
		{"custom large", 64 * 1024 * 1024, 64 * 1024 * 1024},
	}
	for _, tt := range tests {
		s := manifest.Sync{DeltaMinBytes: tt.deltaMinBytes}
		got := s.DeltaThreshold()
		if got != tt.want {
			t.Errorf("%s: DeltaThreshold() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestDeltaMinBytesRoundTrip verifies that DeltaMinBytes survives Save→Load
// and that omitempty keeps it out of the TOML when zero.
func TestDeltaMinBytesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Sync: []manifest.Sync{
			{Name: "big", Local: "~/a", Remote: "h:~/a", DeltaMinBytes: 16 * 1024 * 1024},
			{Name: "disabled", Local: "~/b", Remote: "h:~/b", DeltaMinBytes: -1},
			{Name: "default", Local: "~/c", Remote: "h:~/c"},
		},
	}

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got.Sync) != 3 {
		t.Fatalf("Sync len: got %d want 3", len(got.Sync))
	}
	if got.Sync[0].DeltaMinBytes != 16*1024*1024 {
		t.Errorf("Sync[0].DeltaMinBytes: got %d want %d", got.Sync[0].DeltaMinBytes, 16*1024*1024)
	}
	if got.Sync[1].DeltaMinBytes != -1 {
		t.Errorf("Sync[1].DeltaMinBytes: got %d want -1", got.Sync[1].DeltaMinBytes)
	}
	if got.Sync[2].DeltaMinBytes != 0 {
		t.Errorf("Sync[2].DeltaMinBytes: got %d want 0", got.Sync[2].DeltaMinBytes)
	}

	// omitempty: "default" entry must not contain delta_min_bytes in TOML.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "delta_min_bytes = 16777216") {
		t.Errorf("expected 'delta_min_bytes = 16777216' in TOML:\n%s", content)
	}
}

// TestSyncChecksumRoundTrip verifies that the Checksum field on [[sync]] entries
// survives a Save→Load round-trip and that omitempty keeps it out of the TOML
// when false.
func TestSyncChecksumRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Sync: []manifest.Sync{
			{Name: "with-cs", Local: "~/a", Remote: "h:~/a", Checksum: true},
			{Name: "without-cs", Local: "~/b", Remote: "h:~/b", Checksum: false},
		},
	}

	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got.Sync) != 2 {
		t.Fatalf("Sync len: got %d want 2", len(got.Sync))
	}
	if !got.Sync[0].Checksum {
		t.Errorf("Sync[0].Checksum: got false, want true")
	}
	if got.Sync[1].Checksum {
		t.Errorf("Sync[1].Checksum: got true, want false")
	}

	// Verify omitempty: the TOML file must not contain "checksum" for the false entry.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "checksum = true") {
		t.Errorf("expected 'checksum = true' in TOML output:\n%s", content)
	}
}
