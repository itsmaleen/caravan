package syncengine

import "testing"

// An editor saving every few seconds (Freewrite, build_hub.py, a growing log)
// can save again between a pass's local scan and tar reading the file. tar
// then pushes the newer bytes. Before the fix the base paired the stale
// local scan with the newer remote copy, the size guard dropped the path,
// and the next pass took the spoke's own push for a remote edit: one
// conflict backup on the hub per save (134 copies of one note on the mini).
func TestSaveBetweenScanAndPushIsNotAConflict(t *testing.T) {
	v1 := Entry{Size: 29, Mtime: 1_000_000_000_100, Mode: 0o644}
	v2 := Entry{Size: 53, Mtime: 1_000_000_005_200, Mode: 0o644} // saved mid-pass; tar sent this
	v3 := Entry{Size: 77, Mtime: 1_000_000_010_300, Mode: 0o644} // the next save

	localPre := map[string]Entry{"note.md": v1}
	localPost := map[string]Entry{"note.md": v2}
	remotePre := map[string]Entry{"note.md": {Size: 4, Mtime: 1, Mode: 0o644}}
	remotePost := map[string]Entry{"note.md": v2}
	actions := []Action{{Op: OpPush, Path: "note.md"}}

	localForBase := dropUnstable(localPre, localPost, touchSet{})
	settleTransfers(actions, localForBase, localPost, remotePre, remotePost, nil, false)
	base := buildBase(localForBase, remotePost)
	if _, ok := base["note.md"]; !ok {
		t.Fatal("pushed file left the base: the next pass will call it a conflict")
	}
	if n := holdBackUnconfirmed(base, nil, actions, localPre, localPost, remotePre, remotePost); n != 0 {
		t.Fatalf("held back %d transfers, want 0", n)
	}

	next := Plan(base, map[string]Entry{"note.md": v3}, remotePost, false)
	if len(next) != 1 || next[0].Op != OpPush || next[0].Conflict {
		t.Fatalf("next pass plans %+v, want one plain push", next)
	}
}

// If the far side really changed after the push (matches neither version of
// the source), settleTransfers must leave it visible so it is still a conflict.
func TestSettleTransfersKeepsForeignRemoteEdit(t *testing.T) {
	v1 := Entry{Size: 29, Mtime: 1_000_000_000_100}
	v2 := Entry{Size: 53, Mtime: 1_000_000_005_200}
	foreign := Entry{Size: 53, Mtime: 1_000_000_009_900}
	localForBase := map[string]Entry{"note.md": v1}
	settleTransfers([]Action{{Op: OpPush, Path: "note.md"}}, localForBase,
		map[string]Entry{"note.md": v2}, nil, map[string]Entry{"note.md": foreign}, nil, false)
	if localForBase["note.md"] != v1 {
		t.Fatalf("local base entry = %+v, want the planned %+v", localForBase["note.md"], v1)
	}
}

// Same file on both sides with no base (base lost, new spoke, seeded peer):
// no conflict and no backup. On 2026-11-06 this case wrote 15,613 identical
// copies (5.7 GB) to the hub.
func TestIdenticalCopiesAreNotConflicts(t *testing.T) {
	f := Entry{Size: 1655, Mtime: 1_795_445_564_344_898_968, Mode: 0o644}
	if a := Plan(nil, map[string]Entry{"n.md": f}, map[string]Entry{"n.md": f}, false); len(a) != 0 {
		t.Fatalf("no base, identical: plans %+v, want nothing", a)
	}
	old := BaseEntry{LSize: 10, LMtime: 5, RSize: 10, RMtime: 5, LMode: 0o644, RMode: 0o644}
	if a := Plan(map[string]BaseEntry{"n.md": old}, map[string]Entry{"n.md": f}, map[string]Entry{"n.md": f}, false); len(a) != 0 {
		t.Fatalf("both modified to the same file: plans %+v, want nothing", a)
	}
	// Whole-second mtimes are too coarse to prove a copy: still a conflict.
	w := Entry{Size: 10, Mtime: 1_795_445_564_000_000_000}
	if a := Plan(nil, map[string]Entry{"n.md": w}, map[string]Entry{"n.md": w}, false); len(a) != 1 || !a[0].Conflict {
		t.Fatalf("whole-second mtimes: plans %+v, want a conflict", a)
	}
}

// Saved twice during the pass: the scan saw v1, tar sent v2, the re-read saw
// v3. The remote holds one of our own saves, so the next pass is a plain push.
func TestDoubleSaveDuringPushIsNotAConflict(t *testing.T) {
	v1 := Entry{Size: 29, Mtime: 1_000_000_000_100, Mode: 0o644}
	v2 := Entry{Size: 53, Mtime: 1_000_300_000_200, Mode: 0o644}
	v3 := Entry{Size: 77, Mtime: 1_000_600_000_300, Mode: 0o644}
	localPre := map[string]Entry{"note.md": v1}
	localPost := map[string]Entry{"note.md": v3}
	remotePre := map[string]Entry{"note.md": {Size: 4, Mtime: 1, Mode: 0o644}}
	remotePost := map[string]Entry{"note.md": v2}
	actions := []Action{{Op: OpPush, Path: "note.md"}}

	localForBase := dropUnstable(localPre, localPost, touchSet{})
	settleTransfers(actions, localForBase, localPost, remotePre, remotePost, nil, false)
	base := buildBase(localForBase, remotePost)
	if n := holdBackUnconfirmed(base, nil, actions, localPre, localPost, remotePre, remotePost); n != 0 {
		t.Fatalf("held back %d transfers, want 0", n)
	}
	next := Plan(base, localPost, remotePost, false)
	if len(next) != 1 || next[0].Op != OpPush || next[0].Conflict {
		t.Fatalf("next pass plans %+v, want one plain push", next)
	}
}
