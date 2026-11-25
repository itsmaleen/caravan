package gitremote

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

const caravanHookMarker = "# caravan guard-prepush hook"

// CmdRemote dispatches caravan remote <subcommand>.
func CmdRemote(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: caravan remote <setup|status> [repo] [-f MANIFEST]")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "setup":
		return cmdRemoteSetup(rest)
	case "status":
		return cmdRemoteStatus(rest)
	default:
		fmt.Fprintf(os.Stderr, "caravan remote: unknown subcommand %q\n", sub)
		return 2
	}
}

func cmdRemoteSetup(args []string) int {
	fs := flag.NewFlagSet("remote setup", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	repoFilter := ""
	if len(positionals) > 0 {
		repoFilter = positionals[0]
	}

	m, err := manifest.Load(manifest.ResolvePath(*f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tPRIVATE REMOTE\tGLOBS\tHOOK")

	anyFailed := false
	for _, r := range m.Repos {
		if repoFilter != "" && r.Name != repoFilter {
			continue
		}
		if !r.RoutingEnabled() {
			continue
		}
		dir := m.RepoDir(r)
		hookStatus, failed := setupRepo(r, dir)
		if failed {
			anyFailed = true
		}
		globs := strings.Join(r.PrivateBranchGlobs(), ",")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.PrivateRemote, globs, hookStatus)
	}
	w.Flush()
	if anyFailed {
		return 1
	}
	return 0
}

// setupRepo configures the private remote and installs the hook for one repo.
// Returns a hook status string and whether an error occurred.
func setupRepo(r manifest.Repo, dir string) (hookStatus string, failed bool) {
	// 1. Add/update the private remote (idempotent).
	if err := ensureRemote(dir, r.PrivateRemote, r.PrivateURL); err != nil {
		fmt.Fprintf(os.Stderr, "  [%s] remote: %v\n", r.Name, err)
		return "error", true
	}

	// 2. Install the pre-push hook.
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := installHook(hookPath); err != nil {
		fmt.Fprintf(os.Stderr, "  [%s] hook: %v\n", r.Name, err)
		return "error", true
	}
	return "✓", false
}

// ensureRemote adds the remote if absent, or updates its URL if it already exists.
func ensureRemote(dir, name, url string) error {
	// Check if remote already exists.
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", name).Output()
	if err == nil {
		existing := strings.TrimSpace(string(out))
		if existing == url {
			return nil // already correct
		}
		// Update URL.
		cmd := exec.Command("git", "-C", dir, "remote", "set-url", name, url)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("set-url: %w: %s", err2, strings.TrimSpace(string(out2)))
		}
		return nil
	}
	// Add remote.
	cmd := exec.Command("git", "-C", dir, "remote", "add", name, url)
	if out2, err2 := cmd.CombinedOutput(); err2 != nil {
		return fmt.Errorf("add: %w: %s", err2, strings.TrimSpace(string(out2)))
	}
	return nil
}

// hookScript is the pre-push hook content we install.
func hookScript() string {
	return `#!/bin/sh
` + caravanHookMarker + `
caravan guard-prepush "$1" "$2" < /dev/stdin
`
}

// installHook installs (or updates) the pre-push hook at hookPath.
// If a non-Caravan hook already exists, it is backed up and chain-called.
func installHook(hookPath string) error {
	existing, err := os.ReadFile(hookPath)
	if err == nil {
		// Hook file already exists.
		content := string(existing)
		if strings.Contains(content, caravanHookMarker) {
			// Already ours: re-write to ensure it's current.
			return writeHook(hookPath, hookScript())
		}
		// Back up the existing hook.
		bakPath := hookPath + ".caravan-bak"
		if err2 := os.WriteFile(bakPath, existing, 0o755); err2 != nil {
			return fmt.Errorf("backing up existing hook: %w", err2)
		}
		// Write a combined hook that calls ours then the backup.
		combined := `#!/bin/sh
` + caravanHookMarker + `
# Caravan pre-push guard (original hook backed up to pre-push.caravan-bak)
caravan guard-prepush "$1" "$2" < /dev/stdin
rc=$?
if [ -x "$(dirname "$0")/pre-push.caravan-bak" ]; then
  "$(dirname "$0")/pre-push.caravan-bak" "$@"
  [ $? -ne 0 ] && rc=$?
fi
exit $rc
`
		return writeHook(hookPath, combined)
	}
	// No existing hook.
	return writeHook(hookPath, hookScript())
}

func writeHook(hookPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(hookPath, []byte(content), 0o755)
}

func cmdRemoteStatus(args []string) int {
	fs := flag.NewFlagSet("remote status", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	repoFilter := ""
	if len(positionals) > 0 {
		repoFilter = positionals[0]
	}

	m, err := manifest.Load(manifest.ResolvePath(*f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tROUTING\tPRIVATE REMOTE\tGLOBS\tHOOK\tBRANCHES")

	for _, r := range m.Repos {
		if repoFilter != "" && r.Name != repoFilter {
			continue
		}
		dir := m.RepoDir(r)
		routing := "disabled"
		privateRemote := "-"
		globs := "-"
		hookStatus := "-"
		branches := "-"

		if r.RoutingEnabled() {
			routing = "enabled"
			privateRemote = r.PrivateRemote
			globs = strings.Join(r.PrivateBranchGlobs(), ",")

			// Check hook.
			hookPath := filepath.Join(dir, ".git", "hooks", "pre-push")
			if data, err2 := os.ReadFile(hookPath); err2 == nil && strings.Contains(string(data), caravanHookMarker) {
				hookStatus = "✓"
			} else {
				hookStatus = "✗"
			}

			// List local branches classified.
			if out, err2 := exec.Command("git", "-C", dir, "branch", "--format=%(refname:short)").Output(); err2 == nil {
				var pub, priv []string
				for _, b := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					b = strings.TrimSpace(b)
					if b == "" {
						continue
					}
					if r.IsPrivateBranch(b) {
						priv = append(priv, "~"+b)
					} else {
						pub = append(pub, b)
					}
				}
				all := append(pub, priv...)
				branches = strings.Join(all, " ")
				if branches == "" {
					branches = "(none)"
				}
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, routing, privateRemote, globs, hookStatus, branches)
	}
	w.Flush()
	return 0
}
