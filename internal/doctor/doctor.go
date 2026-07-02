// Package doctor implements the `caravan doctor` diagnostics command.
// It runs a suite of cheap, read-only checks and renders the results as a
// tabwriter table with ✓/~/✗/- glyphs, consistent with other caravan commands.
package doctor

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"caravan/internal/buildinfo"
	"caravan/internal/cliargs"
	"caravan/internal/manifest"
	"caravan/internal/secrets"
	"caravan/internal/syncengine"
)

// ── tunables / overrides (for tests) ─────────────────────────────────────────

// StateDirOverride, when non-empty, replaces ~/.config/caravan/sync-state as
// the directory where state JSON files and lock files are read.
// Tests set this to a t.TempDir().
var StateDirOverride = ""

// LaunchAgentsDirOverride, when non-empty, replaces ~/Library/LaunchAgents as
// the directory where daemon plist files are checked.
// Tests set this to a t.TempDir().
var LaunchAgentsDirOverride = ""

// runCmd is the exec hook: tests replace this to avoid real subprocess calls.
// The default executes the named binary with combined stdout+stderr output.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// ── result types ──────────────────────────────────────────────────────────────

const (
	statusOK   = "✓"
	statusWarn = "~"
	statusFail = "✗"
	statusNA   = "-"
)

// result holds a single check row for the output table.
type result struct {
	section string
	name    string
	status  string
	detail  string
}

// ── CmdDoctor is the exported entry-point ─────────────────────────────────────

// CmdDoctor implements `caravan doctor [-f MANIFEST]`.
// Returns 0 if all checks pass (✓ or ~), 1 if any check is ✗, 2 on usage error.
func CmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	f := fs.String("f", "", "manifest path")
	if _, err := cliargs.ParseAnywhere(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var results []result

	// ── environment checks (always run) ──────────────────────────────────────
	results = append(results, checkEnv()...)

	// ── manifest check ────────────────────────────────────────────────────────
	manifestPath := manifest.ResolvePath(*f)
	mRes, m := checkManifest(manifestPath)
	results = append(results, mRes)

	// ── manifest-dependent checks (skip if manifest failed) ───────────────────
	if m != nil {
		results = append(results, checkRepos(m)...)
		results = append(results, checkSync(m)...)
		results = append(results, checkSecrets(m)...)
	}

	render(results)

	for _, r := range results {
		if r.status == statusFail {
			return 1
		}
	}
	return 0
}

// ── render ────────────────────────────────────────────────────────────────────

// renderTo writes the results table to the given tabwriter and flushes it.
// It is separate from render so tests can capture output.
func renderTo(w *tabwriter.Writer, results []result) {
	fmt.Fprintln(w, "SECTION\tCHECK\tSTATUS\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.section, r.name, r.status, r.detail)
	}
	w.Flush()
}

func render(results []result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	renderTo(w, results)
}

// ── environment checks ────────────────────────────────────────────────────────

func checkEnv() []result {
	return []result{
		checkBinaryInPath("environment", "git", "git", "--version"),
		checkBinaryInPath("environment", "ssh", "ssh", "-V"),
		checkBinaryInPath("environment", "rsync", "rsync", "--version"),
		{
			section: "environment",
			name:    "caravan version",
			status:  statusOK,
			detail:  buildinfo.Version,
		},
	}
}

// checkBinaryInPath checks whether a binary is in PATH by running it with a
// version flag. It uses only the first output line as the detail string.
func checkBinaryInPath(section, label, binary string, versionArgs ...string) result {
	out, err := runCmd(binary, versionArgs...)
	if err != nil {
		// Some tools (ssh -V) write to stderr and exit non-zero but still work;
		// if we got output, use the first non-empty line as success indicator.
		if len(out) > 0 && !isNotFound(err) {
			return result{section, label, statusOK, firstLine(string(out))}
		}
		if isNotFound(err) {
			return result{section, label, statusFail, binary + " not found in PATH"}
		}
		return result{section, label, statusFail, err.Error()}
	}
	detail := firstLine(string(out))
	if detail == "" {
		detail = binary + " ok"
	}
	return result{section, label, statusOK, detail}
}

// isNotFound returns true when the error indicates the binary was not found.
func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "executable file not found")
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

// ── manifest check ────────────────────────────────────────────────────────────

