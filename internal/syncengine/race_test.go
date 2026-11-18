package syncengine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"caravan/internal/manifest"
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

// A chmod refreshes a file's mode and nothing else. A same-size edit made to
// the file while the pass ran was never transferred: recording its new mtime
// or hash beside the other side's old content would hide the edit for good.
func TestDropUnstableChmodFileKeepsContent(t *testing.T) {
	pre := map[string]Entry{
		"x":    {Size: 4, Mtime: 1, Mode: 0o644, Hash: "old"},
		"gone": {Size: 4, Mtime: 1, Mode: 0o644},
	}
	post := map[string]Entry{
		"x": {Size: 4, Mtime: 2, Mode: 0o755, Hash: "new"}, // chmod'd by the pass, edited by someone else
		// gone: chmod'd by the pass, then deleted by someone else
	}
	got := dropUnstable(pre, post, touchSet{shallow: []string{"x", "gone"}})
	if want := (Entry{Size: 4, Mtime: 1, Mode: 0o755, Hash: "old"}); got["x"] != want {
		t.Errorf("x = %+v, want %+v (new mode, pre-pass content)", got["x"], want)
	}
	if e, ok := got["gone"]; !ok || e != pre["gone"] {
		t.Errorf("gone = %+v (present %v), want the pre-pass entry so the delete is seen", e, ok)
	}

	// End to end: the remote's mode changed, the pass chmods the local file,
	// and a local writer edits it (same size) before the post-pass scan. The
	// next pass must push the edit.
	synced := Entry{Size: 4, Mtime: 100, Mode: 0o644}
	remote := map[string]Entry{"f": {Size: 4, Mtime: 100, Mode: 0o755}}
	localPre := map[string]Entry{"f": synced}
	oldBase := buildBase(localPre, map[string]Entry{"f": synced})
	actions := Plan(oldBase, localPre, remote, false)
	if len(actions) != 1 || actions[0].Op != OpChmodLocal {
		t.Fatalf("setup: plan = %+v, want one chmodLocal", actions)
	}
	touchedL, _ := touchedPaths(actions)
	localPost := map[string]Entry{"f": {Size: 4, Mtime: 200, Mode: 0o755}}
	base := buildBase(dropUnstable(localPre, localPost, touchedL), remote)
	next := Plan(base, localPost, remote, false)
	if len(next) != 1 || next[0].Op != OpPush || next[0].Path != "f" {
		t.Fatalf("next pass plans %+v, want one push of f", next)
	}
}

// The same on the remote side: the stat after a chmodRemote must not adopt an
// edit or a delete a remote writer made to the file during the pass.
func TestRemoteAfterPassChmodFileKeepsContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("hers"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The pass's chmod (WriteFile's mode is subject to the umask).
	if err := os.Chmod(filepath.Join(root, "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := map[string]Entry{
		"f":    {Size: 4, Mtime: 100, Mode: 0o644}, // as planned; the file on disk is a later same-size edit
		"gone": {Size: 4, Mtime: 100, Mode: 0o644}, // deleted on the remote during the pass
	}
	r := &RemoteConn{Kind: transportLocal, Root: root}
	actions := []Action{{Op: OpChmodRemote, Path: "f", Mode: 0o755}, {Op: OpChmodRemote, Path: "gone", Mode: 0o755}}
	_, touched := touchedPaths(actions)
	got, ok := remoteAfterPass(r, pre, actions, touched, false)
	if !ok {
		t.Fatal("fell back to full scan")
	}
	if want := (Entry{Size: 4, Mtime: 100, Mode: got["f"].Mode}); got["f"] != want || got["f"].Mode&0o777 != 0o755 {
		t.Errorf("f = %+v, want mtime 100 and mode 0755", got["f"])
	}
	if e, ok := got["gone"]; !ok || e != pre["gone"] {
		t.Errorf("gone = %+v (present %v), want the pre-pass entry", e, ok)
	}
	// The long-poll must see the remote as changed, or the next pass never runs.
	now, _, _ := ScanDir(root, nil, false)
	if EntriesDigest(got) == EntriesDigest(now) {
		t.Error("the snapshot digest matches the edited remote: the long-poll would stay quiet")
	}
}

func TestPassRetryBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	var r passRetry
	if r.C(now) != nil {
		t.Fatal("a retry is scheduled before any pass")
	}
	r.after(false, now)
	if r.C(now) != nil {
		t.Fatal("a clean pass scheduled a retry")
	}
	want := passRetryBase
	for i := 0; i < 12; i++ {
		r.after(true, now)
		if got := r.at.Sub(now); got != want {
			t.Fatalf("retry %d due in %s, want %s", i, got, want)
		}
		if r.C(now) == nil {
			t.Fatalf("retry %d: no timer", i)
		}
		if want *= 2; want > passRetryMax {
			want = passRetryMax
		}
	}
	r.after(false, now)
	if r.C(now) != nil || r.backoff != 0 {
		t.Fatalf("a clean pass did not reset the retry: %+v", r)
	}
}

