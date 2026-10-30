package syncengine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SystemdUnitDir is the directory where systemd user unit files are written.
// Empty means the default ~/.config/systemd/user is resolved lazily.
// Tests override this to a t.TempDir().
var SystemdUnitDir = ""

// systemctlRun is the function used to invoke systemctl. Tests replace it.
var systemctlRun = func(args ...string) ([]byte, error) {
	return runCommand("systemctl", append([]string{"--user"}, args...)...)
}

// loginctlRun is the function used to invoke loginctl. Tests replace it.
var loginctlRun = func(args ...string) ([]byte, error) {
	return runCommand("loginctl", args...)
}

// systemdSupervisor drives `systemctl --user`. User units (not system units)
// are the right level: the sync runs as the developer, reads their ssh agent
// and ~/.config/caravan, and needs no root. The one consequence is that a user
// manager only exists while the user is logged in — Start warns when lingering
// is off, since without it the daemon dies at logout instead of surviving a
// reboot the way the launchd equivalent does.
type systemdSupervisor struct{}

// Label returns the systemd unit name. Dashes rather than dots: dots are legal
// in unit names but the segment after the final dot is the unit *type*, so a
// dotted label reads as a malformed suffix to anyone scanning `systemctl
// --user list-units`.
func (systemdSupervisor) Label(entry string) string {
	return "caravan-sync-" + entry + ".service"
}

func (s systemdSupervisor) UnitPath(entry string) string {
	return filepath.Join(resolvedSystemdUnitDir(), s.Label(entry))
}

func (systemdSupervisor) UnitKind() string { return "unit" }

// LogPath mirrors the launchd backend's file-based logs rather than sending
// output to the journal. `caravan daemon status` and the docs both point at a
// single file per entry; keeping that true on Linux means one answer to "where
// are the logs" instead of two. The journal still gets the unit's lifecycle
// events either way.
func (systemdSupervisor) LogPath(entry string) string {
	return filepath.Join(resolvedSystemdLogDir(), entry+".log")
}

func (s systemdSupervisor) Render(entry, binPath, manifestPath, interval, logPath string) string {
	return systemdUnit(s.Label(entry), binPath, manifestPath, entry, interval, logPath)
}

func (systemdSupervisor) Reload() error {
	if out, err := systemctlRun("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s systemdSupervisor) Start(entry string) error {
	unit := s.Label(entry)
	if out, err := systemctlRun("enable", "--now", unit); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	warnIfNoLinger()
	return nil
}

func (s systemdSupervisor) Stop(entry string) error {
	unit := s.Label(entry)
	if out, err := systemctlRun("disable", "--now", unit); err != nil {
		return fmt.Errorf("systemctl --user disable --now %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s systemdSupervisor) Probe(entry string) (bool, string) {
	out, err := systemctlRun("show", s.Label(entry), "-p", "ActiveState", "-p", "MainPID", "--value")
	if err != nil {
		return false, ""
	}
	// `--value` prints one value per line, in the order the -p flags were
	// given: ActiveState then MainPID.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return false, ""
	}
	running := strings.TrimSpace(lines[0]) == "active"
	pid := ""
	if len(lines) > 1 {
		if p := strings.TrimSpace(lines[1]); p != "" && p != "0" {
			pid = p
		}
	}
	return running, pid
}

// ── unit generation (pure function, unit-testable) ────────────────────────────

// systemdUnit returns a complete systemd user unit file.
// Pure function — no I/O — so it is straightforward to unit test.
func systemdUnit(label, binPath, manifestPath, entry, interval, logPath string) string {
	args := []string{
		binPath,
		"sync",
		"--watch",
		"--interval", interval,
		"-f", manifestPath,
		entry,
	}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, systemdQuote(a))
	}

	return `[Unit]
Description=caravan sync (` + entry + `)
Documentation=https://github.com/itsmaleen/caravan
# The sync is ssh-driven, so there is nothing to do before the network is up.
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + strings.Join(quoted, " ") + `
# Equivalent of launchd's KeepAlive: come back from crashes, dropped ssh
# multiplexers, and remotes that went to sleep mid-scan.
Restart=always
RestartSec=5
StandardOutput=append:` + systemdEscape(logPath) + `
StandardError=append:` + systemdEscape(logPath) + `

[Install]
WantedBy=default.target
`
}

// systemdQuote renders one ExecStart argument. systemd splits on whitespace
// unless the argument is double-quoted, and expands %-specifiers everywhere,
// so both need handling before a path with a space or a percent reaches it.
func systemdQuote(arg string) string {
	escaped := systemdEscape(arg)
	if arg == "" {
		return `""`
	}
	if strings.ContainsAny(arg, " \t\"'\\") {
		return `"` + escaped + `"`
	}
	return escaped
}

// systemdEscape escapes the characters systemd treats specially inside a unit
// value: backslash, double quote, and the %-specifier introducer.
func systemdEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return s
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolvedSystemdUnitDir returns the effective systemd user unit directory.
func resolvedSystemdUnitDir() string {
	if SystemdUnitDir != "" {
		return SystemdUnitDir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "systemd", "user")
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

// resolvedSystemdLogDir returns the directory for per-entry log files.
func resolvedSystemdLogDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "caravan", "logs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "caravan", "logs")
	}
	return filepath.Join(home, ".local", "state", "caravan", "logs")
}

// warnIfNoLinger prints a note when the user manager will not survive logout.
// Without lingering, a `caravan daemon install` looks like it worked and then
// silently stops syncing the moment the session ends — the exact failure the
// daemon exists to prevent. Enabling it needs polkit, so this reports rather
// than tries.
func warnIfNoLinger() {
	user := os.Getenv("USER")
	if user == "" {
		return
	}
	out, err := loginctlRun("show-user", user, "-p", "Linger", "--value")
	if err != nil {
		// loginctl missing or unreadable — say nothing rather than cry wolf.
		return
	}
	if strings.TrimSpace(string(out)) == "yes" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"daemon: note — lingering is off for %s, so this unit stops at logout.\n"+
			"        enable it with: sudo loginctl enable-linger %s\n", user, user)
}
