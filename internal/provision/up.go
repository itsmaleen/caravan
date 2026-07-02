// Package provision implements the `caravan up` and `caravan status` commands.
package provision

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
	"caravan/internal/secrets"
)

// CmdUp provisions repos (clone or pull), writes .env from secrets, runs mise.
func CmdUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would happen, touch nothing")
	only := fs.String("only", "", "comma-separated repo names to provision")
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

	// Filter repos if --only is set.
	repos := m.Repos
	if *only != "" {
		onlySet := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			onlySet[strings.TrimSpace(n)] = true
		}
		var filtered []manifest.Repo
		for _, r := range repos {
			if onlySet[r.Name] {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	}

	secretsPath := m.SecretsPath()
	start := time.Now()

	type row struct {
		glyph  string
		name   string
		action string
		branch string
		detail string
	}

	var rows []row
	anyFailed := false

	for _, r := range repos {
		dir := m.RepoDir(r)
		action, branch, detail, failed := processRepo(r, dir, secretsPath, m.Toolchain.Mise, *dryRun)

		glyph := "✓"
		if failed {
			glyph = "✗"
			anyFailed = true
		} else if action == "would clone" || action == "would pull" {
			glyph = "~"
		}
		rows = append(rows, row{glyph, r.Name, action, branch, detail})
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tACTION\tBRANCH\tDETAIL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\n", r.glyph, r.name, r.action, r.branch, r.detail)
	}
	w.Flush()
	fmt.Printf("\ntook %v\n", time.Since(start).Round(time.Millisecond))

	if anyFailed {
		return 1
	}
	return 0
}

// processRepo handles a single repo: clone/pull/skip, secrets, toolchain.
func processRepo(r manifest.Repo, dir, secretsPath string, useMise, dryRun bool) (action, branch, detail string, failed bool) {
	fi, statErr := os.Stat(dir)

	switch {
	case os.IsNotExist(statErr):
		if dryRun {
			return "would clone", "", "", false
		}
		return cloneRepo(r, dir)

	case statErr != nil:
		return "error", "", statErr.Error(), true

	case !fi.IsDir():
		return "error", "", "path occupied (not a directory)", true
	}

	// Check for .git directory.
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		return "error", "", "path occupied (not a git repo)", true
	}

	branch = currentBranch(dir)

	if dryRun {
		return "would pull", branch, "", false
	}

	action, detail, failed = pullRepo(dir)

	// Secrets: write .env even when pull failed — the repo dir exists and
	// secrets/direnv should still be materialised so the developer can work.
	envWritten := false
	if secretsPath != "" {
		env, err := secrets.DecryptRepoEnv(secretsPath, r.Name)
		if err != nil {
			if !failed {
				detail = "[secrets: " + truncate(err.Error(), 80) + "]"
			}
		} else if len(env) > 0 {
			if wErr := writeEnv(dir, env); wErr != nil {
				if !failed {
					detail = "[env: " + wErr.Error() + "]"
				}
			} else {
				envWritten = true
			}
		}
	}

	// direnv: write .envrc containing "dotenv" if .env was written and .envrc absent.
	if envWritten && direnvInPath() {
		envrcPath := filepath.Join(dir, ".envrc")
		if _, err := os.Stat(envrcPath); os.IsNotExist(err) {
			_ = os.WriteFile(envrcPath, []byte("dotenv\n"), 0o644)
		}
	}

	if failed {
		if envWritten {
			detail += "; .env written"
		}
		return action, branch, detail, failed
	}

	// Toolchain: mise install if mise is configured, binary found, and config present.
	if useMise && miseInPath() {
		if hasMiseConfig(dir) {
			cmd := exec.Command("mise", "install")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				if detail != "" {
					detail += " "
				}
				detail += "[mise: " + truncate(strings.TrimSpace(string(out)), 80) + "]"
			}
		}
	}

	return action, branch, detail, false
}

