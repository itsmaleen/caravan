package syncengine

import (
	"caravan/internal/watch"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
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
	Chmods          int // permission-only changes applied
}

// CmdSync implements `caravan sync [NAME] [--watch] [--interval 1s] [--dry-run] [--bootstrap] [-f manifest]`.
func CmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestPath := fs.String("f", "", "manifest path")
	dryRun := fs.Bool("dry-run", false, "print plan without applying")
	watch := fs.Bool("watch", false, "loop continuously")
	intervalStr := fs.String("interval", "1s", "local poll interval in watch mode (e.g. 1s)")
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

	// Fail closed if the per-uid ssh ControlMaster dir isn't a private 0700 dir
	// we own — otherwise ssh could route through another local user's mux socket.
	// Only when a selected entry actually uses ssh: a local-only manifest opens no
	// ssh master, so a stale/unsafe socket dir must not fail it. Covers one-shot
	// and watch before any ssh op runs.
	if anySSHEntry(entries) {
		if err := EnsureSecureSockDir(); err != nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
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

	// Watch mode: one goroutine per sync entry.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	names := make([]string, len(entries))
	for i, s := range entries {
		names[i] = s.Name
	}
	fmt.Printf("watching %s (local poll %s, remote long-poll 20s)\n", strings.Join(names, ", "), interval)

	var wg sync.WaitGroup
	for _, s := range entries {
		s := s // capture
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWatchEntry(s, *dryRun, interval, sigCh)
		}()
	}
	wg.Wait()
	return 0
}

// waitScanResult is the outcome of one WaitScan call delivered on a channel.
type waitScanResult struct {
	entries map[string]Entry
	changed bool
	err     error
}

