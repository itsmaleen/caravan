package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"caravan/internal/manifest"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// captureRender collects the table output for a slice of results.
func captureRender(results []result) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	renderTo(w, results)
	return buf.String()
}

// withStubCmd temporarily replaces runCmd for the duration of the test.
func withStubCmd(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := runCmd
	runCmd = fn
	t.Cleanup(func() { runCmd = orig })
}

// makeGitRepo creates a directory with a .git subdirectory at dir.
func makeGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ── table rendering ───────────────────────────────────────────────────────────

func TestRenderIncludesHeader(t *testing.T) {
	results := []result{
		{"environment", "git", statusOK, "git version 2.42.0"},
		{"environment", "ssh", statusFail, "ssh not found in PATH"},
	}
	out := captureRender(results)
	for _, col := range []string{"SECTION", "CHECK", "STATUS", "DETAIL"} {
		if !strings.Contains(out, col) {
			t.Errorf("expected column header %q in output:\n%s", col, out)
		}
	}
}

func TestRenderGlyphs(t *testing.T) {
	results := []result{
		{"s", "ok", statusOK, "fine"},
		{"s", "warn", statusWarn, "meh"},
		{"s", "fail", statusFail, "bad"},
		{"s", "na", statusNA, "skip"},
	}
	out := captureRender(results)
	for _, glyph := range []string{statusOK, statusWarn, statusFail, statusNA} {
		if !strings.Contains(out, glyph) {
			t.Errorf("expected glyph %q in output:\n%s", glyph, out)
		}
	}
}

func TestRenderColumnAlignment(t *testing.T) {
	results := []result{
		{"environment", "git", statusOK, "git version 2.42.0"},
		{"environment", "a-much-longer-check-name", statusWarn, "detail here"},
	}
	lines := strings.Split(strings.TrimRight(captureRender(results), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// tabwriter replaces tab separators with spaces for alignment.
	// Each line must contain the 4 expected column values; we verify they all appear.
	for _, line := range lines[1:] { // skip header
		if !strings.Contains(line, "environment") {
			t.Errorf("expected 'environment' column in line %q", line)
		}
		if !strings.Contains(line, "git version 2.42.0") && !strings.Contains(line, "detail here") {
			t.Errorf("expected detail column value in line %q", line)
		}
	}
}

// ── exit-code / status aggregation ───────────────────────────────────────────

func TestExitCodeAllOK(t *testing.T) {
	results := []result{
		{"s", "a", statusOK, ""},
		{"s", "b", statusWarn, ""},
		{"s", "c", statusNA, ""},
	}
	code := exitCode(results)
	if code != 0 {
		t.Errorf("expected exit 0 with no ✗ results, got %d", code)
	}
}

func TestExitCodeOneFail(t *testing.T) {
	results := []result{
		{"s", "a", statusOK, ""},
		{"s", "b", statusFail, "bad"},
		{"s", "c", statusWarn, ""},
	}
	code := exitCode(results)
	if code != 1 {
		t.Errorf("expected exit 1 with a ✗ result, got %d", code)
	}
}

func TestWarnDoesNotAffectExitCode(t *testing.T) {
	results := []result{
		{"s", "a", statusWarn, "just a warning"},
	}
	code := exitCode(results)
	if code != 0 {
		t.Errorf("warning should not set exit 1, got %d", code)
	}
}

// exitCode computes the intended exit code for a result set (mirrors CmdDoctor logic).
func exitCode(results []result) int {
	for _, r := range results {
		if r.status == statusFail {
			return 1
		}
	}
	return 0
}

// ── env checks (stubbed binaries) ────────────────────────────────────────────

func TestCheckBinaryPresent(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("git version 2.42.0\n"), nil
		}
		return nil, fmt.Errorf("executable file not found in $PATH: %s", name)
	})
	r := checkBinaryInPath("environment", "git", "git", "--version")
	if r.status != statusOK {
		t.Errorf("expected ✓ for git, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "2.42.0") {
		t.Errorf("expected version in detail, got %q", r.detail)
	}
}

func TestCheckBinaryMissing(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("executable file not found in $PATH: %s", name)
	})
	r := checkBinaryInPath("environment", "rsync", "rsync", "--version")
	if r.status != statusFail {
		t.Errorf("expected ✗ for missing rsync, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "not found") {
		t.Errorf("expected 'not found' in detail, got %q", r.detail)
	}
}

func TestCheckSSHNonZeroExitWithOutput(t *testing.T) {
	// ssh -V may exit non-zero on some builds but still print a version string.
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		if name == "ssh" {
			return []byte("OpenSSH_9.0p1\n"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("executable file not found in $PATH: %s", name)
	})
	r := checkBinaryInPath("environment", "ssh", "ssh", "-V")
	if r.status != statusOK {
		t.Errorf("expected ✓ for ssh with output despite non-zero exit, got %q (detail: %s)", r.status, r.detail)
	}
}

