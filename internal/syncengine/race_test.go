package syncengine

import (
	"os"
	"path/filepath"
	"testing"
)

// A file written while a pass runs must not enter the new base, or its edit
// is recorded as synced without ever being transferred.
func TestDropUnstable(t *testing.T) {
	pre := map[string]Entry{
		"a.txt":  {Size: 10, Mtime: 1},
		"b.txt":  {Size: 10, Mtime: 1},
		"dir":    {IsDir: true, Mtime: 1},
		"pulled": {Size: 5, Mtime: 1},
		"sub/x":  {Size: 3, Mtime: 1},
	}
	post := map[string]Entry{
		"a.txt":   {Size: 10, Mtime: 1}, // unchanged: keep
		"b.txt":   {Size: 10, Mtime: 2}, // same-size edit mid-pass: drop
		"dir":     {IsDir: true, Mtime: 9},
		"pulled":  {Size: 7, Mtime: 3}, // changed by the pass itself: keep
		"new.txt": {Size: 1, Mtime: 5}, // created mid-pass: drop
		"sub/x":   {Size: 4, Mtime: 4}, // under a touched dir: keep
	}
	got := dropUnstable(pre, post, []string{"pulled", "sub"})
	for _, want := range []string{"a.txt", "dir", "pulled", "sub/x"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s dropped, want kept", want)
		}
	}
	for _, gone := range []string{"b.txt", "new.txt"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s kept, want dropped", gone)
		}
	}
}

func TestTouchedPaths(t *testing.T) {
	l, r := touchedPaths([]Action{{Op: OpPull, Path: "p"}, {Op: OpPush, Path: "q"}, {Op: OpDeleteRemote, Path: "d"}, {Op: OpMkdirLocal, Path: "m"}})
	if len(l) != 2 || l[0] != "p" || l[1] != "m" {
		t.Errorf("local = %v", l)
	}
	if len(r) != 2 || r[0] != "q" || r[1] != "d" {
		t.Errorf("remote = %v", r)
	}
}

// remoteAfterPass must keep untouched entries as they were before the pass,
// refresh touched paths and their parents, and drop deleted subtrees.
func TestRemoteAfterPassLocalTransport(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, s string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("keep.txt", "k")
	mustWrite("d/pushed.txt", "new content")
	pre := map[string]Entry{
		"keep.txt":     {Size: 1, Mtime: 111}, // deliberately stale: must be kept as is
		"d":            {IsDir: true, Mtime: 1},
		"gone":         {IsDir: true, Mtime: 1},
		"gone/a.txt":   {Size: 1, Mtime: 1},
		"d/pushed.txt": {Size: 3, Mtime: 1},
	}
	r := &RemoteConn{Kind: transportLocal, Root: root}
	actions := []Action{{Op: OpPush, Path: "d/pushed.txt"}, {Op: OpDeleteRemote, Path: "gone"}}
	_, touched := touchedPaths(actions)
	got, ok := remoteAfterPass(r, pre, actions, touched, false)
	if !ok {
		t.Fatal("fell back to full scan")
	}
	if got["keep.txt"].Mtime != 111 {
		t.Errorf("untouched entry was refreshed: %+v", got["keep.txt"])
	}
	if got["d/pushed.txt"].Size != int64(len("new content")) {
		t.Errorf("pushed file not refreshed: %+v", got["d/pushed.txt"])
	}
	if _, ok := got["gone/a.txt"]; ok {
		t.Error("deleted subtree survived")
	}
	if got["d"].Mtime == 1 {
		t.Error("parent dir of a pushed file was not refreshed")
	}

	// A push that did not land must force the full-scan fallback.
	if _, ok := remoteAfterPass(r, pre, []Action{{Op: OpPush, Path: "missing.txt"}}, []string{"missing.txt"}, false); ok {
		t.Error("missing pushed file accepted")
	}
}