// runWatchEntry is the per-entry watch loop. It runs until sigCh fires.
//
// Loop invariant:
//  1. Run one initial full sync pass.
//  2. Start a remote WaitScan (20s window) in a goroutine → waitCh.
//  3. Concurrently poll local dir every interval; compare to prevLocal.
//  4. Whichever fires first:
//     - local change  → sync, drain/discard in-flight WaitScan, restart loop.
//     - WaitScan changed=true  → sync, restart loop.
//     - WaitScan changed=false → restart loop (no sync).
//     - WaitScan error → warn, exponential back-off, restart loop.
//     - sigCh → return.
func runWatchEntry(s manifest.Sync, dryRun bool, localPoll time.Duration, sigCh <-chan os.Signal) {
	const remoteWindow = 20 * time.Second
	const backoffBase = 5 * time.Second
	const backoffMax = 60 * time.Second

	// Clear any stale ControlMaster left by a previous (possibly pre-keepalive)
	// process before the first op, so a restart can't inherit a half-open master
	// and immediately re-hang. Best-effort.
	if remote, err := parseSyncRemote(s); err == nil {
		remote.closeMaster()
	}

	localRoot := manifest.ExpandPath(s.Local)
	excludes := s.Excludes()

	// Initial sync pass. Change detection starts from the snapshot the pass
	// planned from, so edits made while it ran still count as changes.
	seen, remoteSeen, pending, err := syncPass(s, dryRun, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync %s: %v\n", s.Name, err)
	}
	// A pass that left work unfinished is retried on a timer: nothing else
	// would trigger one, since neither tree changed.
	var retry passRetry
	retry.after(pending && !dryRun, time.Now())
	// Digest of the remote as of the last pass; the long-poll reports a change
	// at once if the remote no longer matches it.
	remoteDigest := ""
	if remoteSeen != nil {
		remoteDigest = EntriesDigest(remoteSeen)
	}
	prevLocal := seen
	if prevLocal == nil {
		prevLocal, _, _ = ScanDir(localRoot, excludes, s.Checksum)
	}

	backoff := backoffBase
	var waitSuppressedUntil time.Time

	// Native watching turns a ~1 s poll into ~0.1 s propagation and stops the
	// idle tree walks. Events only trigger a rescan; the rescan decides.
	pollEvery := localPoll
	var localEvents <-chan struct{}
	if w, err := watch.New(localRoot, func(rel string, isDir bool) bool { return matchesExclude(rel, excludes) }); err == nil {
		defer w.Close()
		localEvents = w.C
		pollEvery = safetyRescan(localPoll)
	}

	for {
		// Parse remote to run WaitScan.
		remote, err := parseSyncRemote(s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync %s: parse remote: %v\n", s.Name, err)
			// Wait for signal or sleep before retry.
			select {
			case <-sigCh:
				return
			case <-time.After(backoff):
			}
			continue
		}

		// While the remote long-poll is in an error-backoff window, keep local
		// polling at full cadence and only retry the long-poll when the window
		// expires — a broken remote must never blind us to local changes.
		var waitCh chan waitScanResult
		var retryC <-chan time.Time
		if d := time.Until(waitSuppressedUntil); d > 0 {
			retryC = time.After(d)
		} else {
			waitCh = make(chan waitScanResult, 1)
			since := remoteDigest
			go func() {
				entries, changed, err := remote.WaitScan(excludes, s.Checksum, remoteWindow, since)
				waitCh <- waitScanResult{entries: entries, changed: changed, err: err}
			}()
		}

		// Local change detection: native events when available, with a slow
		// full rescan as the safety net; otherwise poll at localPoll.
		ticker := time.NewTicker(pollEvery)
		passRetryC := retry.C(time.Now())

		triggered := false
		var drainWait bool

	pollLoop:
		for {
			select {
			case <-sigCh:
				ticker.Stop()
				// Drain waitCh so the goroutine can exit.
				go func() { <-waitCh }()
				return

			case <-localEvents:
				watch.Settle(localEvents, 20*time.Millisecond, 500*time.Millisecond)
				cur, _, _ := ScanDir(localRoot, excludes, s.Checksum)
				if !entriesEqual(prevLocal, cur) {
					prevLocal = cur
					triggered = true
					drainWait = true
					break pollLoop
				}

			case <-ticker.C:
				cur, _, _ := ScanDir(localRoot, excludes, s.Checksum)
				if !entriesEqual(prevLocal, cur) {
					prevLocal = cur
					triggered = true
					drainWait = true
					break pollLoop
				}

			case <-passRetryC:
				// The last pass left a transfer unfinished; try again.
				triggered = true
				drainWait = true
				break pollLoop

			case <-retryC:
				// Backoff window over — restart the loop to attempt a fresh
				// long-poll. (retryC is nil unless suppressed; nil never fires.)
				break pollLoop

			case res := <-waitCh:
				if res.err != nil {
					fmt.Fprintf(os.Stderr, "sync %s: remote wait-scan error: %v; local polling continues, retrying long-poll in %s\n",
						s.Name, res.err, backoff)
					waitSuppressedUntil = time.Now().Add(backoff)
					backoff *= 2
					if backoff > backoffMax {
						backoff = backoffMax
					}
					// Run a full pass now: its remote scan performs the
					// bootstrap/version handshake, which heals the most likely
					// cause — a stale remote binary that predates --wait.
					triggered = true
					break pollLoop
				}
				// Success — reset backoff.
				backoff = backoffBase
				if res.changed {
					triggered = true
				}
				break pollLoop
			}
		}

		ticker.Stop()

		if drainWait && waitCh != nil {
			// Local change fired first; drain the in-flight WaitScan asynchronously
			// so its goroutine is not leaked. A single redundant sync pass from a
			// late-arriving changed=true is acceptable (cheap no-op or catches an
			// additional remote change).
			go func() { <-waitCh }()
		}

		if triggered {
			seen, remoteSeen, pending, err := syncPass(s, dryRun, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sync %s: %v\n", s.Name, err)
			}
			retry.after(pending && !dryRun, time.Now())
			if remoteSeen != nil {
				remoteDigest = EntriesDigest(remoteSeen)
			} else {
				remoteDigest = "" // unknown: fall back to the poll's own baseline
			}
			// Compare future polls against what this pass planned from, not a
			// fresh scan: a fresh scan would absorb files written while the
			// pass ran, and they would never sync. Changes the pass itself
			// made (pulls) show up as one extra, cheap pass.
			if seen != nil {
				prevLocal = seen
			} else {
				prevLocal, _, _ = ScanDir(localRoot, excludes, s.Checksum)
			}
		}
	}
}

