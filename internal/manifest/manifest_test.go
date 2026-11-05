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

// ── Split-remote helpers ───────────────────────────────────────────────────

func TestIsPrivateBranch(t *testing.T) {
	r := manifest.Repo{
		Name:            "app",
		URL:             "https://example.com/app.git",
		PrivateRemote:   "private",
		PrivateURL:      "git@git.internal:me/app.git",
		PrivateBranches: []string{"private/*", "embargo/*", "secret/*"},
	}
	tests := []struct {
		branch string
		want   bool
	}{
		{"private/feature", true},
		{"embargo/cve-2026-0001", true},
		{"secret/refactor", true},
		{"main", false},
		{"feature/public", false},
		{"release/v1", false},
	}
	for _, tt := range tests {
		got := r.IsPrivateBranch(tt.branch)
		if got != tt.want {
			t.Errorf("IsPrivateBranch(%q) = %v, want %v", tt.branch, got, tt.want)
		}
	}
}

func TestIsPrivateBranchDefaults(t *testing.T) {
	// When PrivateBranches is empty but routing is enabled, defaults kick in.
	r := manifest.Repo{
		Name:          "app",
		URL:           "https://example.com/app.git",
		PrivateRemote: "private",
		PrivateURL:    "git@git.internal:me/app.git",
	}
	if !r.IsPrivateBranch("private/x") {
		t.Error("expected private/x to match default globs")
	}
	if !r.IsPrivateBranch("embargo/x") {
		t.Error("expected embargo/x to match default globs")
	}
	if r.IsPrivateBranch("main") {
		t.Error("expected main not to match default globs")
	}
}

func TestRoutingEnabled(t *testing.T) {
	tests := []struct {
		name    string
		repo    manifest.Repo
		enabled bool
	}{
		{"both set", manifest.Repo{Name: "r", URL: "u", PrivateRemote: "private", PrivateURL: "url"}, true},
		{"neither set", manifest.Repo{Name: "r", URL: "u"}, false},
		{"only remote", manifest.Repo{Name: "r", URL: "u", PrivateRemote: "private"}, false},
		{"only url", manifest.Repo{Name: "r", URL: "u", PrivateURL: "url"}, false},
	}
	for _, tt := range tests {
		got := tt.repo.RoutingEnabled()
		if got != tt.enabled {
			t.Errorf("%s: RoutingEnabled() = %v, want %v", tt.name, got, tt.enabled)
		}
	}
}

func TestPublicRemoteOr(t *testing.T) {
	r := manifest.Repo{Name: "r", URL: "u"}
	if got := r.PublicRemoteOr(); got != "origin" {
		t.Errorf("PublicRemoteOr() with empty = %q, want 'origin'", got)
	}
	r.PublicRemote = "upstream"
	if got := r.PublicRemoteOr(); got != "upstream" {
		t.Errorf("PublicRemoteOr() with set = %q, want 'upstream'", got)
	}
}

func TestValidatePrivateRemotePairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	// Only remote set, no URL → error.
	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos: []manifest.Repo{
			{Name: "app", URL: "https://example.com/app.git", PrivateRemote: "private"},
		},
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := manifest.Load(path); err == nil {
		t.Fatal("expected error when private_remote set without private_url")
	}

	// Both set → OK.
	m.Repos[0].PrivateURL = "git@git.internal:me/app.git"
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := manifest.Load(path); err != nil {
		t.Fatalf("expected no error when both set: %v", err)
	}
}

