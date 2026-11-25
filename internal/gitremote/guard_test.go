package gitremote_test

import (
	"os"
	"path/filepath"
	"testing"

	"caravan/internal/gitremote"
)

func TestDecision(t *testing.T) {
	globs := []string{"private/*", "embargo/*", "secret/*"}
	const pub = "origin"
	const priv = "private"

	tests := []struct {
		name         string
		branch       string
		targetRemote string
		publicRemote string
		publishRef   string
		wantBlock    bool
	}{
		// Private branch pushed to public remote → block.
		{"private→public block", "private/feature", pub, pub, "", true},
		{"embargo→public block", "embargo/cve-2026-0001", pub, pub, "", true},
		{"secret→public block", "secret/stuff", pub, pub, "", true},

		// Private branch pushed to private remote → allow.
		{"private→private allow", "private/feature", priv, pub, "", false},
		{"embargo→private allow", "embargo/cve-2026-0001", priv, pub, "", false},

		// Public branch pushed to public remote → allow.
		{"main→public allow", "main", pub, pub, "", false},
		{"feature→public allow", "feature/ui", pub, pub, "", false},
		{"release→public allow", "release/v1", pub, pub, "", false},

		// Nested names and malformed globs fail closed.
		{"nested private→public block", "private/team/feature", pub, pub, "", true},

		// CARAVAN_PUBLISH matches exact branch → allow.
		{"publish bypass", "embargo/cve-2026-0001", pub, pub, "embargo/cve-2026-0001", false},

		// CARAVAN_PUBLISH set but for different branch → still block.
		{"publish bypass wrong branch", "embargo/cve-2026-0001", pub, pub, "embargo/other", true},

		// Custom public remote name.
		{"custom public remote match", "private/x", "upstream", "upstream", "", true},
		{"custom public remote mismatch", "private/x", "origin", "upstream", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, reason := gitremote.Decision(tt.branch, tt.targetRemote, tt.publicRemote, globs, tt.publishRef)
			if block != tt.wantBlock {
				t.Errorf("Decision(%q, %q, %q, ..., %q) block=%v, want %v; reason=%q",
					tt.branch, tt.targetRemote, tt.publicRemote, tt.publishRef, block, tt.wantBlock, reason)
			}
			if tt.wantBlock && reason == "" {
				t.Error("block=true but reason is empty")
			}
			if !tt.wantBlock && block {
				t.Errorf("expected allow, got block; reason=%q", reason)
			}
		})
	}
}

// TestGuardPrepushFailsClosed verifies that when no manifest can be found, the
// guardrail still BLOCKS a private-pattern branch from reaching the public
// remote (fail closed against the default globs) rather than letting it leak.
func TestGuardPrepushFailsClosed(t *testing.T) {
	// A directory with no caravan.toml anywhere up the tree.
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	// Feed the pre-push protocol for an embargo branch pushed to "origin".
	stdin := "refs/heads/embargo/cve-2026-0001 aaaa refs/heads/embargo/cve-2026-0001 0000\n"
	code := runGuardWithStdin(t, []string{"origin", "git@github.com:me/app.git"}, stdin)
	if code != 1 {
		t.Fatalf("fail-closed: expected block (exit 1) for embargo→origin with no manifest, got %d", code)
	}

	// Sanity: the same branch pushed to a non-public remote name is allowed.
	code = runGuardWithStdin(t, []string{"private", "git@internal:me/app.git"}, stdin)
	if code != 0 {
		t.Fatalf("expected allow (exit 0) for embargo→private, got %d", code)
	}
}

// chdir changes to dir and returns a restore func.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}

// runGuardWithStdin invokes CmdGuardPrepush with the given args and stdin,
// returning its exit code. It also clears CARAVAN_PUBLISH for determinism.
func runGuardWithStdin(t *testing.T, args []string, stdin string) int {
	t.Helper()
	t.Setenv("CARAVAN_PUBLISH", "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	w.Close()

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	// Ensure no stray manifest env points elsewhere.
	t.Setenv("CARAVAN_MANIFEST", filepath.Join(t.TempDir(), "does-not-exist.toml"))

	return gitremote.CmdGuardPrepush(args)
}
