package syncengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"caravan/internal/manifest"
)

// --- helpers ---

// seedFile writes content to root/rel, creating parent dirs as needed.
// It also sets mtime precisely so planner comparisons are deterministic.
func seedFile(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("seedFile mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("seedFile write: %v", err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatalf("seedFile chtimes: %v", err)
	}
}

// seedDir creates an empty directory at root/rel.
func seedDir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("seedDir: %v", err)
	}
}

// assertFile checks that root/rel exists and has the expected content.
func assertFile(t *testing.T, root, rel, want string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Errorf("assertFile %s: %v", rel, err)
		return
	}
	if string(got) != want {
		t.Errorf("assertFile %s: got %q want %q", rel, string(got), want)
	}
}

// assertAbsent checks that root/rel does NOT exist.
func assertAbsent(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(full); err == nil {
		t.Errorf("assertAbsent %s: file exists unexpectedly", rel)
	}
}

// assertDir checks that root/rel is a directory.
func assertDir(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		t.Errorf("assertDir %s: %v", rel, err)
		return
	}
	if !info.IsDir() {
		t.Errorf("assertDir %s: not a directory", rel)
	}
}

// syncEntry builds a manifest.Sync entry and runs one sync pass.
func doSync(t *testing.T, name, localRoot, remoteSpec string, excludes []string, dryRun bool) error {
	t.Helper()
	s := manifest.Sync{
		Name:    name,
		Local:   localRoot,
		Remote:  remoteSpec,
		Exclude: excludes,
	}
	return runSyncEntry(s, dryRun, false)
}

func t1() time.Time { return time.Unix(1_000_000, 0) }
func t2() time.Time { return time.Unix(2_000_000, 0) }
func t3() time.Time { return time.Unix(3_000_000, 0) }

// setupStateDir redirects state files into the test temp dir.
func setupStateDir(t *testing.T) {
	t.Helper()
	orig := StateDir
	StateDir = t.TempDir()
	t.Cleanup(func() { StateDir = orig })
}

// --- integration tests using local: transport ---

func TestIntegration_SeedAndSync(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "hello.txt", "hello", t1())
	seedFile(t, sideA, "sub/world.txt", "world", t1())

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertFile(t, sideB, "hello.txt", "hello")
	assertFile(t, sideB, "sub/world.txt", "world")
}

func TestIntegration_MutateRemote_Sync(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "original", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Mutate sideB (remote).
	seedFile(t, sideB, "f.txt", "modified by remote", t2())
	// Add a new file on remote.
	seedFile(t, sideB, "new-remote.txt", "remote-new", t2())

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// sideA should pick up remote changes.
	assertFile(t, sideA, "f.txt", "modified by remote")
	assertFile(t, sideA, "new-remote.txt", "remote-new")
}

func TestIntegration_MutateLocal_Sync(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "original", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Mutate sideA (local).
	seedFile(t, sideA, "f.txt", "modified by local", t2())
	seedFile(t, sideA, "new-local.txt", "local-new", t2())

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertFile(t, sideB, "f.txt", "modified by local")
	assertFile(t, sideB, "new-local.txt", "local-new")
}

func TestIntegration_Conflict_NewerWins(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "original", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Both sides modify f.txt; remote (sideB) is newer.
	seedFile(t, sideA, "f.txt", "local edit", t2())
	seedFile(t, sideB, "f.txt", "remote edit", t3()) // newer

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertFile(t, sideA, "f.txt", "remote edit")
	assertFile(t, sideB, "f.txt", "remote edit")
}

func TestIntegration_Conflict_TieLocalWins(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "original", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Both sides modify f.txt with the same mtime → local (sideA) wins.
	seedFile(t, sideA, "f.txt", "local edit", t2())
	seedFile(t, sideB, "f.txt", "remote edit", t2()) // tie mtime

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertFile(t, sideA, "f.txt", "local edit")
	assertFile(t, sideB, "f.txt", "local edit")
}

func TestIntegration_DeleteLocal_PropagatesRemote(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "content", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Delete from local.
	os.Remove(filepath.Join(sideA, "f.txt"))

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertAbsent(t, sideA, "f.txt")
	assertAbsent(t, sideB, "f.txt")
}

func TestIntegration_DeleteRemote_PropagatesLocal(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "content", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Delete from remote.
	os.Remove(filepath.Join(sideB, "f.txt"))

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertAbsent(t, sideA, "f.txt")
	assertAbsent(t, sideB, "f.txt")
}

func TestIntegration_Excludes(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "keep.txt", "keep", t1())
	seedFile(t, sideA, "node_modules/big.js", "big", t1())
	seedFile(t, sideA, ".DS_Store", "noise", t1())
	seedDir(t, sideA, "dist")
	seedFile(t, sideA, "dist/bundle.js", "bundle", t1())

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertFile(t, sideB, "keep.txt", "keep")
	assertAbsent(t, sideB, "node_modules/big.js")
	assertAbsent(t, sideB, ".DS_Store")
	assertAbsent(t, sideB, "dist/bundle.js")
}

