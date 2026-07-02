package syncengine

import (
	"sort"
	"testing"
)

// --- helpers ---

func fe(size int64, mtime int64) Entry {
	return Entry{Size: size, Mtime: mtime, Mode: 0o644}
}

func de(mtime int64) Entry {
	return Entry{Mtime: mtime, Mode: 0o755, IsDir: true}
}

func be(ls, lm, rs, rm int64, dir bool) BaseEntry {
	return BaseEntry{LSize: ls, LMtime: lm, RSize: rs, RMtime: rm, Dir: dir}
}

// opsFor extracts (Op,Path) pairs from actions, sorted for stable comparison.
type opPath struct {
	Op   Op
	Path string
}

func collectOps(actions []Action) []opPath {
	var out []opPath
	for _, a := range actions {
		out = append(out, opPath{a.Op, a.Path})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func wantOps(pairs ...interface{}) []opPath {
	if len(pairs)%2 != 0 {
		panic("wantOps: odd number of args")
	}
	var out []opPath
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, opPath{pairs[i].(Op), pairs[i+1].(string)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func checkActions(t *testing.T, got []Action, want []opPath) {
	t.Helper()
	gotOps := collectOps(got)
	if len(gotOps) != len(want) {
		t.Errorf("action count: got %d want %d\ngot:  %v\nwant: %v", len(gotOps), len(want), gotOps, want)
		return
	}
	for i := range gotOps {
		if gotOps[i] != want[i] {
			t.Errorf("action[%d]: got %v want %v", i, gotOps[i], want[i])
		}
	}
}

// --- table tests ---

func TestPlanFreshBothEmpty(t *testing.T) {
	actions := Plan(
		map[string]BaseEntry{},
		map[string]Entry{},
		map[string]Entry{},
	)
	if len(actions) != 0 {
		t.Errorf("expected no actions, got %v", actions)
	}
}

func TestPlanFreshPushAll(t *testing.T) {
	// Local has files, remote is empty, no base → push all.
	actions := Plan(
		map[string]BaseEntry{},
		map[string]Entry{
			"a.txt": fe(100, 1000),
			"b.txt": fe(200, 2000),
		},
		map[string]Entry{},
	)
	checkActions(t, actions, wantOps(
		OpPush, "a.txt",
		OpPush, "b.txt",
	))
}

func TestPlanFreshPullAll(t *testing.T) {
	// Remote has files, local empty, no base → pull all.
	actions := Plan(
		map[string]BaseEntry{},
		map[string]Entry{},
		map[string]Entry{
			"x.txt": fe(50, 500),
		},
	)
	checkActions(t, actions, wantOps(OpPull, "x.txt"))
}

func TestPlanModifyLocal(t *testing.T) {
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{"f.txt": fe(110, 2000)}  // changed
	remote := map[string]Entry{"f.txt": fe(100, 1000)} // unchanged
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPush, "f.txt"))
}

func TestPlanModifyRemote(t *testing.T) {
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{"f.txt": fe(100, 1000)}  // unchanged
	remote := map[string]Entry{"f.txt": fe(110, 2000)} // changed
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPull, "f.txt"))
}

func TestPlanConflictNewerWins(t *testing.T) {
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	// Remote is newer.
	local := map[string]Entry{"f.txt": fe(110, 2000)}
	remote := map[string]Entry{"f.txt": fe(120, 3000)}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPull, "f.txt")) // remote newer
}

func TestPlanConflictTieLocalWins(t *testing.T) {
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	// Same mtime → local wins.
	local := map[string]Entry{"f.txt": fe(110, 5000)}
	remote := map[string]Entry{"f.txt": fe(120, 5000)}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPush, "f.txt"))
}

func TestPlanDeleteLocalPropagates(t *testing.T) {
	// f.txt was synced; local deleted it; remote unchanged → delete remote.
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{}
	remote := map[string]Entry{"f.txt": fe(100, 1000)}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpDeleteRemote, "f.txt"))
}

func TestPlanDeleteRemotePropagates(t *testing.T) {
	// f.txt was synced; remote deleted it; local unchanged → delete local.
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{"f.txt": fe(100, 1000)}
	remote := map[string]Entry{}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpDeleteLocal, "f.txt"))
}

func TestPlanDeleteVsModify_DeleteLocalModifyRemote(t *testing.T) {
	// Local deleted, remote modified → modification wins → pull.
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{}
	remote := map[string]Entry{"f.txt": fe(110, 2000)} // modified
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPull, "f.txt"))
}

func TestPlanDeleteVsModify_DeleteRemoteModifyLocal(t *testing.T) {
	// Remote deleted, local modified → modification wins → push.
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
	}
	local := map[string]Entry{"f.txt": fe(110, 2000)} // modified
	remote := map[string]Entry{}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPush, "f.txt"))
}

func TestPlanNewSamePathBothSides_RemoteNewer(t *testing.T) {
	// Both sides added same path independently (no base) → conflict, remote newer.
	base := map[string]BaseEntry{}
	local := map[string]Entry{"f.txt": fe(100, 1000)}
	remote := map[string]Entry{"f.txt": fe(200, 2000)}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPull, "f.txt")) // remote newer
}

func TestPlanNewSamePathBothSides_LocalNewer(t *testing.T) {
	base := map[string]BaseEntry{}
	local := map[string]Entry{"f.txt": fe(100, 3000)}
	remote := map[string]Entry{"f.txt": fe(200, 2000)}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpPush, "f.txt")) // local newer
}