func checkManifest(path string) (result, *manifest.Manifest) {
	m, err := manifest.Load(path)
	if err != nil {
		return result{
			section: "environment",
			name:    "manifest",
			status:  statusFail,
			detail:  fmt.Sprintf("%s — %v", path, err),
		}, nil
	}
	return result{
		section: "environment",
		name:    "manifest",
		status:  statusOK,
		detail:  path,
	}, m
}

// ── repos checks ──────────────────────────────────────────────────────────────

func checkRepos(m *manifest.Manifest) []result {
	var out []result
	for _, r := range m.Repos {
		out = append(out, checkRepo(m, r))
	}
	return out
}

func checkRepo(m *manifest.Manifest, r manifest.Repo) result {
	dir := m.RepoDir(r)
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return result{"repos", r.Name, statusFail, "directory missing — run caravan up"}
	}
	if err != nil {
		return result{"repos", r.Name, statusFail, err.Error()}
	}
	if !info.IsDir() {
		return result{"repos", r.Name, statusFail, dir + " is not a directory"}
	}
	// Check .git presence.
	if _, gitErr := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(gitErr) {
		return result{"repos", r.Name, statusWarn, dir + " exists but is not a git repo (occupied)"}
	}
	return result{"repos", r.Name, statusOK, dir}
}

// ── sync checks ───────────────────────────────────────────────────────────────

func checkSync(m *manifest.Manifest) []result {
	var out []result
	for _, s := range m.Sync {
		out = append(out, checksForSyncEntry(s)...)
	}
	return out
}

// parseRemoteTransport returns "local" or "ssh" and the relevant fields, without
// importing unexported syncengine transport constants. We mirror ParseRemote logic.
func parseRemoteTransport(spec string) (transport, host, root string, err error) {
	if strings.HasPrefix(spec, "local:") {
		root = spec[len("local:"):]
		if root == "" {
			return "", "", "", fmt.Errorf("remote spec %q: local: requires a path", spec)
		}
		return "local", "", root, nil
	}
	// SSH: user@host:path or host:path
	idx := strings.LastIndex(spec, ":")
	if idx < 0 {
		return "", "", "", fmt.Errorf("remote spec %q: expected user@host:path or local:<path>", spec)
	}
	host = spec[:idx]
	root = spec[idx+1:]
	if host == "" || root == "" {
		return "", "", "", fmt.Errorf("remote spec %q: host and path must be non-empty", spec)
	}
	return "ssh", host, root, nil
}

func checksForSyncEntry(s manifest.Sync) []result {
	label := "sync/" + s.Name
	var out []result

	// local dir exists
	localDir := manifest.ExpandPath(s.Local)
	if _, err := os.Stat(localDir); os.IsNotExist(err) {
		out = append(out, result{label, "local dir", statusFail, localDir + " does not exist"})
	} else {
		out = append(out, result{label, "local dir", statusOK, localDir})
	}

	// remote checks
	transport, host, root, err := parseRemoteTransport(s.Remote)
	if err != nil {
		out = append(out, result{label, "remote", statusFail, "cannot parse remote spec: " + err.Error()})
	} else {
		switch transport {
		case "local":
			out = append(out, checkLocalRemote(label, root)...)
		case "ssh":
			out = append(out, checkSSHRemote(label, host, root)...)
		default:
			out = append(out, result{label, "remote", statusFail, "unknown transport"})
		}
	}

	// state file
	out = append(out, checkStateFile(label, s.Name))

	// lock
	out = append(out, checkLock(label, s.Name))

	// conflicts dir
	out = append(out, checkConflicts(label, s.Name))

	// daemon plist
	out = append(out, checkDaemonPlist(label, s.Name))

	return out
}

// checkLocalRemote checks whether a local: remote target exists or is creatable.
func checkLocalRemote(label, root string) []result {
	expanded := manifest.ExpandPath(root)
	info, err := os.Stat(expanded)
	if os.IsNotExist(err) {
		parent := filepath.Dir(expanded)
		if _, perr := os.Stat(parent); perr == nil {
			return []result{{label, "remote", statusWarn, "local:" + expanded + " does not exist (will be created on first sync)"}}
		}
		return []result{{label, "remote", statusFail, "local:" + expanded + " does not exist and parent is missing"}}
	}
	if err != nil {
		return []result{{label, "remote", statusFail, err.Error()}}
	}
	if !info.IsDir() {
		return []result{{label, "remote", statusFail, "local:" + expanded + " exists but is not a directory"}}
	}
	return []result{{label, "remote (local transport)", statusOK, expanded}}
}

