package syncengine

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// SyncStats summarises what happened during one sync run.
type SyncStats struct {
	Pushed          int
	Pulled          int
	DeletedL        int // local deletions
	DeletedR        int // remote deletions
	Conflicts       int
	Errors          int
	ConflictBackups int // number of successful conflict-loser backups
}

// CmdSync implements `caravan sync [NAME] [--watch] [--interval 2s] [--dry-run] [--bootstrap] [-f manifest]`.
func CmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestPath := fs.String("f", "", "manifest path")
	dryRun := fs.Bool("dry-run", false, "print plan without applying")
	watch := fs.Bool("watch", false, "loop continuously")
	intervalStr := fs.String("interval", "2s", "watch interval (e.g. 2s)")
	_ = fs.Bool("bootstrap", false, "bootstrap remote binary (always attempted automatically)")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		return 2
	}

	interval, err := time.ParseDuration(*intervalStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: invalid --interval %q: %v\n", *intervalStr, err)
		return 2
	}

	mpath := manifest.ResolvePath(*manifestPath)
	m, err := manifest.Load(mpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		return 1
	}

	if len(m.Sync) == 0 {
		fmt.Fprintln(os.Stderr, "sync: no [[sync]] entries in manifest")
		return 1
	}

	// Determine which sync entries to process.
	var entries []manifest.Sync
	if len(positionals) == 0 {
		entries = m.Sync
	} else {
		name := positionals[0]
		for _, s := range m.Sync {
			if s.Name == name {
				entries = append(entries, s)
				break
			}
		}
		if len(entries) == 0 {
			fmt.Fprintf(os.Stderr, "sync: no sync entry named %q\n", name)
			return 1
		}
	}

	if !*watch {
		code := 0
		for _, s := range entries {
			if err := runSyncEntry(s, *dryRun, false); err != nil {
				fmt.Fprintf(os.Stderr, "sync %s: %v\n", s.Name, err)
				code = 1
			}
		}
		return code
	}

	// Watch mode.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	names := make([]string, len(entries))
	for i, s := range entries {
		names[i] = s.Name
	}
	fmt.Printf("watching %s (interval %s)\n", strings.Join(names, ", "), interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			return 0
		case <-ticker.C:
			for _, s := range entries {
				if err := runSyncEntry(s, *dryRun, true); err != nil {
					fmt.Fprintf(os.Stderr, "sync %s: %v\n", s.Name, err)
				}
			}
		}
	}
}

// runSyncEntry runs one sync pass for a single [[sync]] entry. In quiet mode
// (watch loop) an in-sync pass prints nothing.
func runSyncEntry(s manifest.Sync, dryRun, quiet bool) error {
	release, lockErr := AcquireSyncLock(s.Name)
	if lockErr != nil {
		if errors.Is(lockErr, ErrSyncBusy) {
			// Another caravan process (daemon or manual run) owns this entry
			// right now; skipping is the correct behavior, not an error state.
			if !quiet {
				fmt.Printf("- %s skipped: %v\n", s.Name, lockErr)
			}
			return nil
		}
		return lockErr
	}
	defer release()

	localRoot := manifest.ExpandPath(s.Local)
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir local %s: %w", localRoot, err)
	}

	remote, err := ParseRemote(s.Remote)
	if err != nil {
		return err
	}

	excludes := s.Excludes()

	// Scan local.
	localEntries, _, err := ScanDir(localRoot, excludes, s.Checksum)
	if err != nil {
		return fmt.Errorf("local scan: %w", err)
	}

	// Scan remote (creates remote root if needed).
	remoteEntries, err := remote.Scan(excludes, s.Checksum)
	if err != nil {
		return fmt.Errorf("remote scan: %w", err)
	}

	// Load base snapshot.
	state, err := LoadState(s.Name)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Plan.
	actions := Plan(state.Pairs, localEntries, remoteEntries, s.Checksum)

	if len(actions) == 0 {
		if !quiet {
			total := countFiles(localEntries)
			fmt.Printf("✓ %s in sync (%d files)\n", s.Name, total)
		}
		return nil
	}

	// Print plan.
	printPlan(s.Name, actions)

	if dryRun {
		return nil
	}

	// Apply.
	stats, applyErr := applyActions(actions, localRoot, remote, localEntries, remoteEntries, s.Name, s.DeltaThreshold())

	// Rescan both sides to build the new authoritative base.
	newLocal, _, _ := ScanDir(localRoot, excludes, s.Checksum)
	newRemote, _ := remote.Scan(excludes, s.Checksum)
	newBase := buildBase(newLocal, newRemote)

	newState := &State{
		Pairs:    newBase,
		LastSync: time.Now().UnixNano(),
	}
	if err := SaveState(s.Name, newState); err != nil {
		fmt.Fprintf(os.Stderr, "sync: save state: %v\n", err)
	}

	// Summary line.
	fmt.Printf("  pushed %d, pulled %d, deleted %d local/%d remote, conflicts %d",
		stats.Pushed, stats.Pulled, stats.DeletedL, stats.DeletedR, stats.Conflicts)
	if stats.Errors > 0 {
		fmt.Printf(", errors %d", stats.Errors)
	}
	fmt.Println()
	if stats.ConflictBackups > 0 {
		fmt.Printf("  conflict backups in %s/ (%d)\n",
			filepath.Join(resolvedConflictDir(), s.Name), stats.ConflictBackups)
	}

	return applyErr
}

