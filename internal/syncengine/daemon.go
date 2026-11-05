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
	if _, err := activeSupervisor(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
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

// validDaemonName reports whether a sync entry name is safe to use in a
// service label and a file path. Entry names are otherwise free-form, but
// systemd rejects spaces and most punctuation in unit names, and a "/" or ".."
// would put the unit or log file outside its directory.
func validDaemonName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("sync entry name %q cannot be used for a daemon", name)
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("sync entry name %q cannot be used for a daemon: use only letters, digits, '-', '_' and '.'", name)
		}
	}
	return nil
}

// stopForChange stops an installed daemon before its definition is replaced
// or removed. A failed stop is fine when the service is not running anyway
// (never loaded, already stopped); otherwise the caller must not go on, or the
// old process keeps running with nothing on disk describing it.
func stopForChange(sup supervisor, entry string) error {
	err := sup.Stop(entry)
	if err == nil {
		return nil
	}
	if running, _ := sup.Probe(entry); running {
		return err
	}
	return nil
}

// writeFileAtomic writes data to a temporary file beside path and renames it
// into place, so a full disk or an interrupted write never leaves a truncated
// unit behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

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

	// The interval is written into the service definition verbatim, so check
	// it parses the way `sync --interval` will parse it.
	if d, err := time.ParseDuration(*intervalFlag); err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "daemon install: invalid --interval %q (want a duration such as 5s or 1m)\n", *intervalFlag)
		return 2
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon install: cannot determine executable path: %v\n", err)
		return 1
	}

	sup, err := activeSupervisor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon install: %v\n", err)
		return 1
	}
	code := 0

	for _, s := range entries {
		if err := validDaemonName(s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install: %v\n", err)
			code = 1
			continue
		}
		label := sup.Label(s.Name)
		unitPath := sup.UnitPath(s.Name)
		logPath := sup.LogPath(s.Name)

		// Ensure log directory exists.
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: mkdir logs: %v\n", s.Name, err)
			code = 1
			continue
		}

		// If already installed, stop it first (upgrade path) so the running
		// process is not left pinned to the old unit definition. Starting an
		// already-running service does not restart it, so a stop that failed
		// with the service still up would make the upgrade a silent no-op.
		installed := false
		if _, statErr := os.Stat(unitPath); statErr == nil {
			installed = true
			if err := stopForChange(sup, s.Name); err != nil {
				fmt.Fprintf(os.Stderr, "daemon install %s: could not stop the running daemon: %v\n", s.Name, err)
				code = 1
				continue
			}
		}

		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: mkdir %s: %v\n", s.Name, filepath.Dir(unitPath), err)
			code = 1
			continue
		}
		unit := sup.Render(s.Name, binPath, absMpath, *intervalFlag, logPath)
		if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: write %s: %v\n", s.Name, sup.UnitKind(), err)
			if installed {
				// The old definition is untouched; bring the old daemon back.
				_ = sup.Start(s.Name)
			}
			code = 1
			continue
		}

		// systemd needs the new unit file read back before it can be enabled;
		// launchd has no equivalent step and no-ops here.
		if err := sup.Reload(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: %v\n", s.Name, err)
			code = 1
			continue
		}
		if err := sup.Start(s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "daemon install %s: %v\n", s.Name, err)
			code = 1
			continue
		}
		fmt.Printf("daemon: installed %s → %s\n", label, unitPath)
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

	sup, err := activeSupervisor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon uninstall: %v\n", err)
		return 1
	}
	code := 0

	for _, s := range entries {
		if err := validDaemonName(s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "daemon uninstall: %v\n", err)
			code = 1
			continue
		}
		label := sup.Label(s.Name)
		unitPath := sup.UnitPath(s.Name)

		// Stop first. A failure is fine when it was not running anyway, but
		// removing the definition of a daemon that is still running would
		// leave a sync going that nothing can manage.
		if err := stopForChange(sup, s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "daemon uninstall %s: could not stop the running daemon, leaving it installed: %v\n", s.Name, err)
			code = 1
			continue
		}

		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "daemon uninstall %s: remove %s: %v\n", s.Name, sup.UnitKind(), err)
			code = 1
			continue
		}
		if err := sup.Reload(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon uninstall %s: %v\n", s.Name, err)
			code = 1
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

	sup, err := activeSupervisor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon status: %v\n", err)
		return 1
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  NAME\t%s\tRUNNING\tPID\tLAST SYNC\n", strings.ToUpper(sup.UnitKind()))

	for _, s := range entries {
		unitPath := sup.UnitPath(s.Name)

		unitPresent := "✗"
		if _, err := os.Stat(unitPath); err == nil {
			unitPresent = "✓"
		}

		running := "✗"
		pid := "-"
		if isRunning, gotPID := sup.Probe(s.Name); isRunning {
			running = "✓"
			if gotPID != "" {
				pid = gotPID
			}
		}

		lastSync := daemonReadLastSync(s.Name)

		glyph := "~"
		if unitPresent == "✓" && running == "✓" {
			glyph = "✓"
		} else if unitPresent == "✗" {
			glyph = "✗"
		}

		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\n",
			glyph, s.Name, unitPresent, running, pid, lastSync)
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

// ── supervisor abstraction ────────────────────────────────────────────────────

// supervisor is the platform's service manager. launchd on macOS, systemd
// --user on Linux. Everything in the three daemon sub-commands that differs
// between platforms lives behind this interface, so the command flow itself is
// written once.
type supervisor interface {
	// Label is the service identifier as the platform knows it — the launchd
	// label, or the systemd unit name.
	Label(entry string) string
	// UnitPath is the absolute path of the file that defines the service.
	UnitPath(entry string) string
	// UnitKind names that file for humans: "plist" or "unit".
	UnitKind() string
	// LogPath is where the service's stdout+stderr are appended.
	LogPath(entry string) string
	// Render returns the complete text of the service definition file.
	Render(entry, binPath, manifestPath, interval, logPath string) string
	// Reload makes the service manager re-read on-disk definitions. No-op
	// where the manager reads the file at load time (launchd).
	Reload() error
	// Start loads and runs the service, enabling it across reboots.
	Start(entry string) error
	// Stop unloads the service and disables it across reboots.
	Stop(entry string) error
	// Probe reports whether the service is running, and its PID when known.
	Probe(entry string) (running bool, pid string)
}

// DaemonBackend forces a supervisor: "launchd" or "systemd". Empty means pick
// from runtime.GOOS. Tests set this so both backends are exercised on either
// platform.
var DaemonBackend = ""

// activeSupervisor returns the supervisor for this machine, or an error naming
// the unsupported platform.
func activeSupervisor() (supervisor, error) {
	backend := DaemonBackend
	if backend == "" {
		switch runtime.GOOS {
		case "darwin":
			backend = "launchd"
		case "linux":
			backend = "systemd"
		default:
			return nil, fmt.Errorf("no service manager for %s (supported: macOS/launchd, Linux/systemd)", runtime.GOOS)
		}
	}
	switch backend {
	case "launchd":
		return launchdSupervisor{}, nil
	case "systemd":
		return systemdSupervisor{}, nil
	default:
		return nil, fmt.Errorf("unknown daemon backend %q (want launchd or systemd)", backend)
	}
}

// ── launchd supervisor ────────────────────────────────────────────────────────

type launchdSupervisor struct{}

func (launchdSupervisor) Label(entry string) string { return daemonLabel(entry) }

func (l launchdSupervisor) UnitPath(entry string) string {
	return filepath.Join(resolvedLaunchAgentsDir(), l.Label(entry)+".plist")
}

func (launchdSupervisor) UnitKind() string { return "plist" }

func (launchdSupervisor) LogPath(entry string) string { return daemonLogPath(entry) }

func (l launchdSupervisor) Render(entry, binPath, manifestPath, interval, logPath string) string {
	return launchdPlist(l.Label(entry), binPath, manifestPath, entry, interval, logPath)
}

// Reload is a no-op: launchd reads the plist when the job is bootstrapped.
func (launchdSupervisor) Reload() error { return nil }

func (l launchdSupervisor) Start(entry string) error {
	plistPath := l.UnitPath(entry)
	out, err := launchctlRun("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	if err == nil {
		return nil
	}
	outStr := string(out)
	// "already loaded" is success — the job is running, which is the goal.
	if strings.Contains(outStr, "already") || strings.Contains(outStr, "Bootstrap failed") {
		fmt.Printf("daemon: %s already loaded\n", l.Label(entry))
		return nil
	}
	// Fall back to the legacy loader for older macOS.
	if _, loadErr := launchctlRun("load", plistPath); loadErr != nil {
		return fmt.Errorf("launchctl load: %w", loadErr)
	}
	return nil
}

func (l launchdSupervisor) Stop(entry string) error {
	_, err := launchctlRun("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), l.Label(entry)))
	return err
}

func (l launchdSupervisor) Probe(entry string) (bool, string) {
	out, err := launchctlRun("print", fmt.Sprintf("gui/%d/%s", os.Getuid(), l.Label(entry)))
	if err != nil {
		return false, ""
	}
	running := false
	pid := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			pidStr := strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
			if _, parseErr := strconv.Atoi(pidStr); parseErr == nil {
				pid = pidStr
				running = true
			}
		}
		if strings.Contains(line, "state = running") {
			running = true
		}
	}
	return running, pid
}