// passRetry schedules a retry pass after one that left work unfinished. The
// delay doubles from passRetryBase up to passRetryMax while passes keep
// failing, so a file that can never transfer costs one pass every few minutes,
// and it resets once a pass finishes cleanly.
type passRetry struct {
	at      time.Time // zero: no retry due
	backoff time.Duration
}

const (
	passRetryBase = 5 * time.Second
	passRetryMax  = 5 * time.Minute
)

// after records the outcome of a pass that ended at now.
func (r *passRetry) after(pending bool, now time.Time) {
	if !pending {
		*r = passRetry{}
		return
	}
	switch {
	case r.backoff == 0:
		r.backoff = passRetryBase
	case r.backoff*2 > passRetryMax:
		r.backoff = passRetryMax
	default:
		r.backoff *= 2
	}
	r.at = now.Add(r.backoff)
}

// C fires when the retry is due, and never when none is scheduled.
func (r *passRetry) C(now time.Time) <-chan time.Time {
	if r.at.IsZero() {
		return nil
	}
	return time.After(r.at.Sub(now))
}

// runSyncEntry runs one sync pass for a single [[sync]] entry. In quiet mode
// (watch loop) an in-sync pass prints nothing.
func runSyncEntry(s manifest.Sync, dryRun, quiet bool) error {
	_, _, _, err := syncPass(s, dryRun, quiet)
	return err
}