// applyActions executes the plan.
//
// The action slice is already sorted by sortActions:
//
//	0. preDeleteLocal/preDeleteRemote (deepest first) — run inline immediately
//	1. mkdirLocal/mkdirRemote (shallow first) — run inline immediately
//	2. push/pull — batched then executed
//	3. deleteLocal/deleteRemote (deepest first) — batched then executed
//
// Pre-deletes are executed inline as they are encountered so that the
// subsequent mkdir/push/pull operations find a clean slate.
//
// syncName is the [[sync]] entry name, used to compute the conflict-backup dir.
// deltaThreshold is the minimum file size for rsync delta transfer (bytes).
func applyActions(
	actions []Action,
	localRoot string,
	remote *RemoteConn,
	localEntries, remoteEntries map[string]Entry,
	syncName string,
	deltaThreshold int64,
) (SyncStats, error) {
	var stats SyncStats

	// conflictPaths records which paths are conflict losers and the direction
	// (true = local is loser/pull wins, false = remote is loser/push wins).
	// Used to back up the loser before overwriting.
	type conflictLoser struct {
		localLoser bool // true if local side is the loser (pull wins)
	}
	conflictLosers := map[string]conflictLoser{}

	// Prune old backups before we start applying (best-effort, silent).
	pruneConflictBackups(syncName)

	var pushPaths, pullPaths []string
	var delLocalFiles, delRemoteFiles, delLocalDirs, delRemoteDirs []string

	for _, a := range actions {
		if a.Conflict {
			winner := "local"
			switch a.Op {
			case OpPull, OpMkdirLocal, OpPreDeleteLocal:
				winner = "remote"
			}
			fmt.Printf("  conflict: %s (%s wins)\n", a.Path, winner)
			stats.Conflicts++

			// Record which side is the loser for backup purposes.
			switch a.Op {
			case OpPull:
				// Remote wins → local file is the loser.
				conflictLosers[a.Path] = conflictLoser{localLoser: true}
			case OpPush:
				// Local wins → remote file is the loser.
				conflictLosers[a.Path] = conflictLoser{localLoser: false}
			case OpPreDeleteLocal:
				// Remote wins type-flip → local path is the loser.
				conflictLosers[a.Path] = conflictLoser{localLoser: true}
			case OpPreDeleteRemote:
				// Local wins type-flip → remote path is the loser.
				conflictLosers[a.Path] = conflictLoser{localLoser: false}
			}
		}

		switch a.Op {
		case OpPreDeleteLocal:
			// Back up the local loser before removing it (if it's a conflict).
			if a.Conflict {
				if n := backupLocalLoser(syncName, localRoot, a.Path); n > 0 {
					stats.ConflictBackups += n
				}
			}
			// Recursively remove the local path to make room for the remote winner.
			if err := os.RemoveAll(localJoin(localRoot, a.Path)); err != nil {
				fmt.Fprintf(os.Stderr, "pre-delete local %s: %v\n", a.Path, err)
				stats.Errors++
			} else {
				stats.DeletedL++
			}
		case OpPreDeleteRemote:
			// Back up the remote loser before removing it (if it's a conflict).
			if a.Conflict {
				if n := backupRemoteLoser(syncName, remote, a.Path); n > 0 {
					stats.ConflictBackups += n
				}
			}
			// Recursively remove the remote path (DeleteDir handles both files and dirs via rm -rf).
			if err := remote.DeleteDir(a.Path); err != nil {
				fmt.Fprintf(os.Stderr, "pre-delete remote %s: %v\n", a.Path, err)
				stats.Errors++
			} else {
				stats.DeletedR++
			}
		case OpMkdirLocal:
			if err := os.MkdirAll(localJoin(localRoot, a.Path), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "mkdir local %s: %v\n", a.Path, err)
				stats.Errors++
			}
		case OpMkdirRemote:
			if err := remote.MkdirAll(a.Path); err != nil {
				fmt.Fprintf(os.Stderr, "mkdir remote %s: %v\n", a.Path, err)
				stats.Errors++
			}
		case OpPush:
			pushPaths = append(pushPaths, a.Path)
		case OpPull:
			pullPaths = append(pullPaths, a.Path)
		case OpDeleteLocal:
			if e, ok := localEntries[a.Path]; ok && e.IsDir {
				delLocalDirs = append(delLocalDirs, a.Path)
			} else {
				delLocalFiles = append(delLocalFiles, a.Path)
			}
		case OpDeleteRemote:
			if e, ok := remoteEntries[a.Path]; ok && e.IsDir {
				delRemoteDirs = append(delRemoteDirs, a.Path)
			} else {
				delRemoteFiles = append(delRemoteFiles, a.Path)
			}
		}
	}

	// Back up conflict losers in push/pull batches before executing transfers.
	// Pull: local is loser (remote wins) → back up local file before pull overwrites it.
	// Push: remote is loser (local wins) → back up remote file before push overwrites it.
	for _, p := range pullPaths {
		if cl, ok := conflictLosers[p]; ok && cl.localLoser {
			if n := backupLocalLoser(syncName, localRoot, p); n > 0 {
				stats.ConflictBackups += n
			}
		}
	}
	for _, p := range pushPaths {
		if cl, ok := conflictLosers[p]; ok && !cl.localLoser {
			if n := backupRemoteLoser(syncName, remote, p); n > 0 {
				stats.ConflictBackups += n
			}
		}
	}

	// Partition push/pull into small (tar batch) and large (rsync delta) based
	// on the size recorded in the local/remote entry maps.
	smallPush, largePush := partitionBySize(pushPaths, localEntries, deltaThreshold)
	smallPull, largePull := partitionBySize(pullPaths, remoteEntries, deltaThreshold)

	// Push files — large via rsync delta, small via tar batch.
	// rsync delta is only available for SSH transport; local: falls back to tar/copy.
	for _, p := range largePush {
		if err := remote.PushDelta(localRoot, []string{p}); err != nil {
			fmt.Fprintf(os.Stderr, "push delta %s (falling back to tar): %v\n", p, err)
			smallPush = append(smallPush, p) // fall back to tar
		} else {
			stats.Pushed++
		}
	}
	if len(smallPush) > 0 {
		if err := remote.Push(localRoot, smallPush); err != nil {
			fmt.Fprintf(os.Stderr, "push: %v\n", err)
			stats.Errors++
		} else {
			stats.Pushed += len(smallPush)
		}
	}

	// Pull files — large via rsync delta, small via tar batch.
	for _, p := range largePull {
		if err := remote.PullDelta(localRoot, []string{p}); err != nil {
			fmt.Fprintf(os.Stderr, "pull delta %s (falling back to tar): %v\n", p, err)
			smallPull = append(smallPull, p) // fall back to tar
		} else {
			stats.Pulled++
		}
	}
	if len(smallPull) > 0 {
		if err := remote.Pull(localRoot, smallPull); err != nil {
			fmt.Fprintf(os.Stderr, "pull: %v\n", err)
			stats.Errors++
		} else {
			stats.Pulled += len(smallPull)
		}
	}

	// Delete local files.
	for _, p := range delLocalFiles {
		if err := os.Remove(localJoin(localRoot, p)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "delete local %s: %v\n", p, err)
			stats.Errors++
		} else {
			stats.DeletedL++
		}
	}

	// Delete remote files.
	if len(delRemoteFiles) > 0 {
		if err := remote.DeleteFiles(delRemoteFiles); err != nil {
			fmt.Fprintf(os.Stderr, "delete remote files: %v\n", err)
			stats.Errors++
		} else {
			stats.DeletedR += len(delRemoteFiles)
		}
	}

	// Delete local dirs (deepest first — already sorted by planner).
	for _, p := range delLocalDirs {
		if err := os.RemoveAll(localJoin(localRoot, p)); err != nil {
			fmt.Fprintf(os.Stderr, "delete local dir %s: %v\n", p, err)
			stats.Errors++
		} else {
			stats.DeletedL++
		}
	}

	// Delete remote dirs (deepest first — already sorted by planner).
	for _, p := range delRemoteDirs {
		if err := remote.DeleteDir(p); err != nil {
			fmt.Fprintf(os.Stderr, "delete remote dir %s: %v\n", p, err)
			stats.Errors++
		} else {
			stats.DeletedR++
		}
	}

	return stats, nil
}