// sshDoctorArgs mirrors the options from syncengine/remote.go sshBaseArgs()
// with an extra ConnectTimeout for diagnostic use. sshBaseArgs is unexported
// in syncengine, so we duplicate the small list here.
func sshDoctorArgs() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/caravan-ssh-%r@%h-%p",
		"-o", "ControlPersist=60s",
		"-o", "ConnectTimeout=5",
	}
}

// checkSSHRemote checks reachability and remote caravan version via SSH.
func checkSSHRemote(label, host, root string) []result {
	var out []result

	// 1. Reachability: ssh <host> true
	reachArgs := append(sshDoctorArgs(), host, "true")
	_, err := runCmd("ssh", reachArgs...)
	if err != nil {
		out = append(out, result{label, "remote reachable", statusFail,
			fmt.Sprintf("ssh %s unreachable: %v", host, err)})
		// Remote version and remote dir cannot be checked without connectivity.
		out = append(out, result{label, "remote caravan version", statusNA, "skipped (ssh unreachable)"})
		out = append(out, result{label, "remote dir", statusNA, "skipped (ssh unreachable)"})
		return out
	}
	out = append(out, result{label, "remote reachable", statusOK, host})

	// 2. Remote caravan version.
	verArgs := append(sshDoctorArgs(), host, "~/.local/bin/caravan version")
	verOut, verErr := runCmd("ssh", verArgs...)
	remoteVersion := strings.TrimSpace(string(verOut))
	remoteVersion = strings.TrimPrefix(remoteVersion, "caravan ")
	if verErr != nil || remoteVersion == "" {
		out = append(out, result{label, "remote caravan version", statusWarn,
			"caravan not found on remote — bootstraps on next sync"})
	} else if remoteVersion == buildinfo.Version {
		out = append(out, result{label, "remote caravan version", statusOK, remoteVersion})
	} else {
		out = append(out, result{label, "remote caravan version", statusWarn,
			fmt.Sprintf("remote=%s local=%s — will self-update on next sync", remoteVersion, buildinfo.Version)})
	}

	// 3. Remote dir exists/creatable.
	// Build a shell snippet that checks the directory on the remote side.
	remoteCheckDir := root
	if remoteCheckDir == "~" || strings.HasPrefix(remoteCheckDir, "~/") {
		remoteCheckDir = strings.Replace(remoteCheckDir, "~", "$HOME", 1)
	}
	checkScript := fmt.Sprintf(`test -d "%s" || echo MISSING`, remoteCheckDir)
	dirArgs := append(sshDoctorArgs(), host, checkScript)
	dirOut, dirErr := runCmd("ssh", dirArgs...)
	if dirErr != nil {
		out = append(out, result{label, "remote dir", statusWarn, "could not check remote dir: " + dirErr.Error()})
	} else if strings.Contains(string(dirOut), "MISSING") {
		out = append(out, result{label, "remote dir", statusWarn, root + " does not exist (will be created on first sync)"})
	} else {
		out = append(out, result{label, "remote dir", statusOK, root})
	}

	return out
}

// ── state file check ──────────────────────────────────────────────────────────

// resolvedStateDir returns the effective state directory, consulting
// StateDirOverride, then syncengine.StateDir, then the production default.
func resolvedStateDir() string {
	if StateDirOverride != "" {
		return StateDirOverride
	}
	if syncengine.StateDir != "" {
		return syncengine.StateDir
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".caravan-sync-state"
	}
	return filepath.Join(h, ".config", "caravan", "sync-state")
}

// stateFilePath resolves the path to the state JSON file for a sync name.
func stateFilePath(name string) string {
	return filepath.Join(resolvedStateDir(), name+".json")
}

// stateFileStruct mirrors the subset of syncengine.State we need for diagnostics.
type stateFileStruct struct {
	LastSync int64 `json:"lastSync"`
}