func TestValidatePrivateGlobMatchesMain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Repos: []manifest.Repo{
			{
				Name:            "app",
				URL:             "https://example.com/app.git",
				PrivateRemote:   "private",
				PrivateURL:      "git@git.internal:me/app.git",
				PrivateBranches: []string{"*"}, // matches "main"!
			},
		},
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := manifest.Load(path); err == nil {
		t.Fatal("expected error when private glob matches 'main'")
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

// ── service block ─────────────────────────────────────────────────────────────

func serviceManifest(s manifest.Service) *manifest.Manifest {
	return &manifest.Manifest{Version: 1, Workspace: manifest.Workspace{Root: "~/code"}, Services: []manifest.Service{s}}
}

func TestService_VisibilityDefaultsToPrivate(t *testing.T) {
	// An omitted field must never widen exposure.
	if got := (manifest.Service{Name: "a", Command: "x"}).Vis(); got != manifest.VisPrivate {
		t.Errorf("default visibility = %q, want private", got)
	}
}

func TestService_ValidateRejectsUnknownVisibility(t *testing.T) {
	err := serviceManifest(manifest.Service{Name: "a", Command: "x", Port: "80", Visibility: "world"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "visibility") {
		t.Errorf("want a visibility error, got %v", err)
	}
}

func TestService_ValidateRequiresPortWhenPublished(t *testing.T) {
	err := serviceManifest(manifest.Service{Name: "a", Command: "x", Visibility: manifest.VisTailnet}).Validate()
	if err == nil || !strings.Contains(err.Error(), "needs a port") {
		t.Errorf("want a port error, got %v", err)
	}
	// private without a port is fine
	if err := serviceManifest(manifest.Service{Name: "a", Command: "x"}).Validate(); err != nil {
		t.Errorf("private service without a port should validate, got %v", err)
	}
}

func TestService_ValidateRejectsDuplicatePorts(t *testing.T) {
	m := &manifest.Manifest{Services: []manifest.Service{
		{Name: "a", Command: "x", Port: "8080"},
		{Name: "b", Command: "x", Port: "8080"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("want a port-collision error, got %v", err)
	}
}

func TestService_ValidateRejectsDuplicateNames(t *testing.T) {
	m := &manifest.Manifest{Services: []manifest.Service{
		{Name: "a", Command: "x"},
		{Name: "a", Command: "y"},
	}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate service") {
		t.Errorf("want a duplicate-name error, got %v", err)
	}
}

func TestService_ValidateRejectsBadRouteAndPort(t *testing.T) {
	if err := serviceManifest(manifest.Service{Name: "a", Command: "x", Port: "80", Route: "wrinkles"}).Validate(); err == nil {
		t.Error("route without a leading slash should fail")
	}
	if err := serviceManifest(manifest.Service{Name: "a", Command: "x", Port: "99999"}).Validate(); err == nil {
		t.Error("out-of-range port should fail")
	}
	if err := serviceManifest(manifest.Service{Name: "a", Command: "x", Port: "eighty"}).Validate(); err == nil {
		t.Error("non-numeric port should fail")
	}
}

func TestService_ValidateRequiresNameAndCommand(t *testing.T) {
	if err := serviceManifest(manifest.Service{Command: "x"}).Validate(); err == nil {
		t.Error("missing name should fail")
	}
	if err := serviceManifest(manifest.Service{Name: "a"}).Validate(); err == nil {
		t.Error("missing command should fail")
	}
}

func TestParseHealth(t *testing.T) {
	cases := []struct {
		spec   string
		method string
		path   string
		code   int
		bad    bool
	}{
		{spec: "GET / 200", method: "GET", path: "/", code: 200},
		{spec: "/healthz", method: "GET", path: "/healthz", code: 200},
		{spec: "/healthz 204", method: "GET", path: "/healthz", code: 204},
		{spec: "head /x 200", method: "HEAD", path: "/x", code: 200},
		{spec: "GET / abc", bad: true},
		{spec: "a b c d", bad: true},
	}
	for _, c := range cases {
		method, path, code, err := manifest.ParseHealth(c.spec)
		if c.bad {
			if err == nil {
				t.Errorf("manifest.ParseHealth(%q) should fail", c.spec)
			}
			continue
		}
		if err != nil || method != c.method || path != c.path || code != c.code {
			t.Errorf("manifest.ParseHealth(%q) = (%q,%q,%d,%v)", c.spec, method, path, code, err)
		}
	}
}

func TestService_RoundTripsThroughTOML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "caravan.toml")
	in := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: "~/code"},
		Services: []manifest.Service{{
			Name: "wrinkles-hub", Workspace: "wrinkles",
			Command: "python3 -m http.server {{port}} --directory ./output",
			Port:    "18790", Route: "/wrinkles", Visibility: manifest.VisTailnet, Health: "GET / 200",
		}},
	}
	if err := manifest.Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := manifest.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 1 {
		t.Fatalf("want 1 service, got %d", len(out.Services))
	}
	got := out.Services[0]
	if got.Name != "wrinkles-hub" || got.Port != "18790" || got.Vis() != manifest.VisTailnet || got.RoutePath() != "/wrinkles" {
		t.Errorf("round-trip lost data: %+v", got)
	}
}