// partitionBySize splits paths into those below (small) and at/above (large) the
// given size threshold using the size recorded in entries. Paths not found in
// entries default to 0 size (treated as small).
func partitionBySize(paths []string, entries map[string]Entry, threshold int64) (small, large []string) {
	for _, p := range paths {
		if e, ok := entries[p]; ok && e.Size >= threshold {
			large = append(large, p)
		} else {
			small = append(small, p)
		}
	}
	return
}

// flattenPath replaces "/" with "__" to produce a safe single-component filename
// for conflict backup destinations, avoiding the need to create deep remote trees.
func flattenPath(rel string) string {
	return strings.ReplaceAll(rel, "/", "__")
}

// backupLocalLoser copies the local file (or dir tree) at localRoot/rel into
// the conflict backup directory for syncName. Returns 1 on success, 0 on failure
// (failures are warnings only and do not abort the sync).
func backupLocalLoser(syncName, localRoot, rel string) int {
	src := localJoin(localRoot, rel)
	info, err := os.Lstat(src)
	if err != nil {
		// Source doesn't exist (nothing to back up).
		return 0
	}

	destDir := filepath.Join(resolvedConflictDir(), syncName)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "conflict backup mkdir %s: %v\n", destDir, err)
		return 0
	}

	ts := time.Now().Unix()
	flat := flattenPath(rel)
	dst := filepath.Join(destDir, fmt.Sprintf("%s.%d", flat, ts))

	if info.IsDir() {
		// Use cp -R for directories.
		if err := exec.Command("cp", "-R", src, dst).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "conflict backup local dir %s: %v\n", rel, err)
			return 0
		}
	} else {
		if err := copyFile(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "conflict backup local %s: %v\n", rel, err)
			return 0
		}
	}
	return 1
}