// cloneRepo runs git clone for the given repo.
func cloneRepo(r manifest.Repo, dir string) (action, branch, detail string, failed bool) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "error", "", err.Error(), true
	}

	cloneArgs := []string{"clone"}
	if r.Sparse {
		cloneArgs = append(cloneArgs, "--filter=blob:none", "--sparse")
	}
	if r.Branch != "" {
		cloneArgs = append(cloneArgs, "--branch", r.Branch)
	}
	cloneArgs = append(cloneArgs, r.URL, dir)

	cmd := exec.Command("git", cloneArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "error", "", truncate(strings.TrimSpace(string(out)), 120), true
	}

	if r.Sparse {
		sc := exec.Command("git", "-C", dir, "sparse-checkout", "init", "--cone")
		if scOut, scErr := sc.CombinedOutput(); scErr != nil {
			return "cloned", currentBranch(dir),
				"sparse-checkout init failed: " + truncate(string(bytes.TrimSpace(scOut)), 80), false
		}
	}

	return "cloned", currentBranch(dir), "", false
}

// pullRepo runs git pull --ff-only in dir.
func pullRepo(dir string) (action, detail string, failed bool) {
	cmd := exec.Command("git", "pull", "--ff-only")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "error", "needs attention (" + truncate(strings.TrimSpace(string(out)), 100) + ")", true
	}
	if bytes.Contains(out, []byte("Already up to date")) {
		return "up-to-date", "", false
	}
	return "updated", "", false
}

// currentBranch returns the current branch of the repo at dir, or "?" on error.
func currentBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// writeEnv merges managed key=value pairs into dir/.env.
// Manifest-managed keys win; pre-existing unknown keys are preserved.
// Never writes a file with fewer keys than currently exist.
func writeEnv(dir string, managed map[string]string) error {
	envPath := filepath.Join(dir, ".env")

	var existingLines []string
	if data, err := os.ReadFile(envPath); err == nil {
		existingLines = strings.Split(string(data), "\n")
		// Remove trailing empty element from Split.
		if len(existingLines) > 0 && existingLines[len(existingLines)-1] == "" {
			existingLines = existingLines[:len(existingLines)-1]
		}
	}

	seenKeys := map[string]bool{}
	var outLines []string

	for _, line := range existingLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			outLines = append(outLines, line)
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			outLines = append(outLines, line)
			continue
		}
		key := parts[0]
		seenKeys[key] = true
		if val, ok := managed[key]; ok {
			outLines = append(outLines, key+"="+val)
		} else {
			outLines = append(outLines, line)
		}
	}

	// Append newly-managed keys.
	for k, v := range managed {
		if !seenKeys[k] {
			outLines = append(outLines, k+"="+v)
		}
	}

	// Safety: never write fewer key lines than existed.
	existingKeyCount := countEnvKeys(existingLines)
	newKeyCount := countEnvKeys(outLines)
	if newKeyCount < existingKeyCount {
		return fmt.Errorf("refusing to write .env with %d keys (had %d)", newKeyCount, existingKeyCount)
	}

	content := strings.Join(outLines, "\n") + "\n"
	return os.WriteFile(envPath, []byte(content), 0o600)
}

// countEnvKeys returns the number of KEY=VALUE lines in a slice of .env lines.
func countEnvKeys(lines []string) int {
	n := 0
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.Contains(t, "=") {
			n++
		}
	}
	return n
}

// miseInPath returns true if the mise binary is available.
func miseInPath() bool {
	_, err := exec.LookPath("mise")
	return err == nil
}

// direnvInPath returns true if the direnv binary is available.
func direnvInPath() bool {
	_, err := exec.LookPath("direnv")
	return err == nil
}

// hasMiseConfig returns true if .mise.toml or mise.toml exists in dir.
func hasMiseConfig(dir string) bool {
	for _, name := range []string{".mise.toml", "mise.toml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// truncate clips s to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
