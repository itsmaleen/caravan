// Package gitremote implements the caravan remote, publish, and guard-prepush commands.
package gitremote

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"caravan/internal/manifest"
)

// Decision returns whether a push should be blocked, and the reason.
// branch is the local branch name, targetRemote is the remote being pushed to,
// publicRemote is the configured public remote name, globs are private-branch
// patterns, and publishRef is the value of CARAVAN_PUBLISH env (empty if unset).
func Decision(branch, targetRemote, publicRemote string, globs []string, publishRef string) (block bool, reason string) {
	isPrivate := false
	for _, glob := range globs {
		if manifest.MatchPrivateGlob(glob, branch) {
			isPrivate = true
			break
		}
	}
	if !isPrivate {
		return false, ""
	}
	if targetRemote != publicRemote {
		return false, ""
	}
	// Branch is private and target is public remote: block unless CARAVAN_PUBLISH
	// matches the exact branch.
	if publishRef == branch {
		return false, ""
	}
	return true, fmt.Sprintf(
		"refusing to push private branch %q to public remote %q; use 'caravan publish' to promote it",
		branch, targetRemote,
	)
}

// CmdGuardPrepush is the brain of the pre-push hook. It reads the pre-push
// protocol from stdin and blocks pushes of private branches to the public remote.
func CmdGuardPrepush(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: caravan guard-prepush <remote-name> <remote-url>\n")
		return 2
	}
	targetRemote := args[0]
	// args[1] is the remote URL (unused for routing decisions; we compare by name)

	// Determine enforcement globs and the public remote. If the manifest loads
	// and this repo has routing configured, use its settings. Otherwise FAIL
	// CLOSED against the built-in default private globs: the hook is only ever
	// installed on repos that opted into routing, so a missing/broken manifest
	// (or a repo whose routing was since removed) must not become a hole that
	// lets a private-pattern branch leak to the public remote. Default the
	// public remote to "origin", matching PublicRemoteOr().
	var globs []string
	publicRemote := "origin"
	if _, repo, err := loadManifestAndRepo(); err == nil && repo.RoutingEnabled() {
		globs = repo.PrivateBranchGlobs()
		publicRemote = repo.PublicRemoteOr()
	} else {
		if err != nil {
			fmt.Fprintf(os.Stderr, "caravan guard-prepush: manifest unavailable (%v); failing closed against default private globs %v\n", err, manifest.DefaultPrivateGlobs)
		}
		globs = manifest.DefaultPrivateGlobs
	}

	publishRef := os.Getenv("CARAVAN_PUBLISH")

	scanner := bufio.NewScanner(os.Stdin)
	blocked := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Format: <local-ref> <local-sha> <remote-ref> <remote-sha>
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		localRef := parts[0]
		// Extract branch name from ref (refs/heads/<branch>).
		branch := localRef
		if after, ok := strings.CutPrefix(localRef, "refs/heads/"); ok {
			branch = after
		}

		block, reason := Decision(branch, targetRemote, publicRemote, globs, publishRef)
		if block {
			fmt.Fprintf(os.Stderr, "caravan: %s\n", reason)
			blocked = true
		}
	}
	if blocked {
		return 1
	}
	return 0
}

// loadManifestAndRepo loads the manifest and finds the repo whose directory
// matches the current working directory (symlinks resolved on both sides).
func loadManifestAndRepo() (*manifest.Manifest, *manifest.Repo, error) {
	manifestPath := manifest.ResolvePath("")
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fmt.Errorf("getwd: %w", err)
	}
	// Resolve symlinks so /tmp == /private/tmp on macOS, etc.
	cwdReal, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		cwdReal = cwd // fall back to unresolved
	}
	for i := range m.Repos {
		dir := m.RepoDir(m.Repos[i])
		dirReal, err2 := filepath.EvalSymlinks(dir)
		if err2 != nil {
			dirReal = dir
		}
		if dirReal == cwdReal {
			return m, &m.Repos[i], nil
		}
	}
	// No exact match: routing is not configured for this directory; return a
	// zero repo so the caller gets RoutingEnabled() == false.
	empty := &manifest.Repo{}
	return m, empty, nil
}
