package syncengine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- stagedExtractScript unit tests ---

// TestStagedExtractScript_AbsPath checks that an absolute path produces a script
// that contains all the required staging steps joined with &&.
func TestStagedExtractScript_AbsPath(t *testing.T) {
	script := stagedExtractScript(`"/srv/sync"`)

	steps := []string{
		`root=`,
		`rm -rf "${root}/.caravan-staging"`,
		`mkdir -p "${root}/.caravan-staging"`,
		`tar -C "${root}/.caravan-staging" -xpf -`,
		`find . -type f -print0`,
		`mv -f "$f" "${root}/$f"`,
		`rm -rf "${root}/.caravan-staging"`,
	}
	for _, step := range steps {
		if !strings.Contains(script, step) {
			t.Errorf("script missing expected step %q\nfull script:\n%s", step, script)
		}
	}

	// Verify && ordering: no step that moves files should precede tar extraction.
	tarIdx := strings.Index(script, "tar -C")
	mvIdx := strings.Index(script, "mv -f")
	if tarIdx < 0 || mvIdx < 0 {
		t.Fatal("script must contain both tar and mv steps")
	}
	if mvIdx <= tarIdx {
		t.Errorf("mv step (idx=%d) must come AFTER tar step (idx=%d)", mvIdx, tarIdx)
	}

	// Verify that the top-level pipeline phases are joined with && (not \n).
	// Note: semicolons are permitted INSIDE the while-loop body (POSIX shell syntax).
	if strings.Contains(script, "\n") {
		t.Error("script must be a single line (no newlines)")
	}
	// All top-level phases must be &&-joined; count that && is present.
	if !strings.Contains(script, "&&") {
		t.Error("script must use && to join top-level phases")
	}
}

// TestStagedExtractScript_HomePath checks that a $HOME-expanded path (from
// shellRemotePath("~/sync")) produces a valid script.
func TestStagedExtractScript_HomePath(t *testing.T) {
	// shellRemotePath("~/sync") returns `"$HOME/sync"`
	rootExpr := shellRemotePath("~/sync")
	script := stagedExtractScript(rootExpr)

	if !strings.Contains(script, "$HOME/sync") {
		t.Errorf("expected $HOME/sync in script:\n%s", script)
	}
	if !strings.Contains(script, ".caravan-staging") {
		t.Errorf("expected .caravan-staging in script:\n%s", script)
	}
	// All top-level phases must still be &&-joined (single line).
	if strings.Contains(script, "\n") {
		t.Error("script must be a single line (no newlines)")
	}
	if !strings.Contains(script, "&&") {
		t.Error("script must use && to join top-level phases")
	}
}

// TestStagedExtractScript_BareHome checks that a bare "~" root works.
func TestStagedExtractScript_BareHome(t *testing.T) {
	rootExpr := shellRemotePath("~")
	script := stagedExtractScript(rootExpr)
	if !strings.Contains(script, `.caravan-staging`) {
		t.Errorf("expected .caravan-staging in script:\n%s", script)
	}
}

// TestStagedExtractScript_StepsInOrder checks that the staging steps appear in
// the correct && order: rm → mkdir → tar → find/mv → cleanup rm.
func TestStagedExtractScript_StepsInOrder(t *testing.T) {
	script := stagedExtractScript(`"/data/sync"`)

	// Split on && and verify the phase ordering.
	parts := strings.Split(script, "&&")
	// We expect at least 6 parts: root=…, rm -rf staging, mkdir staging, tar, find/while, cd/rm.
	if len(parts) < 6 {
		t.Fatalf("expected at least 6 &&-joined parts, got %d:\n%s", len(parts), script)
	}

	firstRM := -1
	mkdirIdx := -1
	tarIdx := -1
	findIdx := -1
	lastRM := -1
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "rm -rf") && firstRM == -1 {
			firstRM = i
		}
		if strings.HasPrefix(p, "mkdir") {
			mkdirIdx = i
		}
		if strings.HasPrefix(p, "tar") {
			tarIdx = i
		}
		if strings.HasPrefix(p, "find") || strings.HasPrefix(p, "cd") && strings.Contains(p, "find") {
			findIdx = i
		}
		if strings.HasPrefix(p, "rm -rf") {
			lastRM = i
		}
	}

	if firstRM < 0 || mkdirIdx < 0 || tarIdx < 0 || findIdx < 0 {
		t.Fatalf("could not locate all phases in script parts: firstRM=%d mkdir=%d tar=%d find=%d\nparts: %v",
			firstRM, mkdirIdx, tarIdx, findIdx, parts)
	}
	if !(firstRM < mkdirIdx) {
		t.Errorf("rm must precede mkdir: firstRM=%d mkdirIdx=%d", firstRM, mkdirIdx)
	}
	if !(mkdirIdx < tarIdx) {
		t.Errorf("mkdir must precede tar: mkdirIdx=%d tarIdx=%d", mkdirIdx, tarIdx)
	}
	if !(tarIdx < findIdx) {
		t.Errorf("tar must precede find/move: tarIdx=%d findIdx=%d", tarIdx, findIdx)
	}
	if !(findIdx < lastRM) {
		t.Errorf("find/move must precede cleanup rm: findIdx=%d lastRM=%d", findIdx, lastRM)
	}
}

