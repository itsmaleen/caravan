package provision

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// SyncStateDir overrides the directory where sync state files are read from.
// Empty means use ~/.config/caravan/sync-state. Tests should override this.
var SyncStateDir = ""

// syncState is a minimal struct that mirrors the fields we need from the sync
// engine's state file. (We must not import syncengine — SPEC requirement.)
type syncState struct {
	LastSync int64 `json:"lastSync"`
}

// CmdStatus prints a table showing each repo's status plus sync-folder state.
func CmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	if _, err := cliargs.ParseAnywhere(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	manifestPath := manifest.ResolvePath(*f)
	m, err := manifest.Load(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// ── Repos table ──────────────────────────────────────────────────────────
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tBRANCH\tAHEAD/BEHIND\tDIRTY\t.ENV\tMISE")

	for _, r := range m.Repos {
		dir := m.RepoDir(r)

		if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Fprintf(w, "✗ %s\t-\t-\t-\t-\t-\n", r.Name)
			continue
		}

		if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
			fmt.Fprintf(w, "✗ %s\tnot a git repo\t-\t-\t-\t-\n", r.Name)
			continue
		}

		branch := currentBranch(dir)
		ahead, behind := aheadBehind(dir)
		dirty := isDirty(dir)
		envPresent := fileExists(filepath.Join(dir, ".env"))
		misePresent := hasMiseConfig(dir)

		glyph := "✓"
		if dirty || ahead > 0 || behind > 0 {
			glyph = "~"
		}

		ab := fmt.Sprintf("%d/%d", ahead, behind)
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\t%s\n",
			glyph, r.Name, branch, ab,
			yesNo(dirty), yesNo(envPresent), yesNo(misePresent))
	}
	w.Flush()

	// ── Sync-folder table ────────────────────────────────────────────────────
	if len(m.Sync) > 0 {
		fmt.Println()
		sw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(sw, "SYNC FOLDER\tLAST SYNC")
		for _, s := range m.Sync {
			fmt.Fprintf(sw, "%s\t%s\n", s.Name, readLastSync(s.Name))
		}
		sw.Flush()
	}

	return 0
}

// aheadBehind returns (ahead, behind) counts vs the upstream branch.
// Returns (0, 0) if there is no upstream or on error.
func aheadBehind(dir string) (ahead, behind int) {
	out, err := exec.Command("git", "-C", dir,
		"rev-list", "--left-right", "--count", "@{u}...HEAD").Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, 0
	}
	// Output is: "<behind>\t<ahead>" (left = upstream-only = behind; right = HEAD-only = ahead)
	behind, _ = strconv.Atoi(parts[0])
	ahead, _ = strconv.Atoi(parts[1])
	return ahead, behind
}

// isDirty returns true if the working tree has uncommitted changes.
func isDirty(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// fileExists returns true if path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// yesNo converts a bool to "yes" or "-".
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "-"
}

// readLastSync reads the lastSync timestamp from the sync-state file for name.
// Returns "never" if the file is absent or unparseable.
func readLastSync(name string) string {
	stateDir := SyncStateDir
	if stateDir == "" {
		stateDir = manifest.ExpandPath("~/.config/caravan/sync-state")
	}
	stateFile := filepath.Join(stateDir, name+".json")

	data, err := os.ReadFile(stateFile)
	if err != nil {
		return "never"
	}

	var s syncState
	if err := json.Unmarshal(data, &s); err != nil || s.LastSync == 0 {
		return "never"
	}

	return time.Unix(0, s.LastSync).Local().Format(time.RFC3339)
}