// backupRemoteLoser backs up the remote file (or dir) at remote.Root/rel into
// the remote conflict backup directory when the transport is SSH, or into the
// local ConflictDir when the transport is local:. Returns 1 on success, 0 on
// failure (failures are warnings only).
func backupRemoteLoser(syncName string, remote *RemoteConn, rel string) int {
	ts := time.Now().Unix()
	flat := flattenPath(rel)
	destName := fmt.Sprintf("%s.%d", flat, ts)

	switch remote.Kind {
	case transportLocal:
		// Both sides are local — copy the remote file into the local conflict dir.
		src := filepath.Join(remote.Root, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if err != nil {
			return 0
		}
		destDir := filepath.Join(resolvedConflictDir(), syncName)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "conflict backup mkdir %s: %v\n", destDir, err)
			return 0
		}
		dst := filepath.Join(destDir, destName)
		if info.IsDir() {
			if err := exec.Command("cp", "-R", src, dst).Run(); err != nil {
				fmt.Fprintf(os.Stderr, "conflict backup remote(local) dir %s: %v\n", rel, err)
				return 0
			}
		} else {
			if err := copyFile(src, dst); err != nil {
				fmt.Fprintf(os.Stderr, "conflict backup remote(local) %s: %v\n", rel, err)
				return 0
			}
		}
		return 1

	case transportSSH:
		// Back up the remote file to the remote conflict dir via SSH.
		// Flatten the relpath so we don't need to create deep remote trees.
		remoteSrc := absoluteRemotePath(remote.Root, rel)
		remoteConflictDir := fmt.Sprintf("~/.config/caravan/conflicts/%s", syncName)
		remoteDst := fmt.Sprintf("%s/%s", remoteConflictDir, destName)

		// Quote the paths for the shell; use the same pattern as deleteSSH.
		quotePath := func(p string) string {
			if strings.HasPrefix(p, "~/") {
				return `"$HOME/` + p[2:] + `"`
			} else if p == "~" {
				return `"$HOME"`
			}
			return `'` + p + `'`
		}

		cmd := fmt.Sprintf(
			`mkdir -p %s && cp -pR %s %s`,
			quotePath(remoteConflictDir),
			quotePath(remoteSrc),
			quotePath(remoteDst),
		)
		if err := exec.Command("ssh", "-o", "BatchMode=yes", remote.Host, cmd).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "conflict backup remote %s: %v\n", rel, err)
			return 0
		}
		return 1
	}
	return 0
}

