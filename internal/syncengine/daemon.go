package syncengine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// runCommand is a thin wrapper around exec.Command so that launchctlRun's
// default implementation stays a simple closure and tests can stub the whole
// thing without importing os/exec.
func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// LaunchAgentsDir is the directory where plist files are written.
// Empty means the default ~/Library/LaunchAgents is resolved lazily.
// Tests override this to a t.TempDir().
var LaunchAgentsDir = ""

// launchctlRun is the function used to invoke launchctl. Tests replace it.
var launchctlRun = func(args ...string) ([]byte, error) {
	// Use os/exec inline to avoid a package-level import cycle.
	// We import os/exec only through this closure so tests can stub easily.
	return runCommand("launchctl", args...)
}

// CmdDaemon implements `caravan daemon <install|uninstall|status> [NAME] [flags]`.
func CmdDaemon(args []string) int {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "daemon: only supported on macOS")
		return 1
	}

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, daemonUsage)
		return 2
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "install":
		return cmdDaemonInstall(rest)
	case "uninstall":
		return cmdDaemonUninstall(rest)
	case "status":
		return cmdDaemonStatus(rest)
	default:
		fmt.Fprintf(os.Stderr, "daemon: unknown sub-command %q\n\n%s\n", sub, daemonUsage)
		return 2
	}
}

const daemonUsage = `Usage:
  caravan daemon install [NAME] [--interval 5s] [-f MANIFEST]
  caravan daemon uninstall [NAME] [-f MANIFEST]
  caravan daemon status [NAME] [-f MANIFEST]`

// ── install ───────────────────────────────────────────────────────────────────

func cmdDaemonInstall(args []string) int {
	fs := flag.NewFlagSet("daemon install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFlag := fs.String("f", "", "manifest path")
	intervalFlag := fs.String("interval", "5s", "sync interval (e.g. 5s, 1m)")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		return 2
	}

	var nameFilter string
	if len(positionals) > 0 {
		nameFilter = positionals[0]
	}

	mpath := manifest.ResolvePath(*manifestFlag)
	absMpath, err := filepath.Abs(mpath)
	if err != nil {
		absMpath = mpath
	}

	m, err := manifest.Load(mpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon install: %v\n", err)
		return 1
	}

	entries := filterEntries(m.Sync, nameFilter)
	if len(entries) == 0 {
		if nameFilter != "" {
			fmt.Fprintf(os.Stderr, "daemon install: no sync entry named %q\n", nameFilter)
		} else {
			fmt.Fprintln(os.Stderr, "daemon install: no [[sync]] entries in manifest")
		}
		return 1
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon install: cannot determine executable path: %v\n", err)
		return 1
	}

	laDir := resolvedLaunchAgentsDir()
	uid := os.Getuid()
	code := 0

	for _, s := range entries {
		label := daemonLabel(s.Name)
		plistPath := filepath.Join(laDir, label+".plist")
		logPath := daemonLogPath(s.Name)

		// Ensure log directory exists.
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: mkdir logs: %v\n", s.Name, err)
			code = 1
			continue
		}

		// If already installed, boot it out first (upgrade path).
		if _, statErr := os.Stat(plistPath); statErr == nil {
			_, _ = launchctlRun("bootout", fmt.Sprintf("gui/%d/%s", uid, label))
		}

		// Write plist.
		plist := launchdPlist(label, binPath, absMpath, s.Name, *intervalFlag, logPath)
		if err := os.MkdirAll(laDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: mkdir LaunchAgents: %v\n", s.Name, err)
			code = 1
			continue
		}
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: write plist: %v\n", s.Name, err)
			code = 1
			continue
		}

		// Bootstrap (preferred on macOS 10.11+).
		out, bootstrapErr := launchctlRun("bootstrap", fmt.Sprintf("gui/%d", uid), plistPath)
		if bootstrapErr != nil {
			outStr := string(out)
			// If already loaded (not a "Bootstrap failed" from a different cause), skip.
			if strings.Contains(outStr, "already") || strings.Contains(outStr, "Bootstrap failed") {
				// Treat as success — already running.
				fmt.Printf("daemon: %s already loaded\n", label)
			} else {
				// Fall back to legacy load.
				if _, loadErr := launchctlRun("load", plistPath); loadErr != nil {
					fmt.Fprintf(os.Stderr, "daemon install %s: launchctl load: %v\n", s.Name, loadErr)
					code = 1
					continue
				}
			}
		}
		fmt.Printf("daemon: installed %s → %s\n", label, plistPath)
	}
	return code
}

// ── uninstall ─────────────────────────────────────────────────────────────────

func cmdDaemonUninstall(args []string) int {
	fs := flag.NewFlagSet("daemon uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFlag := fs.String("f", "", "manifest path")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		return 2
	}

	var nameFilter string
	if len(positionals) > 0 {
		nameFilter = positionals[0]
	}

	m, err := manifest.Load(manifest.ResolvePath(*manifestFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon uninstall: %v\n", err)
		return 1
	}

	entries := filterEntries(m.Sync, nameFilter)
	if len(entries) == 0 {
		if nameFilter != "" {
			fmt.Fprintf(os.Stderr, "daemon uninstall: no sync entry named %q\n", nameFilter)
		} else {
			fmt.Fprintln(os.Stderr, "daemon uninstall: no [[sync]] entries in manifest")
		}
		return 1
	}

	laDir := resolvedLaunchAgentsDir()
	uid := os.Getuid()
	code := 0

	for _, s := range entries {
		label := daemonLabel(s.Name)
		plistPath := filepath.Join(laDir, label+".plist")

		// bootout (ignore errors — may already be stopped).
		_, _ = launchctlRun("bootout", fmt.Sprintf("gui/%d/%s", uid, label))

		// Remove plist.
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "daemon uninstall %s: remove plist: %v\n", s.Name, err)
			code = 1
			continue
		}
		fmt.Printf("daemon: uninstalled %s\n", label)
	}
	return code
}

