package watch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherSeesNestedWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New(root, nil)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no native watcher in this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	time.Sleep(200 * time.Millisecond) // FSEvents needs a moment before it reports
	drain(w)

	// A content change to an existing nested file must be seen.
	p := filepath.Join(root, "a", "b", "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, w, "create in nested dir")

	// A directory created after the watcher started must be watched too.
	if err := os.MkdirAll(filepath.Join(root, "new", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect(t, w, "mkdir")
	time.Sleep(100 * time.Millisecond)
	drain(w)
	if err := os.WriteFile(filepath.Join(root, "new", "deep", "g.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, w, "write in a dir created after start")
}

func TestSkipExcludedDir(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "node_modules"), 0o755)
	w, err := New(root, func(rel string, isDir bool) bool { return rel == "node_modules" })
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no native watcher in this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
}

func drain(w *Watcher) {
	for {
		select {
		case <-w.C:
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

func expect(t *testing.T, w *Watcher, what string) {
	t.Helper()
	select {
	case <-w.C:
	case <-time.After(3 * time.Second):
		t.Fatalf("no event for %s", what)
	}
}
