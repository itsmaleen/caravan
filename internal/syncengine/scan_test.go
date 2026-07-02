package syncengine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- entriesEqual table tests ---

func TestEntriesEqual(t *testing.T) {
	e1 := Entry{Size: 10, Mtime: 1000, Mode: 0o644, IsDir: false}
	e2 := Entry{Size: 20, Mtime: 2000, Mode: 0o644, IsDir: false}

	cases := []struct {
		name string
		a, b map[string]Entry
		want bool
	}{
		{
			name: "both empty",
			a:    map[string]Entry{},
			b:    map[string]Entry{},
			want: true,
		},
		{
			name: "identical single entry",
			a:    map[string]Entry{"f.txt": e1},
			b:    map[string]Entry{"f.txt": e1},
			want: true,
		},
		{
			name: "different value",
			a:    map[string]Entry{"f.txt": e1},
			b:    map[string]Entry{"f.txt": e2},
			want: false,
		},
		{
			name: "different key",
			a:    map[string]Entry{"a.txt": e1},
			b:    map[string]Entry{"b.txt": e1},
			want: false,
		},
		{
			name: "different lengths",
			a:    map[string]Entry{"a.txt": e1, "b.txt": e2},
			b:    map[string]Entry{"a.txt": e1},
			want: false,
		},
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "nil vs empty",
			a:    nil,
			b:    map[string]Entry{},
			want: true,
		},
		{
			name: "hash differs",
			a:    map[string]Entry{"f.txt": {Size: 5, Hash: "aaa"}},
			b:    map[string]Entry{"f.txt": {Size: 5, Hash: "bbb"}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := entriesEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("entriesEqual = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- waitForChange unit tests ---

// TestWaitForChange_ChangedEarly verifies that waitForChange returns early with
// changed=true when a file mutation is made before the window expires.
func TestWaitForChange_ChangedEarly(t *testing.T) {
	root := t.TempDir()
	// Seed an initial file.
	initial := filepath.Join(root, "file.txt")
	if err := os.WriteFile(initial, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	window := 5 * time.Second
	poll := 100 * time.Millisecond

	// Mutate the dir after a short delay in a goroutine.
	go func() {
		time.Sleep(300 * time.Millisecond)
		// Write a new file to trigger a change.
		_ = os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644)
	}()

	start := time.Now()
	_, changed := waitForChange(root, nil, false, window, poll)
	elapsed := time.Since(start)

	if !changed {
		t.Error("expected changed=true but got changed=false")
	}
	// Should have returned well before the window elapsed.
	if elapsed >= window-500*time.Millisecond {
		t.Errorf("expected early return but elapsed %v (window %v)", elapsed, window)
	}
}

// TestWaitForChange_NoChange verifies that waitForChange returns changed=false
// when no mutation occurs within the window.
func TestWaitForChange_NoChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}

	window := 600 * time.Millisecond
	poll := 80 * time.Millisecond

	start := time.Now()
	_, changed := waitForChange(root, nil, false, window, poll)
	elapsed := time.Since(start)

	if changed {
		t.Error("expected changed=false but got changed=true")
	}
	// Should have taken at least the window duration (with some tolerance).
	if elapsed < window-200*time.Millisecond {
		t.Errorf("returned too early: elapsed %v, window %v", elapsed, window)
	}
}

// TestWaitForChange_EmptyDir verifies that waitForChange on a non-existent dir
// returns changed=false (ScanDir returns empty maps, both are equal).
func TestWaitForChange_EmptyDir(t *testing.T) {
	root := t.TempDir()
	// Don't create anything; dir is empty.
	window := 400 * time.Millisecond
	poll := 80 * time.Millisecond

	_, changed := waitForChange(root, nil, false, window, poll)
	if changed {
		t.Error("expected changed=false for stable empty dir")
	}
}
