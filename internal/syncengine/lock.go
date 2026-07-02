package syncengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrSyncBusy is returned by AcquireSyncLock when another process (or goroutine
// with a separate fd) already holds the lock for the named sync entry.
var ErrSyncBusy = errors.New("sync already running for this entry")

// AcquireSyncLock creates (or opens) a per-entry advisory lock file under
// StateDir and takes a non-blocking exclusive flock on it. If the lock is
// already held the returned error is ErrSyncBusy.
//
// The returned release func unlocks and closes the file. The lock file is left
// on disk — its presence is harmless and avoids TOCTOU races.
//
// Note: we open a fresh file descriptor each time so that same-process callers
// (e.g. tests) get real exclusion rather than kernel-level fd re-entrance.
func AcquireSyncLock(name string) (release func(), err error) {
	dir := resolvedStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: mkdir state dir: %w", err)
	}

	lockPath := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", lockPath, err)
	}

	// Non-blocking exclusive flock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrSyncBusy
		}
		return nil, fmt.Errorf("lock: flock %s: %w", lockPath, err)
	}

	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, nil
}
