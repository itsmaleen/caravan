package syncengine

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// SyncStats summarises what happened during one sync run.
type SyncStats struct {
	Pushed    int
	Pulled    int
	DeletedL  int // local deletions
	DeletedR  int // remote deletions
	Conflicts int
	Errors    int
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
	stats, applyErr := applyActions(actions, localRoot, remote, localEntries, remoteEntries)

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

	return applyErr
}

// applyActions executes the plan.
//
// The action slice is already sorted by sortActions:
//   0. preDeleteLocal/preDeleteRemote (deepest first) — run inline immediately
//   1. mkdirLocal/mkdirRemote (shallow first) — run inline immediately
//   2. push/pull — batched then executed
//   3. deleteLocal/deleteRemote (deepest first) — batched then executed
//
// Pre-deletes are executed inline as they are encountered so that the
// subsequent mkdir/push/pull operations find a clean slate.
func applyActions(
	actions []Action,
	localRoot string,
	remote *RemoteConn,
	localEntries, remoteEntries map[string]Entry,
) (SyncStats, error) {
	var stats SyncStats
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
		}

		switch a.Op {
		case OpPreDeleteLocal:
			// Recursively remove the local path to make room for the remote winner.
			if err := os.RemoveAll(localJoin(localRoot, a.Path)); err != nil {
				fmt.Fprintf(os.Stderr, "pre-delete local %s: %v\n", a.Path, err)
				stats.Errors++
			} else {
				stats.DeletedL++
			}
		case OpPreDeleteRemote:
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

	// Push files.
	if len(pushPaths) > 0 {
		if err := remote.Push(localRoot, pushPaths); err != nil {
			fmt.Fprintf(os.Stderr, "push: %v\n", err)
			stats.Errors++
		} else {
			stats.Pushed += len(pushPaths)
		}
	}

	// Pull files.
	if len(pullPaths) > 0 {
		if err := remote.Pull(localRoot, pullPaths); err != nil {
			fmt.Fprintf(os.Stderr, "pull: %v\n", err)
			stats.Errors++
		} else {
			stats.Pulled += len(pullPaths)
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