// A pull that fails before anything is placed leaves both snapshots matching
// their trees, so neither the local rescan nor the remote long-poll would
// ever fire. The pass must report the unfinished work so the loop retries.
func TestFailedPullReportsPendingWork(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	setupStateDir(t)
	localRoot, remoteRoot := t.TempDir(), t.TempDir()
	rp := filepath.Join(remoteRoot, "f.txt")
	if err := os.WriteFile(rp, []byte("data"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rp, 0o644) })
	s := manifest.Sync{Name: "pending-pull", Local: localRoot, Remote: "local:" + remoteRoot}

	seen, remoteSeen, pending, _ := syncPass(s, false, true)
	if !pending {
		t.Fatal("a failed pull was not reported as pending work")
	}
	// The premise: nothing else would trigger the retry.
	cur, _, _ := ScanDir(localRoot, nil, false)
	rnow, _, _ := ScanDir(remoteRoot, nil, false)
	if !entriesEqual(seen, cur) || EntriesDigest(remoteSeen) != EntriesDigest(rnow) {
		t.Log("note: a snapshot differs, so a pass would also be triggered by change detection")
	}

	if err := os.Chmod(rp, 0o644); err != nil {
		t.Fatal(err)
	}
	// The mode change alone is a remote change; the retry must finish the pull.
	if _, _, pending, err := syncPass(s, false, true); err != nil || pending {
		t.Fatalf("retry: pending=%v err=%v, want a clean pass", pending, err)
	}
	if b, err := os.ReadFile(filepath.Join(localRoot, "f.txt")); err != nil || string(b) != "data" {
		t.Fatalf("after the retry local f.txt = %q, %v", b, err)
	}
}

// A delete that fails leaves the file on one side only. The retry must try
// the delete again, not take the survivor for a new file and copy it back.
func TestFailedDeleteIsRetriedNotUndone(t *testing.T) {
	f := Entry{Size: 4, Mtime: 100, Mode: 0o644}
	d := Entry{IsDir: true, Mtime: 1, Mode: 0o755}
	for _, side := range []string{"remote", "local"} {
		both := map[string]Entry{"d": d, "d/f": f}
		gone := map[string]Entry{"d": d} // the user deleted d/f on this side
		oldBase := buildBase(both, both)
		localPre, remotePre, wantOp := gone, both, OpDeleteRemote
		if side == "local" {
			localPre, remotePre, wantOp = both, gone, OpDeleteLocal
		}
		actions := Plan(oldBase, localPre, remotePre, false)
		if len(actions) != 1 || actions[0].Op != wantOp {
			t.Fatalf("%s: setup: plan = %+v, want one %s", side, actions, wantOp)
		}
		// The delete fails: both sides look exactly as before the pass.
		touchedL, touchedR := touchedPaths(actions)
		base := buildBase(dropUnstable(localPre, localPre, touchedL), dropUnstable(remotePre, remotePre, touchedR))
		if held := holdBackUnconfirmed(base, oldBase, actions, localPre, localPre, remotePre, remotePre); held != 1 {
			t.Errorf("%s: held back %d actions, want 1 (so the pass reports pending work)", side, held)
		}
		next := Plan(base, localPre, remotePre, false)
		if len(next) != 1 || next[0].Op != wantOp || next[0].Path != "d/f" {
			t.Fatalf("%s: the retry plans %+v, want one %s d/f", side, next, wantOp)
		}

		// Control: a delete that worked leaves no base entry and nothing to do.
		base = buildBase(gone, gone)
		if held := holdBackUnconfirmed(base, oldBase, actions, localPre, gone, remotePre, gone); held != 0 {
			t.Errorf("%s: a successful delete was held back", side)
		}
		if _, ok := base["d/f"]; ok {
			t.Errorf("%s: a successful delete kept its base entry", side)
		}
	}
}

// A pass that never got to plan (the entry is locked, or a scan failed) must
// not clear a retry an earlier pass scheduled.
func TestSkippedPassStaysPending(t *testing.T) {
	setupStateDir(t)
	s := manifest.Sync{Name: "busy", Local: t.TempDir(), Remote: "local:" + t.TempDir()}
	release, err := AcquireSyncLock(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	seen, _, pending, err := syncPass(s, false, true)
	release()
	if seen != nil || err != nil {
		t.Fatalf("setup: the locked pass ran (seen=%v err=%v)", seen != nil, err)
	}
	if !pending {
		t.Error("a skipped pass reported no pending work")
	}
	if _, _, pending, err := syncPass(s, false, true); err != nil || pending {
		t.Errorf("unlocked pass: pending=%v err=%v, want a clean pass", pending, err)
	}
}
