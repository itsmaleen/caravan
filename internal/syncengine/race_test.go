package syncengine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file written while a pass runs must not enter the new base, or its edit
// is recorded as synced without ever being transferred.
func TestDropUnstable(t *testing.T) {
	pre := map[string]Entry{
		"a.txt":    {Size: 10, Mtime: 1},
		"b.txt":    {Size: 10, Mtime: 1},
		"dir":      {IsDir: true, Mtime: 1},
		"pulled":   {Size: 5, Mtime: 1},
		"sub/x":    {Size: 3, Mtime: 1},
		"gone.txt": {Size: 2, Mtime: 1},
	}
	post := map[string]Entry{
		"a.txt":   {Size: 10, Mtime: 1}, // unchanged
		"b.txt":   {Size: 10, Mtime: 2}, // same-size edit mid-pass
		"dir":     {IsDir: true, Mtime: 9},
		"pulled":  {Size: 7, Mtime: 3}, // changed by the pass itself
		"new.txt": {Size: 1, Mtime: 5}, // created mid-pass
		"sub/x":   {Size: 4, Mtime: 4}, // under a touched dir
		// gone.txt deleted mid-pass
	}
	got := dropUnstable(pre, post, touchSet{deep: []string{"pulled", "sub"}})
	want := map[string]Entry{
		"a.txt":    {Size: 10, Mtime: 1},
		"b.txt":    {Size: 10, Mtime: 1}, // pre: the edit must still look like a change
		"dir":      {IsDir: true, Mtime: 9},
		"pulled":   {Size: 7, Mtime: 3},
		"sub/x":    {Size: 4, Mtime: 4},
		"gone.txt": {Size: 2, Mtime: 1}, // pre: the delete must still look like a change
	}
	if len(got) != len(want) {
		t.Errorf("got %d entries %v, want %d", len(got), got, len(want))
	}
	for p, w := range want {
		if g, ok := got[p]; !ok || g != w {
			t.Errorf("%s = %+v (present %v), want %+v", p, g, ok, w)
		}
	}
	if _, ok := got["new.txt"]; ok {
		t.Error("file created mid-pass entered the base")
	}
}

// A local delete made while a pass runs must become a remote delete on the
// next pass. Before the fix the file left the base and was copied back.
func TestDeleteDuringPassPropagates(t *testing.T) {
	f := Entry{Size: 4, Mtime: 100, Mode: 0o644}
	localPre := map[string]Entry{"keep.txt": f, "gone.txt": f}
	localPost := map[string]Entry{"keep.txt": f} // gone.txt deleted mid-pass
	remote := map[string]Entry{"keep.txt": f, "gone.txt": f}
	base := buildBase(dropUnstable(localPre, localPost, touchSet{}), remote)
	actions := Plan(base, localPost, remote, false)
	if len(actions) != 1 || actions[0].Op != OpDeleteRemote || actions[0].Path != "gone.txt" {
		t.Fatalf("next pass plans %+v, want one deleteRemote gone.txt", actions)
	}
}