// ── status ────────────────────────────────────────────────────────────────────

func cmdDaemonStatus(args []string) int {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFlag := fs.String("f", "", "manifest path")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		return 2
	}

	var nameFilter string
	if len(positionals) > 0 {
		nameFilter = positionals[0]
	}

	m, err := manifest.Load(manifest.ResolvePath(*manifestFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon status: %v\n", err)
		return 1
	}

	entries := filterEntries(m.Sync, nameFilter)
	if len(entries) == 0 {
		if nameFilter != "" {
			fmt.Fprintf(os.Stderr, "daemon status: no sync entry named %q\n", nameFilter)
		} else {
			fmt.Fprintln(os.Stderr, "daemon status: no [[sync]] entries in manifest")
		}
		return 1
	}

	laDir := resolvedLaunchAgentsDir()
	uid := os.Getuid()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tPLIST\tRUNNING\tPID\tLAST SYNC")

	for _, s := range entries {
		label := daemonLabel(s.Name)
		plistPath := filepath.Join(laDir, label+".plist")

		plistPresent := "✗"
		if _, err := os.Stat(plistPath); err == nil {
			plistPresent = "✓"
		}

		running := "-"
		pid := "-"
		out, err := launchctlRun("print", fmt.Sprintf("gui/%d/%s", uid, label))
		if err == nil {
			outStr := string(out)
			// Parse "pid = <num>" from launchctl print output.
			for _, line := range strings.Split(outStr, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "pid = ") {
					pidStr := strings.TrimPrefix(line, "pid = ")
					if _, parseErr := strconv.Atoi(strings.TrimSpace(pidStr)); parseErr == nil {
						pid = strings.TrimSpace(pidStr)
						running = "✓"
					}
				}
				// Also accept "state = running"
				if strings.Contains(line, "state = running") {
					running = "✓"
				}
			}
			if running == "-" {
				running = "✗"
			}
		} else {
			running = "✗"
		}

		lastSync := daemonReadLastSync(s.Name)

		glyph := "~"
		if plistPresent == "✓" && running == "✓" {
			glyph = "✓"
		} else if plistPresent == "✗" {
			glyph = "✗"
		}

		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\n",
			glyph, s.Name, plistPresent, running, pid, lastSync)
	}
	w.Flush()
	return 0
}

// daemonReadLastSync reads the lastSync timestamp from the sync state file.
// Returns "never" if absent or unparseable.
func daemonReadLastSync(name string) string {
	stateFile := filepath.Join(resolvedStateDir(), name+".json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return "never"
	}
	var s struct {
		LastSync int64 `json:"lastSync"`
	}
	if err := json.Unmarshal(data, &s); err != nil || s.LastSync == 0 {
		return "never"
	}
	return time.Unix(0, s.LastSync).Local().Format(time.RFC3339)
}

// ── plist generation (pure function, unit-testable) ───────────────────────────

// launchdPlist returns a complete launchd plist XML string.
// This is a pure function — no I/O — so it is straightforward to unit test.
func launchdPlist(label, binPath, manifestPath, entry, interval, logPath string) string {
	args := []string{
		binPath,
		"sync",
		"--watch",
		"--interval", interval,
		"-f", manifestPath,
		entry,
	}
	var argXML strings.Builder
	for _, a := range args {
		argXML.WriteString("\t\t<string>")
		argXML.WriteString(xmlEscape(a))
		argXML.WriteString("</string>\n")
	}

	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(label) + `</string>
	<key>ProgramArguments</key>
	<array>
` + argXML.String() + `	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`
}

// xmlEscape escapes the five predefined XML entities.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// ── helpers ───────────────────────────────────────────────────────────────────

// daemonLabel returns the launchd service label for a sync entry.
func daemonLabel(entryName string) string {
	return "dev.caravan.sync." + entryName
}

// daemonLogPath returns the log file path for a sync entry.
func daemonLogPath(entryName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "Library", "Logs", "caravan", entryName+".log")
}

// resolvedLaunchAgentsDir returns the effective LaunchAgents directory.
func resolvedLaunchAgentsDir() string {
	if LaunchAgentsDir != "" {
		return LaunchAgentsDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "LaunchAgents"
	}
	return filepath.Join(home, "Library", "LaunchAgents")
}

// filterEntries returns all entries if nameFilter is empty, or the single
// matching entry otherwise.
func filterEntries(entries []manifest.Sync, nameFilter string) []manifest.Sync {
	if nameFilter == "" {
		return entries
	}
	for _, s := range entries {
		if s.Name == nameFilter {
			return []manifest.Sync{s}
		}
	}
	return nil
}