// pruneConflictBackups deletes files in the conflict backup dir for syncName
// that are older than 7 days. Best-effort: errors are silently ignored.
func pruneConflictBackups(syncName string) {
	dir := filepath.Join(resolvedConflictDir(), syncName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // dir doesn't exist yet — nothing to prune
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// buildBase constructs a new base from the intersection of post-apply local and
// remote scans.  Only paths present on BOTH sides are recorded; a path missing
// on one side means the transfer failed and should be retried next run.
//
// Hash is taken from the local entry when present (after a successful sync both
// sides have identical content, so either hash would be equivalent); if local
// has no hash the remote hash is used as a fallback.
func buildBase(local, remote map[string]Entry) map[string]BaseEntry {
	base := make(map[string]BaseEntry, len(local))
	for p, l := range local {
		if r, ok := remote[p]; ok {
			hash := l.Hash
			if hash == "" {
				hash = r.Hash
			}
			base[p] = BaseEntry{
				Hash:   hash,
				LSize:  l.Size,
				LMtime: l.Mtime,
				RSize:  r.Size,
				RMtime: r.Mtime,
				Dir:    l.IsDir || r.IsDir,
			}
		}
	}
	return base
}

// printPlan emits an aligned table of planned actions.
func printPlan(name string, actions []Action) {
	if len(actions) == 0 {
		return
	}
	fmt.Printf("~ %s — plan:\n", name)
	maxPath := 0
	for _, a := range actions {
		if len(a.Path) > maxPath {
			maxPath = len(a.Path)
		}
	}
	for _, a := range actions {
		marker := "~"
		switch a.Op {
		case OpPush:
			marker = "↑"
		case OpPull:
			marker = "↓"
		case OpDeleteLocal, OpDeleteRemote:
			marker = "✗"
		case OpMkdirLocal, OpMkdirRemote:
			marker = "+"
		case OpPreDeleteLocal, OpPreDeleteRemote:
			marker = "⚡"
		}
		fmt.Printf("  %s  %-*s  %s  %s\n", marker, maxPath, a.Path, opSide(a.Op), a.Reason)
	}
}

func opSide(op Op) string {
	switch op {
	case OpPush, OpMkdirRemote, OpDeleteRemote, OpPreDeleteRemote:
		return "→remote"
	case OpPull, OpMkdirLocal, OpDeleteLocal, OpPreDeleteLocal:
		return "←local "
	}
	return "       "
}

func countFiles(entries map[string]Entry) int {
	n := 0
	for _, e := range entries {
		if !e.IsDir {
			n++
		}
	}
	return n
}

func localJoin(root, rel string) string {
	return root + string(os.PathSeparator) + strings.ReplaceAll(rel, "/", string(os.PathSeparator))
}
