package syncengine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"caravan/internal/buildinfo"
)

// transportKind identifies the transfer mechanism.
type transportKind int

const (
	transportSSH   transportKind = iota
	transportLocal               // local:<abs-path> — no ssh, direct FS ops
)

// RemoteConn describes and operates on one side of a sync pair.
type RemoteConn struct {
	Kind    transportKind
	Host    string // SSH: user@host; Local: ""
	Root    string // absolute-ish remote root path (may start with ~)
}

// ParseRemote parses a remote spec into a RemoteConn.
//
//   - "local:<abs-path>"  → local transport, useful for mounted volumes and tests
//   - "user@host:path"    → SSH transport
func ParseRemote(spec string) (*RemoteConn, error) {
	if strings.HasPrefix(spec, "local:") {
		root := spec[len("local:"):]
		if root == "" {
			return nil, fmt.Errorf("remote spec %q: local: requires a path", spec)
		}
		return &RemoteConn{Kind: transportLocal, Root: root}, nil
	}

	// Expect user@host:path or host:path
	idx := strings.LastIndex(spec, ":")
	if idx < 0 {
		return nil, fmt.Errorf("remote spec %q: expected user@host:path or local:<path>", spec)
	}
	host := spec[:idx]
	path := spec[idx+1:]
	if host == "" || path == "" {
		return nil, fmt.Errorf("remote spec %q: host and path must be non-empty", spec)
	}
	return &RemoteConn{Kind: transportSSH, Host: host, Root: path}, nil
}

// --- Scan ---

// Scan returns an Entry map for the remote root, applying excludes.
// When hashFiles is true, file content hashes are computed on the remote side.
// For SSH transport it invokes `caravan scan --json` on the remote; on scan
// failure that looks like a missing binary it attempts to bootstrap the remote
// and retries once.
func (r *RemoteConn) Scan(excludes []string, hashFiles bool) (map[string]Entry, error) {
	switch r.Kind {
	case transportLocal:
		return r.scanLocal(excludes, hashFiles)
	case transportSSH:
		return r.scanSSH(excludes, hashFiles, true)
	}
	return nil, fmt.Errorf("unknown transport kind %d", r.Kind)
}

func (r *RemoteConn) scanLocal(excludes []string, hashFiles bool) (map[string]Entry, error) {
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return nil, fmt.Errorf("local remote mkdir %s: %w", r.Root, err)
	}
	entries, _, err := ScanDir(r.Root, excludes, hashFiles)
	return entries, err
}

func (r *RemoteConn) scanSSH(excludes []string, hashFiles bool, allowBootstrap bool) (map[string]Entry, error) {
	excArg := strings.Join(excludes, ",")
	remotePath := shellRemotePath(r.Root)
	var cmd string
	if excArg != "" {
		cmd = fmt.Sprintf(`~/.local/bin/caravan scan --json --exclude %q %s`, excArg, remotePath)
	} else {
		cmd = fmt.Sprintf(`~/.local/bin/caravan scan --json %s`, remotePath)
	}
	if hashFiles {
		cmd += " --hash"
	}

	out, err := exec.Command("ssh", "-o", "BatchMode=yes", r.Host, cmd).Output()
	if err != nil {
		if allowBootstrap && looksLikeMissingBinary(err, out) {
			fmt.Fprintf(os.Stderr, "caravan: remote binary not found on %s; bootstrapping…\n", r.Host)
			if berr := r.bootstrap(); berr != nil {
				return nil, fmt.Errorf("bootstrap %s: %w", r.Host, berr)
			}
			// Also ensure the remote root exists.
			_ = r.mkdirSSH("")
			return r.scanSSH(excludes, hashFiles, false)
		}
		// Try to create the remote root if it doesn't exist and retry once.
		if allowBootstrap && looksLikeMissingDir(err, out) {
			_ = r.mkdirSSH("")
			return r.scanSSH(excludes, hashFiles, false)
		}
		return nil, fmt.Errorf("remote scan on %s: %w", r.Host, err)
	}

	var result ScanResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse remote scan: %w", err)
	}

	// Version handshake: if the remote binary is a different version, re-push
	// it and rescan once (allowBootstrap guards against infinite recursion).
	if result.Version != buildinfo.Version {
		remoteVer := result.Version
		if remoteVer == "" {
			remoteVer = "pre-0.1.1"
		}
		fmt.Fprintf(os.Stderr, "remote caravan %s != local %s; updating…\n", remoteVer, buildinfo.Version)
		if allowBootstrap {
			if berr := r.bootstrap(); berr != nil {
				fmt.Fprintf(os.Stderr, "caravan: version update failed: %v; proceeding\n", berr)
			} else {
				return r.scanSSH(excludes, hashFiles, false)
			}
		} else {
			fmt.Fprintf(os.Stderr, "caravan: remote version still mismatched after bootstrap; proceeding\n")
		}
	}

	if result.Entries == nil {
		result.Entries = map[string]Entry{}
	}
	return result.Entries, nil
}