func TestTouchedPaths(t *testing.T) {
	l, r := touchedPaths([]Action{{Op: OpPull, Path: "p"}, {Op: OpPush, Path: "q"}, {Op: OpDeleteRemote, Path: "d"}, {Op: OpMkdirLocal, Path: "m"}})
	if len(l.deep) != 2 || l.deep[0] != "p" || l.deep[1] != "m" || len(l.shallow) != 0 {
		t.Errorf("local = %v", l)
	}
	if len(r.deep) != 2 || r.deep[0] != "q" || r.deep[1] != "d" || len(r.shallow) != 0 {
		t.Errorf("remote = %v", r)
	}
	l, r = touchedPaths([]Action{{Op: OpChmodLocal, Path: "a"}, {Op: OpChmodRemote, Path: "b"}})
	if len(l.deep) != 0 || len(l.shallow) != 1 || l.shallow[0] != "a" {
		t.Errorf("chmod local = %v", l)
	}
	if len(r.deep) != 0 || len(r.shallow) != 1 || r.shallow[0] != "b" {
		t.Errorf("chmod remote = %v", r)
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
	if _, ok := remoteAfterPass(r, pre, []Action{{Op: OpPush, Path: "missing.txt"}}, touchSet{deep: []string{"missing.txt"}}, false); ok {
		t.Error("missing pushed file accepted")
	}
}

// A chmod on a remote directory must not drop the directory's unchanged
// children from the remote snapshot: the base would lose them, and a local
// delete made during that pass would be undone.
func TestRemoteAfterPassChmodDirKeepsChildren(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "d", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := Entry{Size: 4, Mtime: 100, Mode: 0o644}
	pre := map[string]Entry{
		"d":       {IsDir: true, Mtime: 1, Mode: 0o755},
		"d/f":     f,
		"d/sub":   {IsDir: true, Mtime: 1, Mode: 0o755},
		"d/sub/g": f,
	}
	r := &RemoteConn{Kind: transportLocal, Root: root}
	actions := []Action{{Op: OpChmodRemote, Path: "d"}}
	_, touched := touchedPaths(actions)
	got, ok := remoteAfterPass(r, pre, actions, touched, false)
	if !ok {
		t.Fatal("fell back to full scan")
	}
	if got["d/f"] != f || got["d/sub/g"] != f {
		t.Errorf("children of a chmod'd dir were dropped or refreshed: %+v", got)
	}
	if got["d"].Mtime == 1 {
		t.Error("chmod'd dir was not refreshed")
	}

	// d/f deleted locally while the pass ran: the next pass must delete it on
	// the remote, not copy it back.
	localPre := map[string]Entry{"d": pre["d"], "d/f": f}
	localPost := map[string]Entry{"d": pre["d"]}
	remoteNow := map[string]Entry{"d": got["d"], "d/f": f}
	base := buildBase(dropUnstable(localPre, localPost, touchSet{}), got)
	next := Plan(base, localPost, remoteNow, false)
	if len(next) != 1 || next[0].Op != OpDeleteRemote || next[0].Path != "d/f" {
		t.Fatalf("next pass plans %+v, want one deleteRemote d/f", next)
	}
}

// The same on the full-scan path and on the local side: a file under a
// chmod'd directory is not touched, so an edit or delete made to it during
// the pass must keep its pre-pass entry.
func TestDropUnstableChmodDirIsShallow(t *testing.T) {
	pre := map[string]Entry{
		"d":      {IsDir: true, Mtime: 1},
		"d/edit": {Size: 4, Mtime: 1},
		"d/gone": {Size: 4, Mtime: 1},
		"x":      {Size: 4, Mtime: 1, Mode: 0o644},
	}
	post := map[string]Entry{
		"d":      {IsDir: true, Mtime: 9},
		"d/edit": {Size: 4, Mtime: 2},              // same-size edit mid-pass
		"x":      {Size: 4, Mtime: 1, Mode: 0o755}, // chmod'd by the pass
	}
	got := dropUnstable(pre, post, touchSet{shallow: []string{"d", "x"}})
	if got["d/edit"] != pre["d/edit"] {
		t.Errorf("d/edit = %+v, want the pre-pass entry", got["d/edit"])
	}
	if e, ok := got["d/gone"]; !ok || e != pre["d/gone"] {
		t.Errorf("d/gone = %+v (present %v), want the pre-pass entry", e, ok)
	}
	if got["x"] != post["x"] {
		t.Errorf("chmod'd file = %+v, want the fresh entry", got["x"])
	}
}

// A remote writer that edits a pushed file between placement and the stat,
// keeping its size, must not have its version recorded as the synced one.
// With checksums off nothing else would ever notice the edit.
func TestRemoteEditAfterPushIsNotRecordedAsSynced(t *testing.T) {
	localRoot, remoteRoot := t.TempDir(), t.TempDir()
	lp, rp := filepath.Join(localRoot, "f.txt"), filepath.Join(remoteRoot, "f.txt")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.WriteFile(rp, []byte("old!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &RemoteConn{Kind: transportLocal, Root: remoteRoot}
	localPre, _, _ := ScanDir(localRoot, nil, false)
	remotePre, _, _ := ScanDir(remoteRoot, nil, false)
	oldBase := buildBase(map[string]Entry{"f.txt": remotePre["f.txt"]}, remotePre) // in sync before the local edit
	actions := Plan(oldBase, localPre, remotePre, false)
	if len(actions) != 1 || actions[0].Op != OpPush {
		t.Fatalf("setup: plan = %+v, want one push", actions)
	}
	_, touched := touchedPaths(actions)

	settle := func() (map[string]BaseEntry, map[string]Entry) {
		localPost, _, _ := ScanDir(localRoot, nil, false)
		remotePost, ok := remoteAfterPass(r, remotePre, actions, touched, false)
		if !ok {
			t.Fatal("fell back to full scan")
		}
		base := buildBase(dropUnstable(localPre, localPost, touchSet{}), remotePost)
		holdBackUnconfirmed(base, oldBase, actions, localPre, localPost, remotePre, remotePost)
		return base, remotePost
	}

	// Control: an undisturbed push is recorded, and the next pass is a no-op.
	if err := r.Push(localRoot, []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}
	base, remotePost := settle()
	if _, ok := base["f.txt"]; !ok {
		t.Fatal("undisturbed push left out of the base")
	}
	remoteNow, _, _ := ScanDir(remoteRoot, nil, false)
	if next := Plan(base, localPre, remoteNow, false); len(next) != 0 {
		t.Fatalf("undisturbed push: next pass plans %+v, want nothing", next)
	}
	if EntriesDigest(remotePost) != EntriesDigest(remoteNow) {
		t.Error("undisturbed push: the snapshot digest does not match the remote")
	}

	// Same size, new mtime, written after placement and before the stat.
	if err := os.WriteFile(rp, []byte("hers"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(rp, later, later); err != nil {
		t.Fatal(err)
	}
	base, remotePost = settle()
	if base["f.txt"] != oldBase["f.txt"] {
		t.Errorf("base = %+v, want the pre-pass entry %+v", base["f.txt"], oldBase["f.txt"])
	}
	remoteNow, _, _ = ScanDir(remoteRoot, nil, false)
	next := Plan(base, localPre, remoteNow, false)
	if len(next) != 1 || !next[0].Conflict || next[0].Path != "f.txt" {
		t.Fatalf("next pass plans %+v, want one conflict on f.txt", next)
	}
	// The long-poll must report a change, or that next pass never runs.
	if EntriesDigest(remotePost) == EntriesDigest(remoteNow) {
		t.Error("the snapshot digest matches the edited remote: the long-poll would stay quiet")
	}
}

func TestInstalled(t *testing.T) {
	sec := int64(time.Second)
	src := Entry{Size: 4, Mtime: 10*sec + 123456789}
	for _, c := range []struct {
		name string
		dst  Entry
		want bool
	}{
		{"exact", Entry{Size: 4, Mtime: src.Mtime}, true},
		{"truncated to the second", Entry{Size: 4, Mtime: 10 * sec}, true},
		{"truncated to the microsecond", Entry{Size: 4, Mtime: 10*sec + 123456000}, true},
		{"newer", Entry{Size: 4, Mtime: src.Mtime + 1}, false},
		{"a second older", Entry{Size: 4, Mtime: src.Mtime - sec}, false},
		{"other size", Entry{Size: 5, Mtime: src.Mtime}, false},
		{"became a dir", Entry{IsDir: true}, false},
		{"same hash, other mtime", Entry{Size: 4, Mtime: 99 * sec, Hash: "h"}, true},
		{"other hash, same mtime", Entry{Size: 4, Mtime: src.Mtime, Hash: "x"}, false},
	} {
		s := src
		if c.dst.Hash != "" {
			s.Hash = "h"
		}
		if got := installed(s, c.dst); got != c.want {
			t.Errorf("%s: installed = %v, want %v", c.name, got, c.want)
		}
	}
}
