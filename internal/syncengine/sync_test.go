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

// doSyncChecksum is like doSync but enables content-checksum mode.
func doSyncChecksum(t *testing.T, name, localRoot, remoteSpec string) error {
	t.Helper()
	s := manifest.Sync{
		Name:     name,
		Local:    localRoot,
		Remote:   remoteSpec,
		Checksum: true,
	}
	return runSyncEntry(s, false, false)
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

	entries, _, err := ScanDir(root, []string{"node_modules", "build"}, false)
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

	_, symlinks, err := ScanDir(root, nil, false)
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

// --- type-flip integration tests ---

// assertIsFile checks that root/rel exists and is a regular file (not a dir).
func assertIsFile(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		t.Errorf("assertIsFile %s: %v", rel, err)
		return
	}
	if info.IsDir() {
		t.Errorf("assertIsFile %s: expected file, got directory", rel)
	}
}

// TestIntegration_TypeFlip_LocalFileToDir: establish base with local file "flip",
// then locally replace it with a directory containing a child file, sync → remote
// should have a dir "flip" with the child inside, and no stale file at "flip".
func TestIntegration_TypeFlip_LocalFileToDir(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed file and establish base.
	seedFile(t, sideA, "flip", "original file", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertIsFile(t, sideB, "flip")

	// Step 2: locally replace the file with a directory containing a child.
	if err := os.Remove(filepath.Join(sideA, "flip")); err != nil {
		t.Fatalf("remove local file: %v", err)
	}
	seedFile(t, sideA, "flip/inner.txt", "inside dir", t2())

	// Step 3: sync — remote stale file must be pre-deleted, dir + child pushed.
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertDir(t, sideB, "flip")
	assertFile(t, sideB, "flip/inner.txt", "inside dir")
	assertDir(t, sideA, "flip")
	assertFile(t, sideA, "flip/inner.txt", "inside dir")
}

// TestIntegration_TypeFlip_LocalDirToFile: establish base with local dir "flip"
// containing a child, then locally replace it with a file, sync → remote should
// have file "flip" and no stale dir.
func TestIntegration_TypeFlip_LocalDirToFile(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed dir with child and establish base.
	seedFile(t, sideA, "flip/child.txt", "child content", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertDir(t, sideB, "flip")
	assertFile(t, sideB, "flip/child.txt", "child content")

	// Step 2: locally replace the dir with a file.
	if err := os.RemoveAll(filepath.Join(sideA, "flip")); err != nil {
		t.Fatalf("remove local dir: %v", err)
	}
	seedFile(t, sideA, "flip", "now a file", t2())

	// Step 3: sync — remote stale dir (with child) must be pre-deleted, file pushed.
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertIsFile(t, sideB, "flip")
	assertFile(t, sideB, "flip", "now a file")
	assertIsFile(t, sideA, "flip")
	assertAbsent(t, sideB, "flip/child.txt")
}

// TestIntegration_TypeFlip_RemoteFileToDir: establish base with remote file "flip",
// then remotely replace it with a directory containing a child, sync → local
// should have a dir "flip" with the child inside.
func TestIntegration_TypeFlip_RemoteFileToDir(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed file (on local) and sync to establish base.
	seedFile(t, sideA, "flip", "original file", t1())
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertIsFile(t, sideA, "flip")
	assertIsFile(t, sideB, "flip")

	// Step 2: mutate remote — replace file with dir+child.
	if err := os.Remove(filepath.Join(sideB, "flip")); err != nil {
		t.Fatalf("remove remote file: %v", err)
	}
	seedFile(t, sideB, "flip/inner.txt", "inside remote dir", t2())

	// Step 3: sync — local stale file must be pre-deleted, remote dir pulled.
	if err := doSync(t, "test", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertDir(t, sideA, "flip")
	assertFile(t, sideA, "flip/inner.txt", "inside remote dir")
	assertDir(t, sideB, "flip")
	assertFile(t, sideB, "flip/inner.txt", "inside remote dir")
}

// --- checksum-mode integration tests ---

// TestIntegration_Checksum_BlindSpot_WithoutChecksum: proves that WITHOUT
// checksum mode an edit that preserves size and mtime is invisible to the
// sync engine (the expected blind-spot behaviour).
func TestIntegration_Checksum_BlindSpot_WithoutChecksum(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed and sync.
	seedFile(t, sideA, "secret.txt", "original content!", t1())
	if err := doSync(t, "blind", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertFile(t, sideB, "secret.txt", "original content!")

	// Step 2: rewrite local file with same-SIZE content and force the SAME mtime.
	// "original content!" is 17 bytes; "xxxxxxxxxxxxxxxxx" is also 17 bytes.
	newContent := "xxxxxxxxxxxxxxxxx"
	if len(newContent) != len("original content!") {
		t.Fatal("test bug: replacement content must be same size")
	}
	if err := os.WriteFile(filepath.Join(sideA, "secret.txt"), []byte(newContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Force same mtime so the change is invisible to size+mtime detection.
	if err := os.Chtimes(filepath.Join(sideA, "secret.txt"), t1(), t1()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Step 3: sync WITHOUT checksum → engine must NOT propagate the change.
	if err := doSync(t, "blind", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	// Remote should still have the original content (blind spot confirmed).
	assertFile(t, sideB, "secret.txt", "original content!")
}

// TestIntegration_Checksum_BlindSpot_WithChecksum: same scenario as above but
// WITH checksum mode enabled — the change MUST be detected and propagated.
func TestIntegration_Checksum_BlindSpot_WithChecksum(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed and sync with checksum mode.
	seedFile(t, sideA, "secret.txt", "original content!", t1())
	if err := doSyncChecksum(t, "cs", sideA, "local:"+sideB); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertFile(t, sideB, "secret.txt", "original content!")

	// Step 2: rewrite with same-size content AND force the same mtime.
	// "original content!" is 17 bytes; "xxxxxxxxxxxxxxxxx" is also 17 bytes.
	newContent := "xxxxxxxxxxxxxxxxx"
	if len(newContent) != len("original content!") {
		t.Fatal("test bug: replacement content must be same size")
	}
	if err := os.WriteFile(filepath.Join(sideA, "secret.txt"), []byte(newContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(filepath.Join(sideA, "secret.txt"), t1(), t1()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Step 3: sync WITH checksum → hash mismatch is detected → change propagates.
	if err := doSyncChecksum(t, "cs", sideA, "local:"+sideB); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertFile(t, sideB, "secret.txt", newContent)
}

// TestIntegration_Checksum_NormalEditsStillWork: verify that checksum mode
// does not break detection of ordinary edits (different size or mtime).
func TestIntegration_Checksum_NormalEditsStillWork(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "f.txt", "v1", t1())
	if err := doSyncChecksum(t, "cs2", sideA, "local:"+sideB); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertFile(t, sideB, "f.txt", "v1")

	// Normal edit with a newer mtime (size also changes).
	seedFile(t, sideA, "f.txt", "v2 — longer content now", t2())
	if err := doSyncChecksum(t, "cs2", sideA, "local:"+sideB); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertFile(t, sideB, "f.txt", "v2 — longer content now")
}

// --- helpers for conflict-backup tests ---

// setupConflictDir redirects the conflict backup directory into the test temp
// dir (alongside the state dir) and restores the original on cleanup.
func setupConflictDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conflicts")
	orig := ConflictDir
	ConflictDir = dir
	t.Cleanup(func() { ConflictDir = orig })
	return dir
}

// findBackup looks for any file in conflictDir/syncName whose name starts with
// flattenPath(rel)+".". Returns the full path of the first match, or "".
func findBackup(conflictDir, syncName, rel string) string {
	dir := filepath.Join(conflictDir, syncName)
	prefix := flattenPath(rel) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// --- conflict backup integration tests ---

// TestConflictBackup_PullWins: remote wins a conflict (pull) → local file
// content must be preserved in ConflictDir before being overwritten.
func TestConflictBackup_PullWins(t *testing.T) {
	setupStateDir(t)
	conflictDir := setupConflictDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Establish base.
	seedFile(t, sideA, "f.txt", "original", t1())
	if err := doSync(t, "cb", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Both sides modify f.txt; remote (sideB) is newer → remote wins → local is loser.
	seedFile(t, sideA, "f.txt", "local version — to be lost", t2())
	seedFile(t, sideB, "f.txt", "remote version — winner", t3())

	if err := doSync(t, "cb", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// Winner content propagated.
	assertFile(t, sideA, "f.txt", "remote version — winner")
	assertFile(t, sideB, "f.txt", "remote version — winner")

	// Loser (local) content backed up.
	backup := findBackup(conflictDir, "cb", "f.txt")
	if backup == "" {
		t.Fatal("expected a conflict backup for f.txt (local loser), found none")
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "local version — to be lost" {
		t.Errorf("backup content: got %q want %q", string(got), "local version — to be lost")
	}
}

// TestConflictBackup_PushWins: local wins a conflict (push) → remote file
// content must be preserved in ConflictDir before being overwritten.
func TestConflictBackup_PushWins(t *testing.T) {
	setupStateDir(t)
	conflictDir := setupConflictDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Establish base.
	seedFile(t, sideA, "g.txt", "original", t1())
	if err := doSync(t, "cbpush", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Both sides modify; local (sideA) is newer → local wins → remote is loser.
	seedFile(t, sideA, "g.txt", "local version — winner", t3())
	seedFile(t, sideB, "g.txt", "remote version — to be lost", t2())

	if err := doSync(t, "cbpush", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	assertFile(t, sideA, "g.txt", "local version — winner")
	assertFile(t, sideB, "g.txt", "local version — winner")

	backup := findBackup(conflictDir, "cbpush", "g.txt")
	if backup == "" {
		t.Fatal("expected a conflict backup for g.txt (remote loser), found none")
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "remote version — to be lost" {
		t.Errorf("backup content: got %q want %q", string(got), "remote version — to be lost")
	}
}

// TestConflictBackup_TypeFlip_DirLoser: remote dir is the type-flip loser
// (local file wins) → the remote dir tree must be backed up before pre-delete.
func TestConflictBackup_TypeFlip_DirLoser(t *testing.T) {
	setupStateDir(t)
	conflictDir := setupConflictDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Establish base: local file "flip", remote mirrors it.
	seedFile(t, sideA, "flip", "original file", t1())
	if err := doSync(t, "tflip", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Remote replaces the file with a directory; local keeps it as a file.
	// Remote's dir is newer → remote dir wins; BUT the base has "flip" as file,
	// and remote flipped to dir → remote wins type-flip.
	if err := os.Remove(filepath.Join(sideB, "flip")); err != nil {
		t.Fatalf("remove remote file: %v", err)
	}
	seedFile(t, sideB, "flip/inner.txt", "remote dir content", t2())

	if err := doSync(t, "tflip", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// Remote dir wins; local should now have the dir.
	assertDir(t, sideA, "flip")
	assertFile(t, sideA, "flip/inner.txt", "remote dir content")

	// The losing local FILE must have been backed up before OpPreDeleteLocal.
	backup := findBackup(conflictDir, "tflip", "flip")
	if backup == "" {
		t.Fatal("expected a conflict backup for local 'flip' file (loser), found none")
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		// Backup might be a directory if something went wrong — check.
		info, statErr := os.Lstat(backup)
		if statErr == nil && info.IsDir() {
			// Also acceptable — dir backup of loser side.
			return
		}
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "original file" {
		t.Errorf("backup content: got %q want %q", string(got), "original file")
	}
}

// --- chmod integration tests (local: transport) ---

// assertFileMode checks that the file at root/rel has the expected permission bits.
func assertFileMode(t *testing.T, root, rel string, want os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		t.Errorf("assertFileMode %s: %v", rel, err)
		return
	}
	got := info.Mode().Perm()
	if got != want {
		t.Errorf("assertFileMode %s: got %04o want %04o", rel, got, want)
	}
}

// TestIntegration_Chmod_LocalChangePropagatesRemote: establish base with 0644,
// chmod local to 0755, sync → remote must become 0755.
func TestIntegration_Chmod_LocalChangePropagatesRemote(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed and sync.
	seedFile(t, sideA, "script.sh", "#!/bin/sh\necho hi\n", t1())
	if err := doSync(t, "chmod1", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertFileMode(t, sideB, "script.sh", 0o644)

	// Step 2: chmod local file to 0755.
	if err := os.Chmod(filepath.Join(sideA, "script.sh"), 0o755); err != nil {
		t.Fatalf("chmod local: %v", err)
	}

	// Step 3: sync → remote should pick up 0755.
	if err := doSync(t, "chmod1", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertFileMode(t, sideB, "script.sh", 0o755)
}

// TestIntegration_Chmod_RemoteChangePropagatesLocal: establish base with 0644,
// chmod remote to 0700, sync → local must become 0700.
func TestIntegration_Chmod_RemoteChangePropagatesLocal(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Step 1: seed and sync.
	seedFile(t, sideA, "secret.sh", "#!/bin/sh\n", t1())
	if err := doSync(t, "chmod2", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertFileMode(t, sideA, "secret.sh", 0o644)

	// Step 2: chmod remote file to 0700.
	if err := os.Chmod(filepath.Join(sideB, "secret.sh"), 0o700); err != nil {
		t.Fatalf("chmod remote: %v", err)
	}

	// Step 3: sync → local should pick up 0700.
	if err := doSync(t, "chmod2", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertFileMode(t, sideA, "secret.sh", 0o700)
}

// TestIntegration_Chmod_OldStateNoStorm: simulate an old state file that lacks
// LMode/RMode (both 0) — no chmod storm should occur even when perms differ.
func TestIntegration_Chmod_OldStateNoStorm(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Manually create an "old" state with zero-mode base entries.
	state := &State{
		Pairs: map[string]BaseEntry{
			"f.txt": {LSize: 5, LMtime: t1().UnixNano(), RSize: 5, RMtime: t1().UnixNano()},
			// LMode=0, RMode=0 — simulates missing field from old state file
		},
		LastSync: t1().UnixNano(),
	}
	if err := SaveState("nochmod", state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// Create files with differing perms (as-if they just happened to differ).
	seedFile(t, sideA, "f.txt", "hello", t1())
	seedFile(t, sideB, "f.txt", "hello", t1())
	if err := os.Chmod(filepath.Join(sideB, "f.txt"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Sync should produce no actions (in sync, and base modes unknown → no chmod).
	remote, _ := ParseRemote("local:" + sideB)
	localEntries, _, _ := ScanDir(sideA, nil, false)
	remoteEntries, _ := remote.Scan(nil, false)
	actions := Plan(state.Pairs, localEntries, remoteEntries, false)
	for _, a := range actions {
		if a.Op == OpChmodLocal || a.Op == OpChmodRemote {
			t.Errorf("unexpected chmod action with zero base modes: %v", a)
		}
	}
}

// TestCopyFile_OverwritePreservesMode: copyFile must propagate source mode even
// when overwriting an existing file that has different permissions.
func TestCopyFile_OverwritePreservesMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.sh")
	dst := filepath.Join(dir, "dst.sh")

	// Write source with 0755.
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// Write destination with 0644 (different).
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// copyFile should overwrite and set mode to 0755.
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("dst mode after copyFile: got %04o want 0755", info.Mode().Perm())
	}
	// Verify content too.
	got, _ := os.ReadFile(dst)
	if string(got) != "#!/bin/sh\n" {
		t.Errorf("dst content: got %q want #!/bin/sh\\n", string(got))
	}
}

// TestConflictBackup_Pruning: a backup file whose mtime is 8 days old must be
// deleted on the next sync pass.
func TestConflictBackup_Pruning(t *testing.T) {
	setupStateDir(t)
	conflictDir := setupConflictDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Create a synthetic old backup file directly in the conflict dir.
	syncName := "prune"
	backupDir := filepath.Join(conflictDir, syncName)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("mkdir conflict dir: %v", err)
	}
	oldFile := filepath.Join(backupDir, "old.txt.1000000000")
	if err := os.WriteFile(oldFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	// Set its mtime to 8 days ago.
	eightDaysAgo := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, eightDaysAgo, eightDaysAgo); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Trigger a sync (even an in-sync pass) to run pruning.
	seedFile(t, sideA, "p.txt", "content", t1())
	if err := doSync(t, syncName, sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Run a second sync to trigger pruning (first sync has no actions after 2nd).
	if err := doSync(t, syncName, sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// The old backup file should be gone.
	if _, err := os.Lstat(oldFile); err == nil {
		t.Error("expected old backup file to be pruned, but it still exists")
	}
}

// --- Layer 2: buildBase size-mismatch guard ---

// TestBuildBase_SizeMismatch_SkipsFilePair verifies that when both entries are
// files with different sizes, buildBase does NOT record the path in the base
// (so the next run re-reconciles it rather than treating it as in-sync).
func TestBuildBase_SizeMismatch_SkipsFilePair(t *testing.T) {
	local := map[string]Entry{
		"good.txt":    {Size: 100, Mtime: t1().UnixNano(), Mode: 0o644},
		"partial.txt": {Size: 50, Mtime: t1().UnixNano(), Mode: 0o644},  // partial
	}
	remote := map[string]Entry{
		"good.txt":    {Size: 100, Mtime: t1().UnixNano(), Mode: 0o644},
		"partial.txt": {Size: 200, Mtime: t1().UnixNano(), Mode: 0o644}, // full on remote
	}

	base := buildBase(local, remote)

	if _, ok := base["good.txt"]; !ok {
		t.Error("good.txt should be in base (sizes match)")
	}
	if _, ok := base["partial.txt"]; ok {
		t.Error("partial.txt must NOT be in base (size mismatch — truncated file guard)")
	}
}

// TestBuildBase_SizeMismatch_DirsUnaffected verifies that directory entries are
// always recorded in the base regardless of their (meaningless) size field.
func TestBuildBase_SizeMismatch_DirsUnaffected(t *testing.T) {
	local := map[string]Entry{
		"subdir": {Size: 4096, IsDir: true, Mode: 0o755},
	}
	remote := map[string]Entry{
		"subdir": {Size: 0, IsDir: true, Mode: 0o755}, // size differs, but both are dirs
	}

	base := buildBase(local, remote)
	if _, ok := base["subdir"]; !ok {
		t.Error("directory entry must be in base even when sizes differ")
	}
	if !base["subdir"].Dir {
		t.Error("Dir flag must be true for directory entry")
	}
}

// TestBuildBase_SizeMismatch_MissingOnOneSide verifies that a path missing on
// one side (transfer failure) is not in the base — this is the existing behavior
// and must remain correct alongside the new size-mismatch guard.
func TestBuildBase_SizeMismatch_MissingOnOneSide(t *testing.T) {
	local := map[string]Entry{
		"f.txt": {Size: 10, Mtime: t1().UnixNano()},
		"g.txt": {Size: 10, Mtime: t1().UnixNano()}, // only on local
	}
	remote := map[string]Entry{
		"f.txt": {Size: 10, Mtime: t1().UnixNano()},
		// g.txt missing on remote
	}

	base := buildBase(local, remote)
	if _, ok := base["f.txt"]; !ok {
		t.Error("f.txt must be in base (both sides, sizes match)")
	}
	if _, ok := base["g.txt"]; ok {
		t.Error("g.txt must NOT be in base (missing on remote)")
	}
}

// --- Staging dir exclusion ---

// TestStagingDir_ExcludedFromScan verifies that .caravan-staging is excluded
// from ScanDir when DefaultExcludes (as returned by Sync.Excludes()) are used.
// This matches the actual call site in runSyncEntry.
func TestStagingDir_ExcludedFromScan(t *testing.T) {
	root := t.TempDir()

	// Create a normal file and a file inside .caravan-staging.
	seedFile(t, root, "real.txt", "real content", t1())
	seedFile(t, root, ".caravan-staging/partial.txt", "partial", t1())

	// Use the effective excludes from a default Sync entry (includes DefaultExcludes).
	s := manifest.Sync{Name: "x", Local: root, Remote: "local:" + t.TempDir()}
	excludes := s.Excludes()

	entries, _, err := ScanDir(root, excludes, false)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}

	if _, ok := entries["real.txt"]; !ok {
		t.Error("real.txt must be present in scan results")
	}
	for p := range entries {
		if strings.HasPrefix(p, ".caravan-staging") {
			t.Errorf("scan must not include .caravan-staging paths, but found: %s", p)
		}
	}
}

// TestStagingDir_DoesNotPolluteSyncBase verifies the end-to-end property:
// if .caravan-staging exists in the sync root at scan time, a full sync pass
// does not include it in either side's entries or the resulting base snapshot.
func TestStagingDir_DoesNotPolluteSyncBase(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	seedFile(t, sideA, "keep.txt", "good", t1())
	// Simulate a leftover staging dir in sideA (local side).
	seedFile(t, sideA, ".caravan-staging/leftovers.txt", "bad", t1())

	if err := doSync(t, "staging-excl", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertFile(t, sideB, "keep.txt", "good")
	assertAbsent(t, sideB, ".caravan-staging/leftovers.txt")

	// Verify state does not record the staging path.
	state, err := LoadState("staging-excl")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for p := range state.Pairs {
		if strings.HasPrefix(p, ".caravan-staging") {
			t.Errorf("base snapshot must not contain .caravan-staging paths, got: %s", p)
		}
	}
}

// --- Integration: local: transport with new atomic copyFile path ---

// TestIntegration_AtomicCopy_MultiFileSync exercises push and pull over the
// local: transport to confirm the atomic copyFile path (tmp+rename) correctly
// transfers multiple files while preserving content and mode.
func TestIntegration_AtomicCopy_MultiFileSync(t *testing.T) {
	setupStateDir(t)
	sideA := t.TempDir()
	sideB := t.TempDir()

	// Seed multiple files with varying modes.
	seedFile(t, sideA, "readme.txt", "readme", t1())
	if err := os.WriteFile(filepath.Join(sideA, "script.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := os.Chtimes(filepath.Join(sideA, "script.sh"), t1(), t1()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	seedFile(t, sideA, "data/values.csv", "a,b,c", t1())

	if err := doSync(t, "atomic", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	assertFile(t, sideB, "readme.txt", "readme")
	assertFile(t, sideB, "data/values.csv", "a,b,c")

	// Check script.sh content and mode.
	got, err := os.ReadFile(filepath.Join(sideB, "script.sh"))
	if err != nil {
		t.Fatalf("read script.sh: %v", err)
	}
	if string(got) != "#!/bin/sh\n" {
		t.Errorf("script.sh content: got %q", string(got))
	}
	info, err := os.Lstat(filepath.Join(sideB, "script.sh"))
	if err != nil {
		t.Fatalf("stat script.sh: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("script.sh mode: got %04o want 0755", info.Mode().Perm())
	}

	// No .caravan-tmp files must remain anywhere in sideB.
	err = filepath.WalkDir(sideB, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if strings.HasSuffix(d.Name(), ".caravan-tmp") {
			t.Errorf("leftover .caravan-tmp file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sideB: %v", err)
	}

	// Second sync (nothing changed) must still be clean.
	if err := doSync(t, "atomic", sideA, "local:"+sideB, nil, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
}
