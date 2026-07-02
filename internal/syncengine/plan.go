package syncengine

import (
	"sort"
	"strings"
)

// Op is the type of a sync action.
type Op string

const (
	OpPreDeleteLocal  Op = "preDeleteLocal"  // recursive removal of a path about to be replaced by the opposite type
	OpPreDeleteRemote Op = "preDeleteRemote" // recursive removal of a path about to be replaced by the opposite type
	OpPush            Op = "push"
	OpPull            Op = "pull"
	OpDeleteLocal     Op = "deleteLocal"
	OpDeleteRemote    Op = "deleteRemote"
	OpMkdirLocal      Op = "mkdirLocal"
	OpMkdirRemote     Op = "mkdirRemote"
	OpChmodLocal      Op = "chmodLocal"  // apply permission change to local path
	OpChmodRemote     Op = "chmodRemote" // apply permission change to remote path
)

// Action is one unit of planned work.
type Action struct {
	Op       Op
	Path     string
	Reason   string
	Conflict bool   // true if this action resolved a conflict
	Mode     uint32 // target permission bits for OpChmodLocal / OpChmodRemote (e.g. 0o755)
}

// Plan is a pure function that computes the actions needed to synchronise local
// and remote given the base (last-known-good mutual snapshot).
//
// When useHash is true, change detection for regular files uses hash inequality
// instead of size/mtime comparisons (dirs always use size/mtime).  A non-empty
// Hash field in BaseEntry and Entry is required for hash-based detection to
// engage; if either hash is absent the comparison falls back to size/mtime.
//
// Three-way diff rules (per path, union of all three maps):
//   - new on local only → push / mkdirRemote
//   - new on remote only → pull / mkdirLocal
//   - both new (conflict) → newer mtime wins; tie → local
//   - both new, useHash, same hash, neither is dir → contents identical → no action
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
func Plan(base map[string]BaseEntry, local, remote map[string]Entry, useHash bool) []Action {
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

		// Determine per-entry modification using hash when available, falling
		// back to size/mtime for dirs or when hash data is absent.
		localModified := hasLocal && hasBase && fileModified(l, b.LSize, b.LMtime, b.Hash, useHash && !l.IsDir)
		remoteModified := hasRemote && hasBase && fileModified(r, b.RSize, b.RMtime, b.Hash, useHash && !r.IsDir)

		// Type-conflict branch: same path exists on both sides but as different types.
		// This takes precedence over all other switch cases.
		if hasLocal && hasRemote && l.IsDir != r.IsDir {
			localWins := false
			if hasBase {
				// The side whose IsDir DIFFERS from base.Dir flipped the type → it wins.
				localFlipped := b.Dir != l.IsDir
				remoteFlipped := b.Dir != r.IsDir
				switch {
				case localFlipped && !remoteFlipped:
					localWins = true
				case !localFlipped && remoteFlipped:
					localWins = false
				default:
					// Both flipped or neither flipped (shouldn't happen with two different
					// types, but fall through to mtime rule).
					localWins = l.Mtime >= r.Mtime
				}
			} else {
				// No base: newer mtime wins; tie → local.
				localWins = l.Mtime >= r.Mtime
			}

			typeStr := func(isDir bool) string {
				if isDir {
					return "dir"
				}
				return "file"
			}

			if localWins {
				// Pre-delete the remote loser, then propagate local winner.
				actions = append(actions, Action{
					Op:       OpPreDeleteRemote,
					Path:     p,
					Conflict: true,
					Reason:   "type change: local " + typeStr(l.IsDir) + " replaces remote " + typeStr(r.IsDir),
				})
				if l.IsDir {
					actions = append(actions, Action{Op: OpMkdirRemote, Path: p, Reason: "type change: local dir"})
				} else {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "type change: local file"})
				}
			} else {
				// Pre-delete the local loser, then propagate remote winner.
				actions = append(actions, Action{
					Op:       OpPreDeleteLocal,
					Path:     p,
					Conflict: true,
					Reason:   "type change: remote " + typeStr(r.IsDir) + " replaces local " + typeStr(l.IsDir),
				})
				if r.IsDir {
					actions = append(actions, Action{Op: OpMkdirLocal, Path: p, Reason: "type change: remote dir"})
				} else {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "type change: remote file"})
				}
			}
			continue
		}

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
			// Hash-based shortcut: if both sides carry a hash and they agree,
			// the content is identical — treat as already in sync, no action.
			if useHash && !l.IsDir && !r.IsDir && l.Hash != "" && r.Hash != "" && l.Hash == r.Hash {
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
			contentAction := false
			switch {
			case !localModified && !remoteModified:
				// In sync — nothing to do for content.

			case localModified && !remoteModified:
				// Only local changed.
				if !l.IsDir {
					actions = append(actions, Action{Op: OpPush, Path: p, Reason: "modified locally"})
					contentAction = true
				}
				// If it's a dir and it "changed" (mtime bump), no content action needed.

			case !localModified && remoteModified:
				// Only remote changed.
				if !r.IsDir {
					actions = append(actions, Action{Op: OpPull, Path: p, Reason: "modified remotely"})
					contentAction = true
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
				contentAction = true
			}

			// Permission-change detection: only when content is NOT also being
			// transferred (the push/pull already carries mode) and the base has
			// known modes (LMode|RMode != 0 — zero means "written by older caravan").
			if !contentAction && b.LMode|b.RMode != 0 {
				lPerm := l.Mode & 0o777
				rPerm := r.Mode & 0o777
				lBasePerm := b.LMode & 0o777
				rBasePerm := b.RMode & 0o777

				lChanged := lPerm != lBasePerm
				rChanged := rPerm != rBasePerm

				switch {
				case lChanged && !rChanged:
					// Local perms changed, remote unchanged → push perm to remote.
					actions = append(actions, Action{
						Op:     OpChmodRemote,
						Path:   p,
						Reason: "local permission changed",
						Mode:   lPerm,
					})
				case !lChanged && rChanged:
					// Remote perms changed, local unchanged → apply to local.
					actions = append(actions, Action{
						Op:     OpChmodLocal,
						Path:   p,
						Reason: "remote permission changed",
						Mode:   rPerm,
					})
				case lChanged && rChanged:
					// Both changed — newer mtime wins; tie → local wins.
					// Mode conflicts are not content loss; Conflict flag NOT set.
					if l.Mtime >= r.Mtime {
						actions = append(actions, Action{
							Op:     OpChmodRemote,
							Path:   p,
							Reason: "permission conflict: local newer or tie",
							Mode:   lPerm,
						})
					} else {
						actions = append(actions, Action{
							Op:     OpChmodLocal,
							Path:   p,
							Reason: "permission conflict: remote newer",
							Mode:   rPerm,
						})
					}
				}
			}
		}
	}

	// Child-action suppression: after the main loop, remove actions that are
	// strictly under a pre-deleted path and flow TOWARD the losing side.
	//
	// For OpPreDeleteRemote on P where local side is a FILE (loser remote was dir):
	//   Drop all actions under P/ (pulls, mkdirLocal, deleteRemote) because the
	//   recursive pre-delete covers them and pulls would resurrect loser data.
	// For OpPreDeleteRemote on P where local side is a DIR (loser remote was file):
	//   Drop nothing — children pushes to remote must survive.
	// Mirror for OpPreDeleteLocal.
	type preDeleteInfo struct {
		op    Op   // OpPreDeleteLocal or OpPreDeleteRemote
		isDir bool // IsDir of the WINNER (local or remote, depending on op)
	}
	var preDeletes []struct {
		prefix string
		info   preDeleteInfo
	}
	for _, a := range actions {
		if a.Op == OpPreDeleteRemote {
			// winner is local
			winnerIsDir := local[a.Path].IsDir
			preDeletes = append(preDeletes, struct {
				prefix string
				info   preDeleteInfo
			}{a.Path, preDeleteInfo{OpPreDeleteRemote, winnerIsDir}})
		} else if a.Op == OpPreDeleteLocal {
			// winner is remote
			winnerIsDir := remote[a.Path].IsDir
			preDeletes = append(preDeletes, struct {
				prefix string
				info   preDeleteInfo
			}{a.Path, preDeleteInfo{OpPreDeleteLocal, winnerIsDir}})
		}
	}

	if len(preDeletes) > 0 {
		filtered := actions[:0]
		for _, a := range actions {
			suppress := false
			for _, pd := range preDeletes {
				childPrefix := pd.prefix + "/"
				if !strings.HasPrefix(a.Path, childPrefix) {
					continue
				}
				// This action is strictly under the pre-deleted path.
				if pd.info.op == OpPreDeleteRemote {
					// Local won. If winner is a FILE, drop all children actions
					// (no children can exist on file winner side, and remote dir
					// children must not be pulled/mkdirLocal'd).
					// If winner is a DIR, children pushes survive — drop only
					// actions that go toward the losing remote side (pulls, mkdirLocal).
					if !pd.info.isDir {
						// Local side is a file — no children expected; suppress anything
						// that would touch either side under this prefix.
						suppress = true
					} else {
						// Local side is a dir — suppress only actions pulling from loser remote.
						switch a.Op {
						case OpPull, OpMkdirLocal, OpDeleteRemote:
							suppress = true
						}
					}
				} else { // OpPreDeleteLocal
					// Remote won. Mirror logic.
					if !pd.info.isDir {
						// Remote side is a file — suppress anything under prefix.
						suppress = true
					} else {
						// Remote side is a dir — suppress only actions pushing from loser local.
						switch a.Op {
						case OpPush, OpMkdirRemote, OpDeleteLocal:
							suppress = true
						}
					}
				}
				if suppress {
					break
				}
			}
			if !suppress {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}

	return sortActions(actions)
}

// fileModified reports whether e differs from the recorded base (bSize, bMtime, bHash).
// When useHash is true and both e.Hash and bHash are non-empty, hash inequality is
// used as the change signal.  Otherwise size/mtime comparison is used.
func fileModified(e Entry, bSize, bMtime int64, bHash string, useHash bool) bool {
	if useHash && e.Hash != "" && bHash != "" {
		return e.Hash != bHash
	}
	return e.Size != bSize || e.Mtime != bMtime
}

// sortActions orders actions so they can be applied safely:
//  0. preDeleteLocal / preDeleteRemote — deepest first (must run before mkdirs/pushes)
//  1. mkdirLocal / mkdirRemote — shallowest first
//  2. push / pull
//  3. deleteLocal / deleteRemote — deepest first (so child files go before parent dirs)
//  4. chmodLocal / chmodRemote — path order within (run last, after content is settled)
func sortActions(actions []Action) []Action {
	priority := func(op Op) int {
		switch op {
		case OpPreDeleteLocal, OpPreDeleteRemote:
			return 0
		case OpMkdirLocal, OpMkdirRemote:
			return 1
		case OpPush, OpPull:
			return 2
		case OpDeleteLocal, OpDeleteRemote:
			return 3
		default: // chmodLocal, chmodRemote
			return 4
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
			// preDeletes: deepest first
			if di != dj {
				return di > dj
			}
		} else if pi == 1 {
			// mkdirs: shallow first
			if di != dj {
				return di < dj
			}
		} else if pi == 3 {
			// deletes: deepest first
			if di != dj {
				return di > dj
			}
		}
		return actions[i].Path < actions[j].Path
	})

	return actions
}