// --- copyFile atomicity unit tests ---

// TestCopyFile_Atomic_NoTmpOnSuccess verifies that after a successful copyFile
// call, no .caravan-tmp file is left in the destination directory.
func TestCopyFile_Atomic_NoTmpOnSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	if err := os.WriteFile(src, []byte("hello atomic"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	// The tmp file must not exist after success.
	tmp := dst + ".caravan-tmp"
	if _, err := os.Lstat(tmp); err == nil {
		t.Errorf(".caravan-tmp file still exists after successful copyFile: %s", tmp)
	}

	// Content must be correct.
	got, _ := os.ReadFile(dst)
	if string(got) != "hello atomic" {
		t.Errorf("dst content: got %q want \"hello atomic\"", string(got))
	}
}

// TestCopyFile_Atomic_DstUntouchedOnSrcError verifies that if the source cannot
// be opened, the destination is not created or truncated.
func TestCopyFile_Atomic_DstUntouchedOnSrcError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.txt")

	// Write initial dst content.
	original := "original content"
	if err := os.WriteFile(dst, []byte(original), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// Try to copy from a non-existent source.
	err := copyFile(filepath.Join(dir, "nonexistent.txt"), dst)
	if err == nil {
		t.Fatal("expected error when source does not exist, got nil")
	}

	// Destination must be untouched.
	got, err2 := os.ReadFile(dst)
	if err2 != nil {
		t.Fatalf("read dst after error: %v", err2)
	}
	if string(got) != original {
		t.Errorf("dst was modified on error: got %q want %q", string(got), original)
	}

	// No .caravan-tmp file must remain.
	tmp := dst + ".caravan-tmp"
	if _, err3 := os.Lstat(tmp); err3 == nil {
		t.Errorf(".caravan-tmp file left behind on error: %s", tmp)
	}
}

// TestCopyFile_Atomic_ModePreserved verifies that the mode on the source is
// preserved on the destination after an atomic copy.
func TestCopyFile_Atomic_ModePreserved(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "script.sh")
	dst := filepath.Join(dir, "dst.sh")

	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// dst exists with a different mode.
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("dst mode: got %04o want 0755", info.Mode().Perm())
	}
	// No leftover tmp.
	if _, err := os.Lstat(dst + ".caravan-tmp"); err == nil {
		t.Error(".caravan-tmp file unexpectedly exists after successful copy")
	}
}

// TestSSHBaseArgsKeepalive pins the ssh reliability options so they cannot
// silently regress: dropping ServerAliveInterval reintroduces the sleep-induced
// watch-loop hang. The ControlPath is supplied by the caller (per-entry).
func TestSSHBaseArgsKeepalive(t *testing.T) {
	cp := "/tmp/caravan-1/s-2-deadbeef-%r@%h-%p"
	args := strings.Join(sshBaseArgs(cp), " ")
	for _, want := range []string{
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
		"ConnectTimeout=10",
		"ControlPath=" + cp,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("sshBaseArgs() missing %q\n  got: %s", want, args)
		}
	}
}

// TestControlPathIsolation pins the socket-isolation invariants: the path is
// scoped by process (pid) AND by entry (host+root), lives under the per-uid 0700
// sockDir, so concurrent processes and sibling entries never share — nor tear
// down — one another's ssh master.
func TestControlPathIsolation(t *testing.T) {
	a := (&RemoteConn{Kind: transportSSH, Host: "m@host", Root: "~/one"}).controlPath()
	b := (&RemoteConn{Kind: transportSSH, Host: "m@host", Root: "~/two"}).controlPath()
	if a == b {
		t.Errorf("two entries to the same host share a ControlPath: %s", a)
	}
	dir := sockDir()
	if !strings.HasPrefix(a, dir+"/") {
		t.Errorf("ControlPath %q is not under sockDir %q", a, dir)
	}
	if !strings.Contains(a, fmt.Sprintf("s-%d-", os.Getpid())) {
		t.Errorf("ControlPath %q is missing pid scoping", a)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("sockDir %q is not 0700 (err=%v)", dir, err)
	}
	if len(strings.ReplaceAll(a, "%", "")) > 104 {
		t.Errorf("ControlPath %q may exceed the unix socket path limit", a)
	}
}