// syncPass runs one sync pass and returns the local snapshot it planned from
// (nil when the pass was skipped or failed before scanning). The watch loop
// compares later polls against exactly this snapshot: anything written after
// the pass scanned is then still seen as a change, instead of being absorbed
// into a fresh post-sync scan and never synced.
// The second result is the remote snapshot as of the end of the pass; the
// watch loop sends its digest with the next long-poll.
// The third result reports work the pass planned but could not finish or
// confirm (a transfer failed, or its destination is not what was installed).
// Both snapshots can still match their trees, so no change would ever fire;
// the watch loop schedules a retry instead of waiting for one. A pass that
// was skipped (the entry is locked) or failed before it could plan reports
// the same: it examined nothing, so it cannot clear work an earlier pass left.
func syncPass(s manifest.Sync, dryRun, quiet bool) (map[string]Entry, map[string]Entry, bool, error) {
	release, lockErr := AcquireSyncLock(s.Name)
	if lockErr != nil {
		if errors.Is(lockErr, ErrSyncBusy) {
			// Another caravan process (daemon or manual run) owns this entry
			// right now; skipping is the correct behavior, not an error state.
			if !quiet {
				fmt.Printf("- %s skipped: %v\n", s.Name, lockErr)
			}
			return nil, nil, true, nil
		}
		return nil, nil, true, lockErr
	}
	defer release()

	localRoot := manifest.ExpandPath(s.Local)
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return nil, nil, true, fmt.Errorf("mkdir local %s: %w", localRoot, err)
	}

	remote, err := parseSyncRemote(s)
	if err != nil {
		return nil, nil, true, err
	}

	excludes := s.Excludes()

	// Scan local.
	localEntries, _, err := ScanDir(localRoot, excludes, s.Checksum)
	if err != nil {
		return nil, nil, true, fmt.Errorf("local scan: %w", err)
	}

	// Scan remote (creates remote root if needed).
	remoteEntries, err := remote.Scan(excludes, s.Checksum)
	if err != nil {
		return nil, nil, true, fmt.Errorf("remote scan: %w", err)
	}

	// Load base snapshot.
	state, err := LoadState(s.Name)
	if err != nil {
		return nil, nil, true, fmt.Errorf("load state: %w", err)
	}

	// Plan.
	actions := Plan(state.Pairs, localEntries, remoteEntries, s.Checksum)

	if len(actions) == 0 {
		// Even with nothing to transfer, refresh the base when the current
		// scans carry information the stored base lacks (modes and hashes
		// added by newer versions) — otherwise features gated on base
		// metadata (chmod detection, checksum mode) stay dormant forever on
		// pairs that are already in sync.
		if !dryRun {
			refreshed := buildBase(localEntries, remoteEntries)
			if !basePairsEqual(state.Pairs, refreshed) {
				if err := SaveState(s.Name, &State{Pairs: refreshed, LastSync: time.Now().UnixNano()}); err != nil {
					fmt.Fprintf(os.Stderr, "sync: save state: %v\n", err)
				}
			}
		}
		if !quiet {
			total := countFiles(localEntries)
			fmt.Printf("✓ %s in sync (%d files)\n", s.Name, total)
		}
		return localEntries, remoteEntries, false, nil
	}

	// Print plan.
	printPlan(s.Name, actions)

	if dryRun {
		return localEntries, remoteEntries, false, nil
	}

	// Apply.
	stats, applyErr := applyActions(actions, localRoot, remote, localEntries, remoteEntries, s.Name, s.DeltaThreshold())

	// Rescan local (cheap) and refresh only the remote paths this pass
	// changed: a full remote scan is the most expensive step of a pass.
	// Untouched remote paths keep their pre-pass entries, so an edit made on
	// the remote during the pass still differs from the base next time.
	touchedL, touchedR := touchedPaths(actions)
	newLocal, _, _ := ScanDir(localRoot, excludes, s.Checksum)
	newRemote, ok := remoteAfterPass(remote, remoteEntries, actions, touchedR, s.Checksum)
	if !ok {
		full, _ := remote.Scan(excludes, s.Checksum)
		newRemote = dropUnstable(remoteEntries, full, touchedR)
	}
	// Leave out any file that changed during the pass without this pass
	// changing it: its new content was never transferred, so recording it as
	// in sync would hide the edit forever (the size guard in buildBase only
	// catches edits that change the size).
	localForBase := dropUnstable(localEntries, newLocal, touchedL)
	// A source saved again between the scan and the transfer: tar sent the
	// newer bytes, so both sides now hold the same file but the snapshot the
	// pass planned from is stale. Record what actually landed, or the next
	// pass takes its own transfer for an edit on the far side and backs it up
	// as a conflict loser, once per save.
	settleTransfers(actions, localForBase, newLocal, remoteEntries, newRemote, remote, s.Checksum)
	newBase := buildBase(localForBase, newRemote)
	// A transferred file that no longer looks like what the transfer put
	// there was edited between placement and the re-read (or never landed).
	// Don't record it as synced.
	unconfirmed := holdBackUnconfirmed(newBase, state.Pairs, actions, localEntries, newLocal, remoteEntries, newRemote)

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
	if stats.Chmods > 0 {
		fmt.Printf(", chmods %d", stats.Chmods)
	}
	if stats.Errors > 0 {
		fmt.Printf(", errors %d", stats.Errors)
	}
	fmt.Println()
	if stats.ConflictBackups > 0 {
		fmt.Printf("  conflict backups in %s/ (%d)\n",
			filepath.Join(resolvedConflictDir(), s.Name), stats.ConflictBackups)
	}

	return localEntries, newRemote, stats.Errors > 0 || unconfirmed > 0, applyErr
}