func TestPlanEmptyDirs(t *testing.T) {
	// Local has an empty dir that remote doesn't → mkdirRemote.
	base := map[string]BaseEntry{}
	local := map[string]Entry{"emptydir": de(1000)}
	remote := map[string]Entry{}
	actions := Plan(base, local, remote)
	checkActions(t, actions, wantOps(OpMkdirRemote, "emptydir"))
}

func TestPlanNestedDirs(t *testing.T) {
	// Local has a/b/c/ structure; remote is empty.
	base := map[string]BaseEntry{}
	local := map[string]Entry{
		"a":       de(1),
		"a/b":     de(2),
		"a/b/c":   de(3),
		"a/b/f.txt": fe(10, 100),
	}
	remote := map[string]Entry{}
	actions := Plan(base, local, remote)

	// Expect mkdirRemote for all dirs (shallow before deep) then push for file.
	if len(actions) != 4 {
		t.Fatalf("expected 4 actions, got %d: %v", len(actions), actions)
	}
	// First three actions are mkdirs, last is push.
	for i := 0; i < 3; i++ {
		if actions[i].Op != OpMkdirRemote {
			t.Errorf("action[%d] should be mkdirRemote, got %s", i, actions[i].Op)
		}
	}
	if actions[3].Op != OpPush {
		t.Errorf("action[3] should be push, got %s", actions[3].Op)
	}
	// Mkdir order: depth 0, 1, 2
	depths := []int{0, 1, 2}
	for i, d := range depths {
		got := countSlash(actions[i].Path)
		if got != d {
			t.Errorf("mkdir action[%d] depth: got %d want %d (path=%s)", i, got, d, actions[i].Path)
		}
	}
}

func TestPlanDeleteDirsDeepestFirst(t *testing.T) {
	// base has nested structure; both deleted locally → deleteRemote deepest first.
	base := map[string]BaseEntry{
		"a":       be(0, 1, 0, 1, true),
		"a/b":     be(0, 2, 0, 2, true),
		"a/b/f":   be(10, 3, 10, 3, false),
	}
	local := map[string]Entry{}
	remote := map[string]Entry{
		"a":     de(1),
		"a/b":   de(2),
		"a/b/f": fe(10, 3),
	}
	actions := Plan(base, local, remote)
	// Expect: deleteRemote for a/b/f, a/b, a (deepest first for dirs)
	// File delete comes before dir delete.
	fileIdx := -1
	for i, a := range actions {
		if a.Path == "a/b/f" && a.Op == OpDeleteRemote {
			fileIdx = i
		}
	}
	if fileIdx < 0 {
		t.Fatal("expected deleteRemote for a/b/f")
	}
	// Find a/b delete and a delete and ensure a/b comes before a (deepest first).
	abIdx, aIdx := -1, -1
	for i, a := range actions {
		if a.Op == OpDeleteRemote {
			switch a.Path {
			case "a/b":
				abIdx = i
			case "a":
				aIdx = i
			}
		}
	}
	if abIdx < 0 || aIdx < 0 {
		t.Fatalf("missing dir deletes: abIdx=%d aIdx=%d actions=%v", abIdx, aIdx, actions)
	}
	if abIdx > aIdx {
		t.Errorf("expected a/b deleted before a (deepest first), got a/b at %d, a at %d", abIdx, aIdx)
	}
	// File delete should come before dir deletes.
	if fileIdx > abIdx {
		t.Errorf("expected file deleted before dirs, fileIdx=%d abIdx=%d", fileIdx, abIdx)
	}
}

func TestPlanInSync(t *testing.T) {
	// No changes since last sync → no actions.
	base := map[string]BaseEntry{
		"f.txt": be(100, 1000, 100, 1000, false),
		"g.txt": be(200, 2000, 200, 2000, false),
	}
	local := map[string]Entry{
		"f.txt": fe(100, 1000),
		"g.txt": fe(200, 2000),
	}
	remote := map[string]Entry{
		"f.txt": fe(100, 1000),
		"g.txt": fe(200, 2000),
	}
	actions := Plan(base, local, remote)
	if len(actions) != 0 {
		t.Errorf("expected no actions when in sync, got %v", actions)
	}
}

func TestPlanOrderMkdirBeforePushBeforeDelete(t *testing.T) {
	// Mix of ops; verify global order.
	base := map[string]BaseEntry{
		"old.txt": be(10, 100, 10, 100, false),
	}
	local := map[string]Entry{
		"newdir":       de(999),
		"newdir/n.txt": fe(5, 500),
	}
	remote := map[string]Entry{
		"old.txt": fe(10, 100),
	}
	actions := Plan(base, local, remote)
	// Expect: mkdirRemote(newdir), push(newdir/n.txt), deleteRemote(old.txt)
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d: %v", len(actions), actions)
	}
	if actions[0].Op != OpMkdirRemote {
		t.Errorf("first action should be mkdirRemote, got %s", actions[0].Op)
	}
	if actions[1].Op != OpPush {
		t.Errorf("second action should be push, got %s", actions[1].Op)
	}
	if actions[2].Op != OpDeleteRemote {
		t.Errorf("third action should be deleteRemote, got %s", actions[2].Op)
	}
}

func countSlash(s string) int {
	n := 0
	for _, c := range s {
		if c == '/' {
			n++
		}
	}
	return n
}
