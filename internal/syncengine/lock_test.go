package syncengine

import (
	"errors"
	"testing"
)

// TestLock_AcquireReleaseAcquire verifies the basic acquire/busy/release cycle.
// We open separate fds per acquire (via AcquireSyncLock) so that same-process
// flock re-entrance does not falsely succeed.
func TestLock_AcquireReleaseAcquire(t *testing.T) {
	// Redirect state to a temp dir so we don't pollute ~/.config/caravan.
	orig := StateDir
	StateDir = t.TempDir()
	t.Cleanup(func() { StateDir = orig })

	const name = "test-entry"

	// First acquire should succeed.
	release1, err := AcquireSyncLock(name)
	if err != nil {
		t.Fatalf("first acquire: unexpected error: %v", err)
	}

	// Second acquire (different fd) must fail with ErrSyncBusy.
	_, err2 := AcquireSyncLock(name)
	if !errors.Is(err2, ErrSyncBusy) {
		t.Fatalf("second acquire: expected ErrSyncBusy, got %v", err2)
	}

	// Release the first lock.
	release1()

	// Third acquire (after release) must succeed.
	release3, err := AcquireSyncLock(name)
	if err != nil {
		t.Fatalf("third acquire (after release): unexpected error: %v", err)
	}
	release3()
}

// TestLock_DifferentNames verifies two distinct entries don't block each other.
func TestLock_DifferentNames(t *testing.T) {
	orig := StateDir
	StateDir = t.TempDir()
	t.Cleanup(func() { StateDir = orig })

	r1, err := AcquireSyncLock("alpha")
	if err != nil {
		t.Fatalf("acquire alpha: %v", err)
	}
	defer r1()

	r2, err := AcquireSyncLock("beta")
	if err != nil {
		t.Fatalf("acquire beta (different name): %v", err)
	}
	r2()
}
