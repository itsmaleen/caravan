// Package manifest: CmdInit discovers git repos under --root and writes a manifest draft.
package manifest

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// CmdInit walks --root (default ~/code), detects git repos, and writes a manifest.
func CmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	root := fs.String("root", "~/code", "workspace root to scan for git repos")
	force := fs.Bool("force", false, "overwrite existing manifest")
	f := fs.String("f", "", "manifest path (default: ~/.config/caravan/caravan.toml)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	manifestPath := ResolvePath(*f)
	rootExpanded := ExpandPath(*root)

	// Check whether manifest already exists.
	if _, err := os.Stat(manifestPath); err == nil {
		if !*force {
			fmt.Fprintf(os.Stderr, "manifest already exists at %s; use --force to overwrite\n", manifestPath)
			return 1
		}
	}

	// Discover repos.
	repos, err := DiscoverRepos(rootExpanded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error discovering repos: %v\n", err)
		return 1
	}

	// Print discovered table.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tURL\tBRANCH\tPATH")
	for _, r := range repos {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.URL, r.Branch, r.Path)
	}
	w.Flush()

	m := &Manifest{
		Version:   1,
		Workspace: Workspace{Root: *root},
		Repos:     repos,
	}

	if err := Save(manifestPath, m); err != nil {
		fmt.Fprintf(os.Stderr, "error saving manifest: %v\n", err)
		return 1
	}
	fmt.Printf("wrote %s (%d repos)\n", manifestPath, len(repos))
	return 0
}

// DiscoverRepos walks root, finds directories with a .git child, and extracts
// metadata via git. Hidden directories and node_modules are skipped. Repos are
// not recursed into.
func DiscoverRepos(root string) ([]Repo, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}

	var repos []Repo

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip unreadable directories.
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		// Skip hidden dirs and node_modules, but not the root itself.
		base := d.Name()
		if path != root && (strings.HasPrefix(base, ".") || base == "node_modules") {
			return filepath.SkipDir
		}
		// Detect a git repo.
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			url, _ := gitRemoteURL(path)
			branch, _ := gitCurrentBranch(path)

			relPath, _ := filepath.Rel(root, path)
			if relPath == "." {
				relPath = ""
			}

			repos = append(repos, Repo{
				Name:   filepath.Base(path),
				URL:    url,
				Path:   relPath,
				Branch: branch,
			})
			// Don't recurse into the repo.
			return filepath.SkipDir
		}
		return nil
	})
	return repos, err
}

// gitRemoteURL returns the URL of the "origin" remote for the repo at dir.
func gitRemoteURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCurrentBranch returns the current branch name (or HEAD ref) for dir.
func gitCurrentBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