// applyActions executes the plan.
//
// The action slice is already sorted by sortActions:
//
//  0. preDeleteLocal/preDeleteRemote (deepest first) — run inline immediately
//  1. mkdirLocal/mkdirRemote (shallow first) — run inline immediately
//  2. push/pull — batched then executed
//  3. deleteLocal/deleteRemote (deepest first) — batched then executed
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
	var chmodLocalActions, chmodRemoteActions []Action

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
		case OpChmodLocal:
			chmodLocalActions = append(chmodLocalActions, a)
		case OpChmodRemote:
			chmodRemoteActions = append(chmodRemoteActions, a)
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

	// Apply local permission changes (run last, after content is settled).
	for _, a := range chmodLocalActions {
		target := localJoin(localRoot, a.Path)
		if err := os.Chmod(target, fs.FileMode(a.Mode)); err != nil {
			fmt.Fprintf(os.Stderr, "chmod local %s: %v\n", a.Path, err)
			stats.Errors++
		} else {
			stats.Chmods++
		}
	}

	// Apply remote permission changes as a single batched SSH invocation.
	if len(chmodRemoteActions) > 0 {
		pairs := make([]ChmodPair, len(chmodRemoteActions))
		for i, a := range chmodRemoteActions {
			pairs[i] = ChmodPair{Path: a.Path, Mode: a.Mode}
		}
		if err := remote.Chmod(pairs); err != nil {
			fmt.Fprintf(os.Stderr, "chmod remote: %v\n", err)
			stats.Errors++
		} else {
			stats.Chmods += len(chmodRemoteActions)
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
		if err := remote.sshCommand(cmd).Run(); err != nil {
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
//
// Size-mismatch guard: when both entries are files and their sizes differ, the
// path is NOT recorded in the base.  This prevents a truncated file left by an
// interrupted tar extraction from being blessed as "in sync" — on the next run
// the planner will see a divergence and re-reconcile it.  Directories are
// always recorded regardless of any size field.
func buildBase(local, remote map[string]Entry) map[string]BaseEntry {
	base := make(map[string]BaseEntry, len(local))
	for p, l := range local {
		r, ok := remote[p]
		if !ok {
			continue
		}
		// Skip file pairs whose sizes disagree — at least one side is partial.
		if !l.IsDir && !r.IsDir && l.Size != r.Size {
			fmt.Fprintf(os.Stderr,
				"sync: base: skipping %s (size mismatch local=%d remote=%d — will re-reconcile)\n",
				p, l.Size, r.Size)
			continue
		}
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
			LMode:  l.Mode & 0o777,
			RMode:  r.Mode & 0o777,
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
		case OpChmodLocal, OpChmodRemote:
			marker = "±"
		}
		fmt.Printf("  %s  %-*s  %s  %s\n", marker, maxPath, a.Path, opSide(a.Op), a.Reason)
	}
}

func opSide(op Op) string {
	switch op {
	case OpPush, OpMkdirRemote, OpDeleteRemote, OpPreDeleteRemote, OpChmodRemote:
		return "→remote"
	case OpPull, OpMkdirLocal, OpDeleteLocal, OpPreDeleteLocal, OpChmodLocal:
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

// anySSHEntry reports whether any selected entry uses ssh transport (so the ssh
// ControlMaster dir must be secured). A local-only manifest opens no ssh.
func anySSHEntry(entries []manifest.Sync) bool {
	for _, s := range entries {
		if r, err := ParseRemote(s.Remote); err == nil && r.Kind == transportSSH {
			return true
		}
	}
	return false
}

// parseSyncRemote is ParseRemote plus the entry name, which keys the per-entry
// ssh ControlPath (RemoteConn.controlPath) so distinct entries never share a
// master even when they target the same remote.
func parseSyncRemote(s manifest.Sync) (*RemoteConn, error) {
	r, err := ParseRemote(s.Remote)
	if err != nil {
		return nil, err
	}
	r.Name = s.Name
	return r, nil
}

// safetyRescan is the full-rescan interval used when native events drive
// change detection: slow, because it only catches events the OS dropped.
func safetyRescan(localPoll time.Duration) time.Duration {
	if localPoll > 10*time.Second {
		return localPoll
	}
	return 10 * time.Second
}

// remoteAfterPass builds the remote snapshot after applying actions by
// stat-ing only the touched paths and their parent directories. It reports
// false when that is not trustworthy (stat failed, or a pushed path is not
// where the pass put it), and the caller falls back to a full scan.
func remoteAfterPass(remote *RemoteConn, pre map[string]Entry, actions []Action, touched touchSet, hash bool) (map[string]Entry, bool) {
	all := touched.all()
	if len(all) == 0 {
		return pre, true
	}
	refresh := map[string]bool{}
	for _, p := range all {
		for q := p; q != "." && q != ""; q = path.Dir(q) {
			refresh[q] = true
		}
	}
	touchedSet := make(map[string]bool, len(touched.deep))
	for _, p := range touched.deep {
		touchedSet[p] = true
	}
	// Drop entries that will be re-stat'd, and everything under a deep-touched
	// path (a deleted or replaced subtree). A chmod'd directory is re-stat'd
	// but its children are kept: nothing under it changed.
	drop := func(p string) bool {
		if refresh[p] {
			return true
		}
		for q := path.Dir(p); q != "." && q != "/"; q = path.Dir(q) {
			if touchedSet[q] {
				return true
			}
		}
		return false
	}
	out := make(map[string]Entry, len(pre))
	for p, e := range pre {
		if !drop(p) {
			out[p] = e
		}
	}
	list := make([]string, 0, len(refresh))
	for p := range refresh {
		list = append(list, p)
	}
	got, err := remote.Stat(list, hash)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v; falling back to a full remote scan\n", err)
		return nil, false
	}
	for _, a := range actions {
		_, present := got[a.Path]
		switch a.Op {
		case OpPush, OpMkdirRemote:
			if !present {
				return nil, false
			}
		case OpDeleteRemote:
			if present {
				return nil, false
			}
		}
	}
	for p, e := range got {
		out[p] = e
	}
	// A chmod'd file takes only its new mode from the stat. Its content was not
	// transferred, so an edit or delete made to it during the pass must still
	// differ from the base, and from the digest the next long-poll sends.
	for _, p := range touched.shallow {
		if old, ok := pre[p]; ok && !old.IsDir {
			out[p] = modeOnly(old, got[p])
		}
	}
	return out, true
}

// modeOnly is the entry to record for a chmod'd file: what the pass planned
// with (pre), carrying the permission bits now on disk. post is the zero Entry
// when the file is gone, and pre is then kept whole.
func modeOnly(pre, post Entry) Entry {
	if post != (Entry{}) && !post.IsDir {
		pre.Mode = post.Mode
	}
	return pre
}

// touchSet is what a plan changes on one side. A deep path is created,
// replaced or deleted, so everything under it counts as touched too. A shallow
// path (a chmod) changes only itself: whatever happens to its children during
// the pass was not done by the pass.
type touchSet struct {
	deep, shallow []string
}

func (t touchSet) all() []string {
	return append(append(make([]string, 0, len(t.deep)+len(t.shallow)), t.deep...), t.shallow...)
}

// touchedPaths returns the paths a plan changes on each side.
func touchedPaths(actions []Action) (local, remote touchSet) {
	for _, a := range actions {
		switch a.Op {
		case OpPull, OpDeleteLocal, OpMkdirLocal, OpPreDeleteLocal:
			local.deep = append(local.deep, a.Path)
		case OpChmodLocal:
			local.shallow = append(local.shallow, a.Path)
		case OpPush, OpDeleteRemote, OpMkdirRemote, OpPreDeleteRemote:
			remote.deep = append(remote.deep, a.Path)
		case OpChmodRemote:
			remote.shallow = append(remote.shallow, a.Path)
		}
	}
	return local, remote
}

// settleTransfers fixes up the post-pass snapshots for files whose source
// changed between the scan and the transfer. tar reads a file when it sends
// it, not when the pass scanned it, so a file saved again in that window
// lands on the destination newer than the planned entry says.
//
// Push: if the local file as it is now is exactly what landed on the remote,
// that is the synced version, so the local half of the base takes it
// (localForBase is updated in place).
//
// Pull: the remote copy is not re-read after a pull, so when the local result
// is not the planned remote entry, the pulled paths are stat'd. If the remote
// file as it is now is exactly what landed locally, remotePost takes it.
//
// Anything else (the destination matches neither version of the source) is
// left alone: something really did write the far side, and the next pass must
// see it.
func settleTransfers(actions []Action, localForBase, localPost, remotePre, remotePost map[string]Entry, remote *RemoteConn, hash bool) {
	var recheck []string
	for _, a := range actions {
		switch a.Op {
		case OpPush:
			l, okL := localPost[a.Path]
			r, okR := remotePost[a.Path]
			if !okL || !okR || l.IsDir || r.IsDir {
				continue
			}
			if installed(l, r) {
				localForBase[a.Path] = l
			} else if intermediate(localForBase[a.Path], l, r) {
				// Saved twice during the pass: tar sent the save in between.
				// That is the synced version; the newest save is a plain edit.
				landed := r
				landed.Mode = l.Mode
				localForBase[a.Path] = landed
			}
		case OpPull:
			l, okL := localPost[a.Path]
			pre, okP := remotePre[a.Path]
			if okL && !l.IsDir && (!okP || !installed(pre, l)) {
				recheck = append(recheck, a.Path)
			}
		}
	}
	if len(recheck) == 0 || remote == nil {
		return
	}
	fresh, err := remote.Stat(recheck, hash)
	if err != nil {
		return // keep the planned entries: the worst case is the old behaviour
	}
	for _, p := range recheck {
		r, ok := fresh[p]
		if !ok || r.IsDir {
			continue
		}
		l := localPost[p]
		switch {
		case installed(r, l):
			remotePost[p] = r
		case intermediate(remotePre[p], r, l):
			// The remote was saved more than once during the pull and tar
			// fetched one of the saves in between: that is the synced
			// version, and the remote's newest save is a plain edit.
			landed := l
			landed.Mode = r.Mode
			remotePost[p] = landed
		}
	}
}

// intermediate reports whether dst is a copy of a version the source passed
// through during the pass: the source changed (post is newer than pre) and
// dst carries an mtime inside that window. Transfers preserve mtime, so only
// a save made on the source side between scan and re-read lands there. A
// second of slack below pre covers tar dropping the sub-second part.
func intermediate(pre, post, dst Entry) bool {
	if pre.IsDir || post.IsDir || dst.IsDir || pre.Mtime == 0 || post.Mtime <= pre.Mtime {
		return false
	}
	return dst.Mtime > pre.Mtime-int64(time.Second) && dst.Mtime <= post.Mtime
}

// installed reports whether dst still looks like the copy of src a transfer
// would have left: the same hash when both carry one, otherwise the same size
// and src's mtime. tar and older rsyncs drop the sub-second part of the mtime,
// so dst may be up to a second older than src, never newer.
func installed(src, dst Entry) bool {
	if src.IsDir || dst.IsDir {
		return src.IsDir == dst.IsDir
	}
	if src.Hash != "" && dst.Hash != "" {
		return src.Hash == dst.Hash
	}
	d := src.Mtime - dst.Mtime
	return src.Size == dst.Size && d >= 0 && d < int64(time.Second)
}

// unconfirmedTransfers returns the pushed and pulled files whose destination,
// as observed after the pass, is not what the transfer installed: something
// else wrote it between placement and the re-read, or the transfer failed.
// The source may itself have been edited mid-pass, so a destination matching
// either the planned or the current source counts as installed.
func unconfirmedTransfers(actions []Action, localPre, localPost, remotePre, remotePost map[string]Entry) (pushes, pulls []string) {
	matches := func(p string, dst Entry, srcs ...map[string]Entry) bool {
		for _, m := range srcs {
			if src, ok := m[p]; ok && installed(src, dst) {
				return true
			}
		}
		return false
	}
	for _, a := range actions {
		switch a.Op {
		case OpPush:
			if dst, ok := remotePost[a.Path]; ok && !matches(a.Path, dst, localPre, localPost) && !intermediate(localPre[a.Path], localPost[a.Path], dst) {
				pushes = append(pushes, a.Path)
			}
		case OpPull:
			if dst, ok := localPost[a.Path]; ok && !matches(a.Path, dst, remotePre, remotePost) {
				pulls = append(pulls, a.Path)
			}
		}
	}
	return pushes, pulls
}

// holdBackUnconfirmed keeps unconfirmed transfers out of the new base. Each
// keeps the base entry it had before the pass (or none), so the next pass
// sees both sides as they really are: a failed push is retried, and an edit
// that landed on top of a transfer is reconciled as a conflict instead of
// being recorded as the synced version. For pushes it also puts the pre-pass
// entry back into remotePost, the snapshot whose digest the next long-poll
// sends, so the remote reports a change at once and that pass runs. It
// does the same for deletes that did not happen, and returns how many
// actions it held back.
func holdBackUnconfirmed(newBase, oldBase map[string]BaseEntry, actions []Action, localPre, localPost, remotePre, remotePost map[string]Entry) int {
	pushes, pulls := unconfirmedTransfers(actions, localPre, localPost, remotePre, remotePost)
	for _, p := range append(pushes, pulls...) {
		fmt.Fprintf(os.Stderr, "sync: base: skipping %s (changed after it was transferred — will re-reconcile)\n", p)
		if old, ok := oldBase[p]; ok && !old.Dir {
			newBase[p] = old
		} else {
			delete(newBase, p)
		}
	}
	for _, p := range pushes {
		if pre, ok := remotePre[p]; ok {
			remotePost[p] = pre
		} else {
			delete(remotePost, p)
		}
	}
	// A delete that did not happen leaves the path on one side only, so
	// buildBase dropped it. Without its base entry the next pass would take the
	// surviving copy for a new file and copy it back, undoing the delete. Keep
	// the entry until the delete succeeds: the next pass then retries it, or
	// sees a conflict if the survivor was edited.
	held := len(pushes) + len(pulls)
	for _, a := range actions {
		var post map[string]Entry
		switch a.Op {
		case OpDeleteRemote:
			post = remotePost
		case OpDeleteLocal:
			post = localPost
		default:
			continue
		}
		if _, still := post[a.Path]; !still {
			continue
		}
		if old, ok := oldBase[a.Path]; ok {
			fmt.Fprintf(os.Stderr, "sync: base: keeping %s (its delete did not happen — will retry)\n", a.Path)
			newBase[a.Path] = old
			held++
		}
	}
	return held
}

// dropUnstable builds a side's post-pass snapshot for the base. Paths the
// pass changed take their fresh entry from post. Files it did not change keep
// the entry the pass planned with (pre), so anything else did to them during
// the pass, an edit or a delete, still differs from the base next time
// instead of being recorded as in sync. (Recording post here undid deletes:
// a file deleted mid-pass dropped out of the base, then looked new on the
// other side and was copied back.) Files created mid-pass are left out until
// the next pass. Directories always take post, since their mtimes move
// whenever a child changes. A file under a chmod'd directory is not touched:
// the chmod changed the directory alone. A chmod'd file takes only its mode
// from post: the pass did not transfer its content, so an edit or delete made
// to it meanwhile is kept visible like any other.
func dropUnstable(pre, post map[string]Entry, touched touchSet) map[string]Entry {
	set := make(map[string]bool, len(touched.deep))
	for _, t := range touched.deep {
		set[t] = true
	}
	shallow := make(map[string]bool, len(touched.shallow))
	for _, t := range touched.shallow {
		shallow[t] = true
	}
	isTouched := func(p string) bool {
		for {
			if set[p] {
				return true
			}
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				return false
			}
			p = p[:i]
		}
	}
	out := make(map[string]Entry, len(post))
	for p, e := range post {
		old, had := pre[p]
		switch {
		case e.IsDir || isTouched(p):
			out[p] = e
		case had && !old.IsDir && shallow[p]:
			out[p] = modeOnly(old, e)
		case had && !old.IsDir:
			out[p] = old
		}
	}
	for p, old := range pre {
		if _, still := post[p]; !still && !old.IsDir && !isTouched(p) {
			out[p] = old // deleted during the pass: keep it so the delete is seen next pass
		}
	}
	return out
}
