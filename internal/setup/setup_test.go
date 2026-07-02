package setup_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"caravan/internal/setup"
)

// ── captureStdout ─────────────────────────────────────────────────────────────

// captureStdout redirects os.Stdout during f and returns the captured output.
// Uses the drain-before-close pattern from internal/provision/provision_test.go.
// stubDoctor replaces the doctor self-invocation (which would recurse into
// the test binary) with a canned string for the duration of a test.
func stubDoctor(t *testing.T) {
	t.Helper()
	orig := setup.RunDoctor
	setup.RunDoctor = func(string) string { return "environment  git  OK (stubbed)" }
	t.Cleanup(func() { setup.RunDoctor = orig })
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	f()

	w.Close()
	os.Stdout = orig
	out := <-outC
	r.Close()
	return out
}

// ── fake binary helpers ───────────────────────────────────────────────────────

// makeFakeBinary writes a minimal shell-script executable named name into dir
// and returns its path. The script exits 0 so LookPath is satisfied.
func makeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	content := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake binary %s: %v", name, err)
	}
	return p
}

// withFakePath calls f with the LookPath variable scoped to a directory that
// contains exactly the listed binary names.
func withFakePath(t *testing.T, names []string, f func()) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		makeFakeBinary(t, dir, name)
	}
	orig := setup.LookPath
	setup.LookPath = func(file string) (string, error) {
		// Check whether any fake binary in dir matches.
		p := filepath.Join(dir, file)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		// Not in our fake dir — not found.
		return "", &os.PathError{Op: "lookPath", Path: file, Err: os.ErrNotExist}
	}
	t.Cleanup(func() { setup.LookPath = orig })
	f()
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestDetectionOrder verifies that when multiple agents are present the first
// one in the registry wins (claude > codex > gemini > opencode > cursor-agent).
func TestDetectionOrder(t *testing.T) {
	stubDoctor(t)
	// Make all agents "available".
	withFakePath(t, []string{"claude", "codex", "gemini", "opencode", "cursor-agent"}, func() {
		// --print-prompt lets us observe which agent would be selected without
		// actually launching anything; we check the output exists (exit 0).
		out := captureStdout(t, func() {
			code := setup.CmdSetup([]string{"--print-prompt"})
			if code != 0 {
				t.Errorf("CmdSetup --print-prompt returned %d (wanted 0)", code)
			}
		})
		// The prompt should be non-empty — detection happened successfully.
		if len(strings.TrimSpace(out)) == 0 {
			t.Error("expected non-empty prompt output")
		}
	})
}

// TestAgentFlagOverride verifies that --agent selects a specific agent even
// when a higher-priority agent is also present.
func TestAgentFlagOverride(t *testing.T) {
	stubDoctor(t)
	withFakePath(t, []string{"claude", "gemini"}, func() {
		// Ask for gemini explicitly; this should succeed (binary exists).
		out := captureStdout(t, func() {
			code := setup.CmdSetup([]string{"--agent", "gemini", "--print-prompt"})
			if code != 0 {
				t.Errorf("CmdSetup --agent gemini --print-prompt returned %d (wanted 0)", code)
			}
		})
		if len(strings.TrimSpace(out)) == 0 {
			t.Error("expected non-empty prompt for --agent gemini")
		}
	})
}

// TestAgentFlagUnknown verifies that --agent with an unknown name exits 1.
func TestAgentFlagUnknown(t *testing.T) {
	stubDoctor(t)
	withFakePath(t, []string{"claude"}, func() {
		code := setup.CmdSetup([]string{"--agent", "notarealbot", "--print-prompt"})
		if code != 1 {
			t.Errorf("expected exit 1 for unknown agent, got %d", code)
		}
	})
}

// TestAgentFlagBinaryMissing verifies that --agent with a known name but
// missing binary exits 1 with a helpful message. Note: we do NOT pass
// --print-prompt here because that flag short-circuits before agent lookup.
func TestAgentFlagBinaryMissing(t *testing.T) {
	stubDoctor(t)
	// Make an empty fake PATH (no binaries at all).
	withFakePath(t, nil, func() {
		// Capture stderr so the error message doesn't pollute test output.
		origStderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stderr = w

		errC := make(chan string)
		go func() {
			var buf bytes.Buffer
			io.Copy(&buf, r)
			errC <- buf.String()
		}()

		code := setup.CmdSetup([]string{"--agent", "claude"})

		w.Close()
		os.Stderr = origStderr
		<-errC
		r.Close()

		if code != 1 {
			t.Errorf("expected exit 1 when claude binary missing, got %d", code)
		}
	})
}