func TestEnvChecksAllMissing(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("executable file not found in $PATH: %s", name)
	})
	results := checkEnv()
	// Should have 4 checks: git, ssh, rsync, caravan version.
	if len(results) != 4 {
		t.Fatalf("expected 4 env checks, got %d", len(results))
	}
	// First three (git, ssh, rsync) should be ✗.
	for _, r := range results[:3] {
		if r.status != statusFail {
			t.Errorf("expected ✗ for missing %s, got %q", r.name, r.status)
		}
	}
	// Caravan version is always ✓ (built-in constant).
	if results[3].status != statusOK {
		t.Errorf("caravan version check should always be ✓")
	}
}

// ── repo checks ───────────────────────────────────────────────────────────────

func TestCheckRepoMissing(t *testing.T) {
	tmp := t.TempDir()
	m := &manifest.Manifest{}
	m.Workspace.Root = tmp
	r := manifest.Repo{Name: "nonexistent", URL: "git@github.com/x/y"}
	res := checkRepo(m, r)
	if res.status != statusFail {
		t.Errorf("expected ✗ for missing repo dir, got %q", res.status)
	}
	if !strings.Contains(res.detail, "run caravan up") {
		t.Errorf("expected actionable hint in detail, got %q", res.detail)
	}
}

func TestCheckRepoExistsIsGitRepo(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "myrepo")
	makeGitRepo(t, repoDir)

	m := &manifest.Manifest{}
	m.Workspace.Root = tmp
	r := manifest.Repo{Name: "myrepo", URL: "git@github.com/x/y"}
	res := checkRepo(m, r)
	if res.status != statusOK {
		t.Errorf("expected ✓ for valid git repo, got %q (detail: %s)", res.status, res.detail)
	}
}

func TestCheckRepoOccupied(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "occupied")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Directory exists but no .git subdirectory.

	m := &manifest.Manifest{}
	m.Workspace.Root = tmp
	r := manifest.Repo{Name: "occupied", URL: "git@github.com/x/y"}
	res := checkRepo(m, r)
	if res.status != statusWarn {
		t.Errorf("expected ~ for occupied non-git dir, got %q (detail: %s)", res.status, res.detail)
	}
	if !strings.Contains(res.detail, "occupied") {
		t.Errorf("expected 'occupied' in detail, got %q", res.detail)
	}
}

// ── state file checks ─────────────────────────────────────────────────────────

func TestCheckStateFileMissing(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	r := checkStateFile("sync/test", "test")
	if r.status != statusWarn {
		t.Errorf("expected ~ for missing state file, got %q", r.status)
	}
	if !strings.Contains(r.detail, "never synced") {
		t.Errorf("expected 'never synced' in detail, got %q", r.detail)
	}
}

