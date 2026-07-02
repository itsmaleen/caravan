package syncengine

import (
	"sort"
	"strings"
)

// Op is the type of a sync action.
type Op string

const (
	OpPush         Op = "push"
	OpPull         Op = "pull"
	OpDeleteLocal  Op = "deleteLocal"
	OpDeleteRemote Op = "deleteRemote"
	OpMkdirLocal   Op = "mkdirLocal"
	OpMkdirRemote  Op = "mkdirRemote"
)

// Action is one unit of planned work.
type Action struct {
	Op       Op
	Path     string
	Reason   string
	Conflict bool // true if this action resolved a conflict
}

// Plan is a pure function that computes the actions needed to synchronise local
// and remote given the base (last-known-good mutual snapshot).
//
// Three-way diff rules (per path, union of all three maps):
//   - new on local only → push / mkdirRemote
//   - new on remote only → pull / mkdirLocal
//   - both new (conflict) → newer mtime wins; tie → local
//   - in base, local deleted, remote unchanged → deleteRemote
//   - in base, remote deleted, local unchanged → deleteLocal
//   - in base, local deleted, remote modified → pull (modification wins)
//   - in base, remote deleted, local modified → push (modification wins)
//   - in base, only local changed → push
//   - in base, only remote changed → pull
//   - in base, both changed (conflict) → newer mtime wins; tie → local
//   - in base, neither changed → in sync, no action
//
// Actions are returned sorted: mkdirs (shallow first), then push/pull,
// then deletes (deepest first).
func Plan(base map[string]BaseEntry, local, remote map[string]Entry) []Action {
	// Collect the union of all known paths.
	paths := make(map[string]struct{}, len(local)+len(remote)+len(base))
	for p := range base {
		paths[p] = struct{}{}
	}
	for p := range local {
		paths[p] = struct{}{}
	}
	for p := range remote {
		paths[p] = struct{}{}
	}

	var actions []Action

	for p := range paths {
		b, hasBase := base[p]
		l, hasLocal := local[p]
		r, hasRemote := remote[p]

		localModified := hasLocal && hasBase && (l.Size != b.LSize || l.Mtime != b.LMtime)
		remoteModified := hasRemote && hasBase && (r.Size != b.RSize || r.Mtime != b.RMtime)

		switch {
		case !hasLocal && !hasRemote:
			// Both absent (deleted on both sides, or already gone). Nothing to do.

		case hasLocal && !hasRemote && !hasBase:
			// New on local only.
			if l.IsDir {
				actions = append(actions, Action{Op: OpMkdirRemote, Path: p, Reason: "new local dir"})
			} else {
				actions = append(actions, Action{Op: OpPush, Path: p, Reason: "new local"})
			}

		case !hasLocal && hasRemote && !hasBase:
			// New on remote only.
			if r.IsDir {
				actions = append(actions, Action{Op: OpMkdirLocal, Path: p, Reason: "new remote dir"})
			} else {
				actions = append(actions, Action{Op: OpPull, Path: p, Reason: "new remote"})
			}

		case hasLocal && hasRemote && !hasBase:
			// Both new (same path added on both sides simultaneously) — conflict.
			if l.IsDir && r.IsDir {
				// Dir exists on both sides; no content conflict.
				break
			}
			if l.Mtime >= r.Mtime {
				// Local wins (newer or tie).
				if l.IsDir {
					actions = append(actions, Action{Op: OpMkdirRemote, Path: p, Reason: "conflict: local dir wins", Conflict: true})
				} else {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "conflict: local newer or tie", Conflict: true})
				}
			} else {
				// Remote wins.
				if r.IsDir {
					actions = append(actions, Action{Op: OpMkdirLocal, Path: p, Reason: "conflict: remote dir wins", Conflict: true})
				} else {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "conflict: remote newer", Conflict: true})
				}
			}

		case !hasLocal && !hasRemote && hasBase:
			// Both deleted since last sync — nothing to do.

		case !hasLocal && hasRemote && hasBase:
			// Local was deleted since last sync.
			if remoteModified {
				// Remote was also modified — modification wins over deletion.
				if r.IsDir {
					actions = append(actions, Action{Op: OpMkdirLocal, Path: p, Reason: "modified remotely; local deleted"})
				} else {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "modified remotely; local deleted"})
				}
			} else {
				// Remote unchanged — propagate local deletion to remote.
				if r.IsDir {
					actions = append(actions, Action{Op: OpDeleteRemote, Path: p, Reason: "deleted locally (dir)"})
				} else {
					actions = append(actions, Action{Op: OpDeleteRemote, Path: p, Reason: "deleted locally"})
				}
			}

		case hasLocal && !hasRemote && hasBase:
			// Remote was deleted since last sync.
			if localModified {
				// Local was also modified — modification wins over deletion.
				if l.IsDir {
					actions = append(actions, Action{Op: OpMkdirRemote, Path: p, Reason: "modified locally; remote deleted"})
				} else {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "modified locally; remote deleted"})
				}
			} else {
				// Local unchanged — propagate remote deletion to local.
				if l.IsDir {
					actions = append(actions, Action{Op: OpDeleteLocal, Path: p, Reason: "deleted remotely (dir)"})
				} else {
					actions = append(actions, Action{Op: OpDeleteLocal, Path: p, Reason: "deleted remotely"})
				}
			}

		case hasLocal && hasRemote && hasBase:
			// Both present and in base.
			switch {
			case !localModified && !remoteModified:
				// In sync — nothing to do.

			case localModified && !remoteModified:
				// Only local changed.
				if !l.IsDir {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "modified locally"})
				}
				// If it's a dir and it "changed" (mtime bump), no content action needed.

			case !localModified && remoteModified:
				// Only remote changed.
				if !r.IsDir {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "modified remotely"})
				}

			default:
				// Both changed — conflict.
				if l.IsDir && r.IsDir {
					break // Dir-vs-dir, no file content conflict.
				}
				if l.Mtime >= r.Mtime {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "conflict: local newer or tie", Conflict: true})
				} else {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "conflict: remote newer", Conflict: true})
				}
			}
		}
	}

	return sortActions(actions)
}

// sortActions orders actions so they can be applied safely:
//  1. mkdirLocal / mkdirRemote — shallowest first
//  2. push / pull
//  3. deleteLocal / deleteRemote — deepest first (so child files go before parent dirs)
func sortActions(actions []Action) []Action {
	priority := func(op Op) int {
		switch op {
		case OpMkdirLocal, OpMkdirRemote:
			return 0
		case OpPush, OpPull:
			return 1
		default: // deleteLocal, deleteRemote
			return 2
		}
	}

	sort.SliceStable(actions, func(i, j int) bool {
		pi := priority(actions[i].Op)
		pj := priority(actions[j].Op)
		if pi != pj {
			return pi < pj
		}
		di := strings.Count(actions[i].Path, "/")
		dj := strings.Count(actions[j].Path, "/")
		if pi == 0 {
			// mkdirs: shallow first
			if di != dj {
				return di < dj
			}
		} else if pi == 2 {
			// deletes: deepest first
			if di != dj {
				return di > dj
			}
		}
		return actions[i].Path < actions[j].Path
	})

	return actions
}