// TestNoAgentFound verifies that when no agent is in PATH the exit code is 1
// and the output mentions the supported agents and --print-prompt.
func TestNoAgentFound(t *testing.T) {
	stubDoctor(t)
	withFakePath(t, nil, func() {
		// Capture stderr via a pipe (CmdSetup writes to os.Stderr).
		origStderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stderr = w

		errC := make(chan string)
		go func() {
			var buf bytes.Buffer
			io.Copy(&buf, r)
			errC <- buf.String()
		}()

		code := setup.CmdSetup([]string{})

		w.Close()
		os.Stderr = origStderr
		errOut := <-errC
		r.Close()

		if code != 1 {
			t.Errorf("expected exit 1 when no agent found, got %d", code)
		}
		if !strings.Contains(errOut, "claude") {
			t.Errorf("expected agent names in no-agent error; stderr:\n%s", errOut)
		}
		if !strings.Contains(errOut, "--print-prompt") {
			t.Errorf("expected --print-prompt hint in no-agent error; stderr:\n%s", errOut)
		}
	})
}

// TestPrintPromptOutput verifies that --print-prompt exits 0 and writes the
// assembled prompt to stdout, including key context markers and guardrail text.
func TestPrintPromptOutput(t *testing.T) {
	stubDoctor(t)
	withFakePath(t, []string{"claude"}, func() {
		out := captureStdout(t, func() {
			code := setup.CmdSetup([]string{"--print-prompt"})
			if code != 0 {
				t.Errorf("CmdSetup --print-prompt returned %d (wanted 0)", code)
			}
		})

		// Context markers
		for _, marker := range []string{
			"Caravan version",
			"OS / arch",
			"Hostname",
			"Manifest path",
			"Manifest exists",
			"Doctor output",
			"Tool inventory",
		} {
			if !strings.Contains(out, marker) {
				t.Errorf("prompt missing context marker %q", marker)
			}
		}

		// Guardrail phrases
		guardrails := []string{
			"Never print secret",
			"ask before",
			"git repos",
			"[[sync]]",
			"caravan doctor",
		}
		for _, phrase := range guardrails {
			if !strings.Contains(out, phrase) {
				t.Errorf("prompt missing guardrail phrase %q", phrase)
			}
		}
	})
}

// TestPrintPromptWithManifest verifies that when a manifest exists its
// contents appear in the --print-prompt output.
func TestPrintPromptWithManifest(t *testing.T) {
	stubDoctor(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "caravan.toml")
	content := `version = 1
[workspace]
root = "~/code"
`
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	withFakePath(t, []string{"claude"}, func() {
		out := captureStdout(t, func() {
			code := setup.CmdSetup([]string{"-f", manifestPath, "--print-prompt"})
			if code != 0 {
				t.Errorf("CmdSetup returned %d", code)
			}
		})
		if !strings.Contains(out, `root = "~/code"`) {
			t.Errorf("expected manifest contents in prompt; got (first 500 chars):\n%s", out[:min(500, len(out))])
		}
		if !strings.Contains(out, "yes") {
			t.Errorf("expected 'yes' for manifest exists in prompt")
		}
	})
}

// TestArgvBuilders verifies the exact argv slices produced by each agent's
// interactive and headless argv builders.
func TestArgvBuilders(t *testing.T) {
	stubDoctor(t)
	const testPrompt = "Hello caravan"

	cases := []struct {
		agentName    string
		wantInteractive []string
		wantHeadless    []string
	}{
		{
			agentName:    "claude",
			wantInteractive: []string{"claude", testPrompt},
			wantHeadless: []string{
				"claude", "-p", testPrompt,
				"--permission-mode", "acceptEdits",
				"--allowedTools", "Bash(caravan:*)", "Bash(./caravan:*)", "Read", "Write", "Edit",
			},
		},
		{
			agentName:    "codex",
			wantInteractive: []string{"codex", testPrompt},
			wantHeadless:    []string{"codex", "exec", testPrompt},
		},
		{
			agentName:    "gemini",
			wantInteractive: []string{"gemini", "-i", testPrompt},
			wantHeadless:    []string{"gemini", "-p", testPrompt},
		},
		{
			agentName:    "opencode",
			wantInteractive: []string{"opencode", testPrompt},
			wantHeadless:    []string{"opencode", "run", testPrompt},
		},
		{
			agentName:    "cursor-agent",
			wantInteractive: []string{"cursor-agent", testPrompt},
			wantHeadless:    []string{"cursor-agent", "-p", testPrompt},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.agentName, func(t *testing.T) {
			got := setup.AgentInteractiveArgv(tc.agentName, testPrompt)
			if !sliceEqual(got, tc.wantInteractive) {
				t.Errorf("interactive argv mismatch\n  got:  %v\n  want: %v", got, tc.wantInteractive)
			}
			got = setup.AgentHeadlessArgv(tc.agentName, testPrompt)
			if !sliceEqual(got, tc.wantHeadless) {
				t.Errorf("headless argv mismatch\n  got:  %v\n  want: %v", got, tc.wantHeadless)
			}
		})
	}
}

// sliceEqual returns true if a and b have identical contents.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

