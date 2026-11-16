package syncengine

import "testing"

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