func looksLikeMissingBinary(err error, out []byte) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 127 {
			return true
		}
		combined := strings.ToLower(string(ee.Stderr) + string(out))
		// "flag provided but not defined": the remote binary is an older
		// version that lacks a flag we now send (e.g. --hash) — it needs the
		// same re-push as a missing binary, and the version handshake can't
		// catch this case because the scan never succeeds.
		return strings.Contains(combined, "not found") ||
			strings.Contains(combined, "no such file") ||
			strings.Contains(combined, "flag provided but not defined")
	}
	return false
}

func looksLikeMissingDir(err error, _ []byte) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		combined := strings.ToLower(string(ee.Stderr))
		return strings.Contains(combined, "no such file") || strings.Contains(combined, "not a directory") || ee.ExitCode() == 1
	}
	return false
}

// bootstrap copies the current executable to ~/.local/bin/caravan on the remote.
func (r *RemoteConn) bootstrap() error {
	// Verify architecture matches.
	uname, err := exec.Command("ssh", "-o", "BatchMode=yes", r.Host, "uname -sm").Output()
	if err != nil {
		return fmt.Errorf("uname check: %w", err)
	}
	localPlatform := localUnameSM()
	remotePlatform := strings.TrimSpace(string(uname))
	if !strings.EqualFold(localPlatform, remotePlatform) {
		return fmt.Errorf("platform mismatch: local=%q remote=%q; cannot bootstrap", localPlatform, remotePlatform)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exeFile, err := os.Open(exe)
	if err != nil {
		return fmt.Errorf("open executable: %w", err)
	}
	defer exeFile.Close()

	install := exec.Command("ssh", "-o", "BatchMode=yes", r.Host,
		`mkdir -p ~/.local/bin && cat > ~/.local/bin/caravan && chmod +x ~/.local/bin/caravan`)
	install.Stdin = exeFile
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	fmt.Fprintf(os.Stderr, "caravan: bootstrapped remote binary on %s\n", r.Host)
	return nil
}

func localUnameSM() string {
	goos := runtime.GOOS
	arch := runtime.GOARCH
	// Map Go GOOS/GOARCH → uname -sm output (e.g. "Darwin arm64").
	osMap := map[string]string{
		"darwin":  "Darwin",
		"linux":   "Linux",
		"freebsd": "FreeBSD",
	}
	archMap := map[string]string{
		"amd64": "x86_64",
		"arm64": "arm64",
		"386":   "i386",
	}
	uname := goos
	if v, ok := osMap[goos]; ok {
		uname = v
	}
	a := arch
	if v, ok := archMap[arch]; ok {
		a = v
	}
	return uname + " " + a
}

// shellRemotePath converts a remote path so it is safe inside a double-quoted
// shell argument while still allowing $HOME expansion.
// "~/foo" → "$HOME/foo"; "/abs" → "/abs" (unquoted later by caller wrapping in "…").
func shellRemotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME/` + p[2:] + `"`
	}
	return `"` + p + `"`
}

// absoluteRemotePath replaces the ~ prefix with $HOME for shell-expanded contexts.
func absoluteRemotePath(root, rel string) string {
	base := root
	if rel != "" && rel != "." {
		base = root + "/" + rel
	}
	return base
}

// --- Mkdir ---

// MkdirAll creates a directory (and parents) under the remote root.
// rel="" creates the root itself.
func (r *RemoteConn) MkdirAll(rel string) error {
	switch r.Kind {
	case transportLocal:
		target := r.Root
		if rel != "" {
			target = filepath.Join(r.Root, filepath.FromSlash(rel))
		}
		return os.MkdirAll(target, 0o755)
	case transportSSH:
		return r.mkdirSSH(rel)
	}
	return nil
}

func (r *RemoteConn) mkdirSSH(rel string) error {
	target := absoluteRemotePath(r.Root, rel)
	cmd := fmt.Sprintf("mkdir -p %s", shellRemotePath(target))
	return exec.Command("ssh", "-o", "BatchMode=yes", r.Host, cmd).Run()
}

// --- Push (local → remote) ---

// Push copies files from localRoot to the remote root using tar (SSH) or
// direct file copies (local:).  paths is relative to both roots.
func (r *RemoteConn) Push(localRoot string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	switch r.Kind {
	case transportLocal:
		return r.pushLocal(localRoot, paths)
	case transportSSH:
		return r.pushSSH(localRoot, paths)
	}
	return nil
}

func (r *RemoteConn) pushLocal(localRoot string, paths []string) error {
	for _, p := range paths {
		src := filepath.Join(localRoot, filepath.FromSlash(p))
		dst := filepath.Join(r.Root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("push %s: %w", p, err)
		}
	}
	return nil
}