func TestIntegration_EmptyNestedDirs(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Create nested empty dirs on sideA.
	seedDir(t, sideA, "a/b/c")

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertDir(t, sideB, "a")
	assertDir(t, sideB, "a/b")
	assertDir(t, sideB, "a/b/c")
}

func TestIntegration_DryRun(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "content", t1())

	if err := doSync(t, "test", sideA, "local:"+sideB, nil, true /* dryRun */); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	// Dry run should NOT copy anything.
	assertAbsent(t, sideB, "f.txt")
}

func TestIntegration_ThreeWayRoundTrip(t *testing.T) {
	// Full round trip:
	// 1. Seed A, sync A→B
	// 2. Add file on B, sync again → A gets it
	// 3. Modify file on A, sync → B gets it
	// 4. Delete file from B, sync → A loses it too
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "shared.txt", "v1", t1())
	doSync(t, "test", sideA, "local:"+sideB, nil, false)
	assertFile(t, sideB, "shared.txt", "v1")

	seedFile(t, sideB, "from-b.txt", "from remote", t2())
	doSync(t, "test", sideA, "local:"+sideB, nil, false)
	assertFile(t, sideA, "from-b.txt", "from remote")

	seedFile(t, sideA, "shared.txt", "v2", t3())
	doSync(t, "test", sideA, "local:"+sideB, nil, false)
	assertFile(t, sideB, "shared.txt", "v2")

	os.Remove(filepath.Join(sideB, "from-b.txt"))
	doSync(t, "test", sideA, "local:"+sideB, nil, false)
	assertAbsent(t, sideA, "from-b.txt")
	assertAbsent(t, sideB, "from-b.txt")
}

func TestIntegration_CustomExcludes(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "main.go", "package main", t1())
	seedFile(t, sideA, "main_test.go", "package main", t1())

	// Exclude test files.
	if err := doSync(t, "test", sideA, "local:"+sideB, []string{"*_test.go"}, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertFile(t, sideB, "main.go", "package main")
	assertAbsent(t, sideB, "main_test.go")
}

// TestScanDir_Excludes verifies the scanner respects excludes.
func TestScanDir_Excludes(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "keep.txt", "k", t1())
	seedFile(t, root, "node_modules/pkg.js", "js", t1())
	seedFile(t, root, "src/main.go", "go", t1())
	seedFile(t, root, "build/out.o", "obj", t1())

	entries, _, err := ScanDir(root, []string{"node_modules", "build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := entries["keep.txt"]; !ok {
		t.Error("keep.txt should be included")
	}
	if _, ok := entries["src/main.go"]; !ok {
		t.Error("src/main.go should be included")
	}
	for p := range entries {
		if strings.HasPrefix(p, "node_modules") || strings.HasPrefix(p, "build") {
			t.Errorf("excluded path in scan: %s", p)
		}
	}
}

// TestScanDir_SkipSymlinks verifies symlinks are skipped.
func TestScanDir_SkipSymlinks(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "real.txt", "real", t1())

	// Create a symlink.
	linkPath := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(root, "real.txt"), linkPath); err != nil {
		t.Skip("symlink creation failed (may need elevated perms):", err)
	}

	_, symlinks, err := ScanDir(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if symlinks != 1 {
		t.Errorf("expected 1 symlink skipped, got %d", symlinks)
	}
}

// TestParseRemote verifies spec parsing.
func TestParseRemote(t *testing.T) {
	r, err := ParseRemote("local:/tmp/foo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != transportLocal {
		t.Error("expected local transport")
	}
	if r.Root != "/tmp/foo" {
		t.Errorf("root: got %q want /tmp/foo", r.Root)
	}

	r, err = ParseRemote("user@host.example.com:~/sync")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != transportSSH {
		t.Error("expected SSH transport")
	}
	if r.Host != "user@host.example.com" {
		t.Errorf("host: got %q", r.Host)
	}
	if r.Root != "~/sync" {
		t.Errorf("root: got %q want ~/sync", r.Root)
	}
}

// TestLoadSaveState verifies state round-trip.
func TestLoadSaveState(t *testing.T) {
	setupStateDir(t)
	s := &State{
		Pairs: map[string]BaseEntry{
			"a.txt": {LSize: 10, LMtime: 1000, RSize: 10, RMtime: 1000},
		},
		LastSync: 999,
	}
	if err := SaveState("mytest", s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState("mytest")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSync != 999 {
		t.Errorf("LastSync: got %d want 999", got.LastSync)
	}
	if e, ok := got.Pairs["a.txt"]; !ok || e.LSize != 10 {
		t.Errorf("pairs: got %v", got.Pairs)
	}
}
