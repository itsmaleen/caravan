// Package syncengine implements bidirectional folder sync over ssh.
package syncengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"caravan/internal/buildinfo"
	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// Entry describes one file or directory in a scan result.
type Entry struct {
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // UnixNano
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
	Hash  string `json:"hash,omitempty"` // sha256 hex; only set when hashFiles=true
}

// ScanResult is the JSON envelope emitted by CmdScan.
type ScanResult struct {
	Entries map[string]Entry `json:"entries"`
	Version string           `json:"version,omitempty"` // caravan version that produced this scan
}

// ScanDir walks root and returns a map of slash-separated relative path → Entry.
// excludes patterns are matched against each path segment (base name) using path.Match.
// When hashFiles is true, a sha256 hex digest is computed for every regular file
// (streamed, not slurped) and stored in Entry.Hash.
// Symlinks are skipped; the count of skipped symlinks is returned.
// Paths containing a single-quote or newline are skipped with a warning.
func ScanDir(root string, excludes []string, hashFiles bool) (map[string]Entry, int, error) {
	entries := make(map[string]Entry)
	symlinks := 0

	// Ensure the root exists; return empty map if it doesn't.
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return entries, 0, nil
	}

	err := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "scan: skip %s: %v\n", absPath, walkErr)
			return nil
		}

		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Normalize to NFC so both sides of a sync agree on the key even when
		// one filesystem stores NFD (macOS tar extraction does): APFS lookups
		// are normalization-insensitive, so operating on the NFC name still
		// reaches an NFD-stored file. Without this, unicode names ping-pong
		// as "new" on both sides forever.
		rel = norm.NFC.String(rel)

		// Skip paths with characters that would break shell quoting.
		if strings.ContainsAny(rel, "'\n") {
			fmt.Fprintf(os.Stderr, "scan: skip %q: contains single quote or newline\n", rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Check excludes against every path segment.
		if matchesExclude(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			symlinks++
			return nil
		}

		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan: stat %s: %v\n", absPath, err)
			return nil
		}

		e := Entry{
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
			Mode:  uint32(info.Mode()),
			IsDir: info.IsDir(),
		}

		if hashFiles && !info.IsDir() {
			if h, herr := hashFile(absPath); herr != nil {
				fmt.Fprintf(os.Stderr, "scan: hash %s: %v\n", absPath, herr)
			} else {
				e.Hash = h
			}
		}

		entries[rel] = e
		return nil
	})

	return entries, symlinks, err
}

// hashFile computes the sha256 digest of the file at path and returns it as a
// lowercase hex string.  The file is streamed so large files are not slurped
// into memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// matchesExclude returns true when rel matches any exclude pattern on any segment.
func matchesExclude(rel string, excludes []string) bool {
	segments := strings.Split(rel, "/")
	for _, seg := range segments {
		for _, pat := range excludes {
			if ok, _ := path.Match(pat, seg); ok {
				return true
			}
		}
	}
	return false
}

// entriesEqual reports whether two entry maps are identical: same keys, same
// Entry values. It is used by waitForChange to detect a mutation.
func entriesEqual(a, b map[string]Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, ea := range a {
		eb, ok := b[k]
		if !ok {
			return false
		}
		if ea != eb {
			return false
		}
	}
	return true
}

// waitForChange takes an initial ScanDir snapshot of root and then polls every
// poll duration until either the snapshot changes (returns early with
// changed=true) or window elapses (returns the current snapshot with
// changed=false). It always returns the final entries and whether a change was
// detected.
func waitForChange(root string, excludes []string, hashFiles bool, window, poll time.Duration) (map[string]Entry, bool) {
	initial, _, err := ScanDir(root, excludes, hashFiles)
	if err != nil {
		// Treat scan failure as a change so the caller triggers a sync pass.
		return initial, true
	}

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		current, _, err := ScanDir(root, excludes, hashFiles)
		if err != nil {
			return current, true
		}
		if !entriesEqual(initial, current) {
			return current, true
		}
	}
	// Final scan at window end.
	final, _, _ := ScanDir(root, excludes, hashFiles)
	return final, false
}

// CmdScan implements `caravan scan --json DIR [--exclude a,b,c] [--hash] [--wait <dur>]`.
// With --wait the command long-polls for a change in DIR for up to <dur>; if a
// change is detected it emits the new snapshot immediately and exits 0.  If no
// change is seen within <dur> it emits the current snapshot and exits 0.
// Either way it prints "changed=true" or "changed=false" to STDERR so callers
// (WaitScan) can distinguish the two outcomes without exit-code gymnastics.
// Without --wait behaviour is unchanged.
func CmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "output directory state as JSON (required)")
	excludeStr := fs.String("exclude", "", "comma-separated exclude patterns")
	hashFiles := fs.Bool("hash", false, "compute sha256 hash for every regular file")
	waitStr := fs.String("wait", "", "long-poll for a change for up to this duration (e.g. 20s); only meaningful with --json")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		return 2
	}

	if !*jsonOut {
		fmt.Fprintln(os.Stderr, "scan: --json flag is required")
		return 2
	}

	if len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "scan: expected exactly one DIR argument")
		return 2
	}

	var excludes []string
	if *excludeStr != "" {
		for _, e := range strings.Split(*excludeStr, ",") {
			if e = strings.TrimSpace(e); e != "" {
				excludes = append(excludes, e)
			}
		}
	}

	dir := manifest.ExpandPath(positionals[0])

	var result map[string]Entry
	var symlinks int

	if *waitStr != "" {
		window, werr := time.ParseDuration(*waitStr)
		if werr != nil {
			fmt.Fprintf(os.Stderr, "scan: invalid --wait %q: %v\n", *waitStr, werr)
			return 2
		}
		changed := false
		result, changed = waitForChange(dir, excludes, *hashFiles, window, 250*time.Millisecond)
		if changed {
			fmt.Fprintln(os.Stderr, "changed=true")
		} else {
			fmt.Fprintln(os.Stderr, "changed=false")
		}
	} else {
		result, symlinks, err = ScanDir(dir, excludes, *hashFiles)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return 1
		}
		if symlinks > 0 {
			fmt.Fprintf(os.Stderr, "scan: skipped %d symlink(s)\n", symlinks)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ScanResult{Entries: result, Version: buildinfo.Version}); err != nil {
		fmt.Fprintf(os.Stderr, "scan: encode: %v\n", err)
		return 1
	}
	return 0
}
