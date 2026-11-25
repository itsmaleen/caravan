package gitremote

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// PublishScheduleDir is the directory for scheduled publish records.
// Package-level var so tests can override it.
var PublishScheduleDir = ""

// resolveScheduleDir returns the effective publish-schedule directory:
// the package-level override (for tests), then $CARAVAN_PUBLISH_SCHEDULE_DIR,
// then the default ~/.config/caravan/publish-schedule.
func resolveScheduleDir() string {
	if PublishScheduleDir != "" {
		return PublishScheduleDir
	}
	if env := os.Getenv("CARAVAN_PUBLISH_SCHEDULE_DIR"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "caravan", "publish-schedule")
}

type scheduleRecord struct {
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch"`
	Onto          string    `json:"onto"`
	PublishAt     time.Time `json:"publish_at"`
	PublicRemote  string    `json:"public_remote"`
	PrivateRemote string    `json:"private_remote"`
}

// CmdPublish implements caravan publish.
func CmdPublish(args []string) int {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "repo name (required when multiple repos are configured)")
	onto := fs.String("onto", "main", "public branch to push onto")
	at := fs.String("at", "", "RFC3339 timestamp for scheduled publish")
	force := fs.Bool("force", false, "pass --force to git push")
	runDue := fs.Bool("run-due", false, "execute all schedule records whose publish_at<=now")
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	if *runDue {
		return runDueRecords()
	}

	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "usage: caravan publish <branch> [--repo NAME] [--onto PUBLIC_BRANCH] [--at RFC3339] [--force]")
		return 2
	}
	branch := positionals[0]

	m, err := manifest.Load(manifest.ResolvePath(*f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Find the repo.
	repo, err := findRepo(m, *repoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if !repo.RoutingEnabled() {
		fmt.Fprintf(os.Stderr, "error: repo %q does not have split-remote routing configured\n", repo.Name)
		return 1
	}

	// Parse --at if provided.
	if *at != "" {
		publishAt, parseErr := time.Parse(time.RFC3339, *at)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "error: --at: %v\n", parseErr)
			return 1
		}
		if publishAt.After(time.Now()) {
			// Schedule for later.
			return schedulePublish(repo, branch, *onto, publishAt)
		}
		// Past timestamp — publish immediately.
	}

	// Immediate publish.
	dir := m.RepoDir(*repo)
	return doPublish(dir, repo, branch, *onto, *force)
}

func findRepo(m *manifest.Manifest, name string) (*manifest.Repo, error) {
	if name == "" {
		// If exactly one routing-enabled repo, pick it.
		var candidates []manifest.Repo
		for _, r := range m.Repos {
			if r.RoutingEnabled() {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) == 1 {
			r := candidates[0]
			return &r, nil
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no repos with split-remote routing configured")
		}
		return nil, fmt.Errorf("multiple routing-enabled repos; specify --repo")
	}
	for i := range m.Repos {
		if m.Repos[i].Name == name {
			return &m.Repos[i], nil
		}
	}
	return nil, fmt.Errorf("repo %q not found in manifest", name)
}

// doPublish performs an immediate publish: fetch from private, push to public.
func doPublish(dir string, repo *manifest.Repo, branch, onto string, force bool) int {
	privateRemote := repo.PrivateRemote
	publicRemote := repo.PublicRemoteOr()

	// Step 1: fetch the branch from the private remote.
	fetchCmd := exec.Command("git", "-C", dir, "fetch", privateRemote, branch)
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: git fetch %s %s: %v\n", privateRemote, branch, err)
		return 1
	}

	// Step 2: push to public remote, bypassing the hook via CARAVAN_PUBLISH.
	refspec := fmt.Sprintf("%s/%s:refs/heads/%s", privateRemote, branch, onto)
	pushArgs := []string{"-C", dir, "push"}
	if force {
		pushArgs = append(pushArgs, "--force")
	}
	pushArgs = append(pushArgs, publicRemote, refspec)
	pushCmd := exec.Command("git", pushArgs...)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	pushCmd.Env = append(os.Environ(), "CARAVAN_PUBLISH="+branch)
	if err := pushCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: git push %s %s: %v\n", publicRemote, refspec, err)
		return 1
	}

	fmt.Printf("published %s → %s/%s (via %s)\n", branch, publicRemote, onto, privateRemote)
	return 0
}

// schedulePublish writes a schedule record for a future publish.
func schedulePublish(repo *manifest.Repo, branch, onto string, publishAt time.Time) int {
	schedDir := resolveScheduleDir()
	if err := os.MkdirAll(schedDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: mkdir publish-schedule: %v\n", err)
		return 1
	}

	rec := scheduleRecord{
		Repo:          repo.Name,
		Branch:        branch,
		Onto:          onto,
		PublishAt:     publishAt,
		PublicRemote:  repo.PublicRemoteOr(),
		PrivateRemote: repo.PrivateRemote,
	}

	filename := sanitizeFilename(repo.Name+"__"+branch) + ".json"
	path := filepath.Join(schedDir, filename)

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write schedule: %v\n", err)
		return 1
	}
	fmt.Printf("scheduled: %s/%s → %s at %s\n", repo.Name, branch, onto, publishAt.Format(time.RFC3339))
	return 0
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// runDueRecords scans the publish-schedule dir and fires records whose publish_at<=now.
func runDueRecords() int {
	schedDir := resolveScheduleDir()
	entries, err := os.ReadDir(schedDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no scheduled publishes")
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: reading publish-schedule: %v\n", err)
		return 1
	}

	now := time.Now()
	anyFailed := false

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(schedDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", entry.Name(), err)
			continue
		}
		var rec scheduleRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			fmt.Fprintf(os.Stderr, "warning: parsing %s: %v\n", entry.Name(), err)
			continue
		}
		if rec.PublishAt.After(now) {
			fmt.Printf("pending: %s/%s → %s at %s\n", rec.Repo, rec.Branch, rec.Onto, rec.PublishAt.Format(time.RFC3339))
			continue
		}

		// Find the repo dir in the manifest.
		m, err := manifest.Load(manifest.ResolvePath(""))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: loading manifest: %v\n", err)
			anyFailed = true
			continue
		}
		repoObj, err := findRepo(m, rec.Repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			anyFailed = true
			continue
		}

		dir := m.RepoDir(*repoObj)
		fmt.Printf("firing: %s/%s → %s/%s\n", rec.Repo, rec.Branch, rec.PublicRemote, rec.Onto)
		code := doPublish(dir, repoObj, rec.Branch, rec.Onto, false)
		if code != 0 {
			anyFailed = true
			continue
		}
		// Remove fired record.
		_ = os.Remove(path)
	}

	if anyFailed {
		return 1
	}
	return 0
}
