// Package syncengine implements bidirectional folder sync over ssh.
package syncengine

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

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
}

// ScanResult is the JSON envelope emitted by CmdScan.
type ScanResult struct {
	Entries map[string]Entry `json:"entries"`
	Version string           `json:"version,omitempty"` // caravan version that produced this scan
}

// ScanDir walks root and returns a map of slash-separated relative path → Entry.
// excludes patterns are matched against each path segment (base name) using path.Match.
// Symlinks are skipped; the count of skipped symlinks is returned.
// Paths containing a single-quote or newline are skipped with a warning.
func ScanDir(root string, excludes []string) (map[string]Entry, int, error) {
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

		entries[rel] = Entry{
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
			Mode:  uint32(info.Mode()),
			IsDir: info.IsDir(),
		}
		return nil
	})

	return entries, symlinks, err
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

// CmdScan implements `caravan scan --json DIR [--exclude a,b,c]`.
func CmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "output directory state as JSON (required)")
	excludeStr := fs.String("exclude", "", "comma-separated exclude patterns")

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

	result, symlinks, err := ScanDir(dir, excludes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
		return 1
	}
	if symlinks > 0 {
		fmt.Fprintf(os.Stderr, "scan: skipped %d symlink(s)\n", symlinks)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ScanResult{Entries: result, Version: buildinfo.Version}); err != nil {
		fmt.Fprintf(os.Stderr, "scan: encode: %v\n", err)
		return 1
	}
	return 0
}