func TestCheckStateFileValid(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	lastSync := time.Now().Add(-10 * time.Minute).UnixNano()
	data, _ := json.Marshal(map[string]any{"lastSync": lastSync, "pairs": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, "myname.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := checkStateFile("sync/myname", "myname")
	if r.status != statusOK {
		t.Errorf("expected ✓ for valid state file, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "ago") {
		t.Errorf("expected 'ago' in detail, got %q", r.detail)
	}
}

func TestCheckStateFileCorrupt(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("not json!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := checkStateFile("sync/bad", "bad")
	if r.status != statusFail {
		t.Errorf("expected ✗ for corrupt state file, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "corrupt") {
		t.Errorf("expected 'corrupt' in detail, got %q", r.detail)
	}
}

func TestCheckStateFileZeroLastSync(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	// Valid JSON but lastSync == 0 (never synced).
	data, _ := json.Marshal(map[string]any{"lastSync": 0, "pairs": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, "zero.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := checkStateFile("sync/zero", "zero")
	if r.status != statusWarn {
		t.Errorf("expected ~ for zero lastSync, got %q (detail: %s)", r.status, r.detail)
	}
}

// ── lock checks ───────────────────────────────────────────────────────────────

func TestCheckLockFree(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	r := checkLock("sync/free", "free")
	if r.status != statusOK {
		t.Errorf("expected ✓ for free lock, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "free") {
		t.Errorf("expected 'free' in detail, got %q", r.detail)
	}
}

// ── conflicts dir checks ──────────────────────────────────────────────────────

func TestCheckConflictsNoneExist(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	r := checkConflicts("sync/test", "test")
	if r.status != statusOK {
		t.Errorf("expected ✓ for no conflicts dir, got %q", r.status)
	}
	if !strings.Contains(r.detail, "0 backups") {
		t.Errorf("expected '0 backups' in detail, got %q", r.detail)
	}
}

func TestCheckConflictsWithFiles(t *testing.T) {
	dir := t.TempDir()
	StateDirOverride = dir
	t.Cleanup(func() { StateDirOverride = "" })

	// conflicts live at <stateDir>/../conflicts/<name>
	conflDir := filepath.Join(dir, "..", "conflicts", "myentry")
	conflDir = filepath.Clean(conflDir)
	if err := os.MkdirAll(conflDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file1.bak", "file2.bak"} {
		if err := os.WriteFile(filepath.Join(conflDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := checkConflicts("sync/myentry", "myentry")
	if r.status != statusOK {
		t.Errorf("expected ✓ even with conflicts, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "2 backup") {
		t.Errorf("expected '2 backup' in detail, got %q", r.detail)
	}
}

// ── daemon plist checks ───────────────────────────────────────────────────────

func TestCheckDaemonPlistNotInstalled(t *testing.T) {
	dir := t.TempDir()
	LaunchAgentsDirOverride = dir
	t.Cleanup(func() { LaunchAgentsDirOverride = "" })

	r := checkDaemonPlist("sync/foo", "foo")
	if r.status != statusNA {
		t.Errorf("expected - for missing plist, got %q", r.status)
	}
	if !strings.Contains(r.detail, "not installed") {
		t.Errorf("expected 'not installed' in detail, got %q", r.detail)
	}
}

func TestCheckDaemonPlistInstalled(t *testing.T) {
	dir := t.TempDir()
	LaunchAgentsDirOverride = dir
	t.Cleanup(func() { LaunchAgentsDirOverride = "" })

	plistPath := filepath.Join(dir, "dev.caravan.sync.foo.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := checkDaemonPlist("sync/foo", "foo")
	if r.status != statusOK {
		t.Errorf("expected ✓ for installed plist, got %q (detail: %s)", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "installed") {
		t.Errorf("expected 'installed' in detail, got %q", r.detail)
	}
}

// ── SSH remote checks (stubbed) ───────────────────────────────────────────────

func TestCheckSSHRemoteUnreachable(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 255")
	})

	results := checkSSHRemote("sync/remote", "user@devbox", "~/projects")
	if len(results) < 3 {
		t.Fatalf("expected ≥3 results, got %d", len(results))
	}
	if results[0].status != statusFail {
		t.Errorf("expected ✗ for unreachable remote, got %q", results[0].status)
	}
	if results[1].status != statusNA {
		t.Errorf("expected - for version (unreachable), got %q (detail: %s)", results[1].status, results[1].detail)
	}
	if results[2].status != statusNA {
		t.Errorf("expected - for dir (unreachable), got %q (detail: %s)", results[2].status, results[2].detail)
	}
}

func TestCheckSSHRemoteReachableVersionMatch(t *testing.T) {
	localVer := localCaravanVersion()
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		if name != "ssh" {
			return nil, fmt.Errorf("unexpected binary: %s", name)
		}
		remoteCmd := args[len(args)-1]
		switch {
		case remoteCmd == "true":
			return []byte{}, nil
		case strings.Contains(remoteCmd, "caravan version"):
			return []byte("caravan " + localVer + "\n"), nil
		default:
			return []byte{}, nil
		}
	})

	results := checkSSHRemote("sync/remote", "user@devbox", "~/projects")
	if len(results) < 3 {
		t.Fatalf("expected ≥3 results, got %d", len(results))
	}
	if results[0].status != statusOK {
		t.Errorf("expected ✓ for reachable, got %q (detail: %s)", results[0].status, results[0].detail)
	}
	if results[1].status != statusOK {
		t.Errorf("expected ✓ for version match, got %q (detail: %s)", results[1].status, results[1].detail)
	}
}

func TestCheckSSHRemoteVersionMismatch(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		if name != "ssh" {
			return nil, fmt.Errorf("unexpected binary: %s", name)
		}
		remoteCmd := args[len(args)-1]
		switch {
		case remoteCmd == "true":
			return []byte{}, nil
		case strings.Contains(remoteCmd, "caravan version"):
			return []byte("caravan 0.0.1\n"), nil
		default:
			return []byte{}, nil
		}
	})

	results := checkSSHRemote("sync/remote", "user@devbox", "~/projects")
	if len(results) < 2 {
		t.Fatalf("expected ≥2 results, got %d", len(results))
	}
	if results[1].status != statusWarn {
		t.Errorf("expected ~ for version mismatch, got %q (detail: %s)", results[1].status, results[1].detail)
	}
	if !strings.Contains(results[1].detail, "will self-update") {
		t.Errorf("expected 'will self-update' in detail, got %q", results[1].detail)
	}
}

func TestCheckSSHRemoteMissingBinary(t *testing.T) {
	withStubCmd(t, func(name string, args ...string) ([]byte, error) {
		if name != "ssh" {
			return nil, fmt.Errorf("unexpected binary: %s", name)
		}
		remoteCmd := args[len(args)-1]
		switch {
		case remoteCmd == "true":
			return []byte{}, nil
		case strings.Contains(remoteCmd, "caravan version"):
			// Simulate missing remote binary: exit 127, no output.
			return []byte{}, fmt.Errorf("exit status 127")
		default:
			return []byte{}, nil
		}
	})

	results := checkSSHRemote("sync/remote", "user@devbox", "~/projects")
	if len(results) < 2 {
		t.Fatalf("expected ≥2 results, got %d", len(results))
	}
	// Missing binary → warning (bootstrap is automatic), not ✗.
	if results[1].status != statusWarn {
		t.Errorf("expected ~ for missing remote binary, got %q (detail: %s)", results[1].status, results[1].detail)
	}
	if !strings.Contains(results[1].detail, "bootstraps on next sync") {
		t.Errorf("expected 'bootstraps on next sync' in detail, got %q", results[1].detail)
	}
}

// ── parseRemoteTransport ──────────────────────────────────────────────────────

func TestParseRemoteTransportLocal(t *testing.T) {
	transport, host, root, err := parseRemoteTransport("local:/tmp/foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport != "local" {
		t.Errorf("expected 'local', got %q", transport)
	}
	if host != "" {
		t.Errorf("expected empty host, got %q", host)
	}
	if root != "/tmp/foo" {
		t.Errorf("expected '/tmp/foo', got %q", root)
	}
}

func TestParseRemoteTransportSSH(t *testing.T) {
	transport, host, root, err := parseRemoteTransport("user@devbox:~/work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport != "ssh" {
		t.Errorf("expected 'ssh', got %q", transport)
	}
	if host != "user@devbox" {
		t.Errorf("expected 'user@devbox', got %q", host)
	}
	if root != "~/work" {
		t.Errorf("expected '~/work', got %q", root)
	}
}

func TestParseRemoteTransportInvalid(t *testing.T) {
	_, _, _, err := parseRemoteTransport("nodomain")
	if err == nil {
		t.Error("expected error for invalid spec, got nil")
	}
}

// ── sample rendered output ────────────────────────────────────────────────────

// TestSampleRenderedOutput produces a human-readable snapshot (also validates no panics).
func TestSampleRenderedOutput(t *testing.T) {
	results := []result{
		{"environment", "git", statusOK, "git version 2.42.0"},
		{"environment", "ssh", statusOK, "OpenSSH_9.0p1"},
		{"environment", "rsync", statusOK, "rsync  version 3.2.7  protocol version 31"},
		{"environment", "caravan version", statusOK, "0.4.0"},
		{"environment", "manifest", statusOK, "/home/user/.config/caravan/caravan.toml"},
		{"repos", "api", statusOK, "/home/user/projects/api"},
		{"repos", "frontend", statusFail, "directory missing — run caravan up"},
		{"repos", "infra", statusWarn, "/home/user/projects/infra exists but is not a git repo (occupied)"},
		{"sync/work", "local dir", statusOK, "/home/user/work"},
		{"sync/work", "remote reachable", statusOK, "user@devbox"},
		{"sync/work", "remote caravan version", statusOK, "0.4.0"},
		{"sync/work", "remote dir", statusOK, "~/work"},
		{"sync/work", "state file", statusOK, "last sync 5m0s ago"},
		{"sync/work", "lock", statusOK, "free"},
		{"sync/work", "conflicts", statusOK, "0 backups"},
		{"sync/work", "daemon plist", statusNA, "not installed"},
		{"secrets", "machine key", statusOK, "/home/user/.config/caravan/age.key"},
		{"secrets", "store file", statusOK, "/home/user/.config/caravan/secrets.enc.json (1 recipient(s), 2 repo(s))"},
	}
	out := captureRender(results)
	t.Logf("sample rendered output:\n%s", out)

	for _, want := range []string{"environment", "sync/work", "repos", "secrets"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in rendered output:\n%s", want, out)
		}
	}
}

// localCaravanVersion extracts the caravan version from the checkEnv results.
// This avoids importing buildinfo directly in the test file.
func localCaravanVersion() string {
	for _, r := range checkEnv() {
		if r.name == "caravan version" {
			return r.detail
		}
	}
	return ""
}