func (r *RemoteConn) pushSSH(localRoot string, paths []string) error {
	// Write the file list to a temp file.
	tmpList, err := os.CreateTemp("", "caravan-push-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpList.Name())
	for _, p := range paths {
		if _, err := fmt.Fprintln(tmpList, p); err != nil {
			tmpList.Close()
			return err
		}
	}
	if err := tmpList.Close(); err != nil {
		return err
	}

	remoteRoot := shellRemotePath(r.Root)
	sshScript := fmt.Sprintf(`mkdir -p %s && tar -C %s -xpf -`, remoteRoot, remoteRoot)

	tarCmd := exec.Command("tar", "-C", localRoot, "-cf", "-", "-T", tmpList.Name())
	sshCmd := exec.Command("ssh", "-o", "BatchMode=yes", r.Host, sshScript)

	pr, pw := io.Pipe()
	tarCmd.Stdout = pw
	tarCmd.Stderr = os.Stderr
	sshCmd.Stdin = pr
	sshCmd.Stderr = os.Stderr

	if err := sshCmd.Start(); err != nil {
		pw.Close()
		return fmt.Errorf("ssh push: start ssh: %w", err)
	}
	if err := tarCmd.Start(); err != nil {
		pw.CloseWithError(err)
		sshCmd.Wait() //nolint
		return fmt.Errorf("ssh push: start tar: %w", err)
	}

	var tarErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer pw.Close()
		tarErr = tarCmd.Wait()
	}()

	sshErr := sshCmd.Wait()
	wg.Wait()

	if tarErr != nil {
		return fmt.Errorf("ssh push tar: %w", tarErr)
	}
	if sshErr != nil {
		return fmt.Errorf("ssh push: %w", sshErr)
	}
	return nil
}

// --- Pull (remote → local) ---

// Pull fetches files from the remote root to localRoot.
func (r *RemoteConn) Pull(localRoot string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	switch r.Kind {
	case transportLocal:
		return r.pullLocal(localRoot, paths)
	case transportSSH:
		return r.pullSSH(localRoot, paths)
	}
	return nil
}

func (r *RemoteConn) pullLocal(localRoot string, paths []string) error {
	for _, p := range paths {
		src := filepath.Join(r.Root, filepath.FromSlash(p))
		dst := filepath.Join(localRoot, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("pull %s: %w", p, err)
		}
	}
	return nil
}

func (r *RemoteConn) pullSSH(localRoot string, paths []string) error {
	listData := strings.Join(paths, "\n") + "\n"
	remoteRoot := shellRemotePath(r.Root)
	sshScript := fmt.Sprintf(
		`cd %s && cat > /tmp/caravan-list-%d && tar -cf - -T /tmp/caravan-list-%d && rm -f /tmp/caravan-list-%d`,
		remoteRoot, os.Getpid(), os.Getpid(), os.Getpid(),
	)

	sshCmd := exec.Command("ssh", "-o", "BatchMode=yes", r.Host, sshScript)
	sshCmd.Stdin = strings.NewReader(listData)
	sshCmd.Stderr = os.Stderr

	tarCmd := exec.Command("tar", "-C", localRoot, "-xpf", "-")
	tarCmd.Stderr = os.Stderr

	pr, pw := io.Pipe()
	sshCmd.Stdout = pw
	tarCmd.Stdin = pr

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("ssh pull: start tar: %w", err)
	}

	var sshErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer pw.Close()
		sshErr = sshCmd.Run()
	}()

	tarErr := tarCmd.Wait()
	wg.Wait()

	if tarErr != nil {
		return fmt.Errorf("ssh pull tar: %w", tarErr)
	}
	if sshErr != nil {
		return fmt.Errorf("ssh pull: %w", sshErr)
	}
	return nil
}

// --- Delete ---

// DeleteFiles removes files (not dirs) under the remote root.
func (r *RemoteConn) DeleteFiles(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	switch r.Kind {
	case transportLocal:
		for _, p := range paths {
			if err := os.Remove(filepath.Join(r.Root, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	case transportSSH:
		return r.deleteSSH(paths, false)
	}
	return nil
}

// DeleteDir removes a directory (recursively) under the remote root.
func (r *RemoteConn) DeleteDir(path string) error {
	switch r.Kind {
	case transportLocal:
		return os.RemoveAll(filepath.Join(r.Root, filepath.FromSlash(path)))
	case transportSSH:
		return r.deleteSSH([]string{path}, true)
	}
	return nil
}

func (r *RemoteConn) deleteSSH(paths []string, recursive bool) error {
	var args []string
	if recursive {
		args = append(args, "-rf")
	}
	// Paths must not contain single quotes (guaranteed by ScanDir skip).
	for _, p := range paths {
		abs := absoluteRemotePath(r.Root, p)
		if strings.HasPrefix(abs, "~/") {
			abs = `"$HOME/` + abs[2:] + `"`
		} else if abs == "~" {
			abs = `"$HOME"`
		} else {
			abs = `'` + abs + `'`
		}
		args = append(args, abs)
	}
	cmd := "rm " + strings.Join(args, " ")
	return exec.Command("ssh", "-o", "BatchMode=yes", r.Host, cmd).Run()
}

// --- Helpers ---

// copyFile copies src to dst, preserving mtime.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	// Preserve mtime.
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}