func checkStateFile(label, name string) result {
	path := stateFilePath(name)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result{label, "state file", statusWarn, "missing — never synced"}
	}
	if err != nil {
		return result{label, "state file", statusFail, "cannot read: " + err.Error()}
	}
	var s stateFileStruct
	if err := json.Unmarshal(data, &s); err != nil {
		return result{label, "state file", statusFail, "corrupt JSON: " + err.Error()}
	}
	if s.LastSync == 0 {
		return result{label, "state file", statusWarn, "present but lastSync is zero — never synced"}
	}
	age := time.Since(time.Unix(0, s.LastSync)).Round(time.Second)
	return result{label, "state file", statusOK, fmt.Sprintf("last sync %s ago", age)}
}

// ── lock check ────────────────────────────────────────────────────────────────

// lockFilePath resolves the path to the advisory lock file for a sync name.
func lockFilePath(name string) string {
	return filepath.Join(resolvedStateDir(), name+".lock")
}

func checkLock(label, name string) result {
	path := lockFilePath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			// The state directory hasn't been created yet — no lock can be held.
			return result{label, "lock", statusOK, "free (no state dir yet)"}
		}
		return result{label, "lock", statusWarn, "cannot check lock: " + err.Error()}
	}
	defer f.Close()

	// Attempt a non-blocking exclusive flock; success means the lock is free.
	flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if flockErr == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return result{label, "lock", statusOK, "free"}
	}
	if errors.Is(flockErr, syscall.EWOULDBLOCK) {
		return result{label, "lock", statusWarn, "held — sync in progress or stale lock"}
	}
	return result{label, "lock", statusWarn, "flock error: " + flockErr.Error()}
}

// ── conflicts dir check ───────────────────────────────────────────────────────

func conflictsDir(name string) string {
	// Mirrors syncengine's resolvedConflictDir: <stateDir>/../conflicts/<name>.
	base := resolvedStateDir()
	return filepath.Join(base, "..", "conflicts", name)
}

func checkConflicts(label, name string) result {
	cdir := conflictsDir(name)
	entries, err := os.ReadDir(cdir)
	if os.IsNotExist(err) {
		return result{label, "conflicts", statusOK, "0 backups"}
	}
	if err != nil {
		return result{label, "conflicts", statusOK, "cannot read conflicts dir: " + err.Error()}
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	detail := fmt.Sprintf("%d backup(s) in %s", count, cdir)
	return result{label, "conflicts", statusOK, detail}
}

// ── daemon plist check ────────────────────────────────────────────────────────

func daemonPlistPath(name string) string {
	laDir := LaunchAgentsDirOverride
	if laDir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			laDir = "LaunchAgents"
		} else {
			laDir = filepath.Join(h, "Library", "LaunchAgents")
		}
	}
	label := "dev.caravan.sync." + name
	return filepath.Join(laDir, label+".plist")
}

func checkDaemonPlist(label, name string) result {
	path := daemonPlistPath(name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return result{label, "daemon plist", statusNA, "not installed"}
	} else if err != nil {
		return result{label, "daemon plist", statusNA, "cannot check: " + err.Error()}
	}
	return result{label, "daemon plist", statusOK, "installed (" + path + ")"}
}

// ── secrets checks ────────────────────────────────────────────────────────────

func checkSecrets(m *manifest.Manifest) []result {
	sp := m.SecretsPath()
	if sp == "" {
		return nil // no secrets configured — skip section entirely
	}

	var out []result

	// Machine key exists?
	kp := secrets.KeyPath
	if kp == "" {
		kp = manifest.ExpandPath("~/.config/caravan/age.key")
	}
	if _, err := os.Stat(kp); os.IsNotExist(err) {
		out = append(out, result{"secrets", "machine key", statusFail,
			kp + " missing — run 'caravan secrets init'"})
	} else {
		out = append(out, result{"secrets", "machine key", statusOK, kp})
	}

	// Store file exists + decryptable?
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		out = append(out, result{"secrets", "store file", statusFail,
			sp + " missing — run 'caravan secrets init'"})
	} else {
		store, err := secrets.LoadStore(sp)
		if err != nil {
			out = append(out, result{"secrets", "store file", statusFail,
				"cannot decrypt: " + err.Error()})
		} else if store == nil {
			out = append(out, result{"secrets", "store file", statusWarn, sp + " empty"})
		} else {
			out = append(out, result{"secrets", "store file", statusOK,
				fmt.Sprintf("%s (%d recipient(s), %d repo(s))", sp, len(store.Recipients), len(store.Repos))})
		}
	}

	return out
}
