// Package setup implements `caravan setup` — an agent-powered wizard that
// finds an AI coding agent already installed on the machine and hands it a
// crafted prompt so the agent can walk the user through configuring caravan.
//
// Design goals:
//   - Zero new dependencies: uses only stdlib + existing caravan packages.
//   - Inference runs on the user's own agent subscription; costs the caravan
//     project nothing.
//   - Interactive mode replaces the process via syscall.Exec so the agent
//     owns the TTY.
//   - --print-prompt lets users paste the assembled prompt into any chat UI.
package setup

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/template"

	"caravan/internal/buildinfo"
	"caravan/internal/cliargs"
	"caravan/internal/manifest"
)

// ── embedded prompt template ─────────────────────────────────────────────────

//go:embed prompt.md
var promptTemplate string

// RunDoctor captures `caravan doctor` output for the prompt's context block by
// re-invoking the current executable (ignoring exit code — doctor exits 1 on
// any failing check). Package var so tests can stub it: under `go test`,
// os.Executable() is the test binary and self-invocation would recurse.
var RunDoctor = func(manifestPath string) string {
	selfPath, err := os.Executable()
	if err != nil {
		selfPath = "caravan"
	}
	out, _ := exec.Command(selfPath, "doctor", "-f", manifestPath).CombinedOutput() // #nosec G204
	return string(out)
}

// ── agent registry ────────────────────────────────────────────────────────────

// agent describes a supported AI coding agent and how to launch it.
type agent struct {
	// Name is the display name (also the value accepted by --agent).
	Name string
	// Binary is the executable to look up in PATH.
	Binary string
	// InteractiveArgv returns the argv (including binary as argv[0]) for
	// interactive mode — the process replaces the current one via syscall.Exec.
	InteractiveArgv func(prompt string) []string
	// HeadlessArgv returns argv for non-interactive (--headless) mode.
	// nil means the agent does not support headless mode.
	HeadlessArgv func(prompt string) []string
}

// agents is ordered by detection preference.
var agents = []agent{
	{
		Name:   "claude",
		Binary: "claude",
		// claude <prompt>  — interactive session with prompt pre-loaded.
		InteractiveArgv: func(prompt string) []string {
			return []string{"claude", prompt}
		},
		// claude -p <prompt> --permission-mode acceptEdits
		//        --allowedTools "Bash(caravan:*)" "Bash(./caravan:*)" Read Write Edit
		//
		// --allowedTools accepts a space-separated list in one string OR repeated
		// values; from `claude --help`: "--allowedTools, --allowed-tools <tools...>
		// Comma or space-separated list of tool names to allow".
		// We pass multiple separate args (one per tool) which is the safest form.
		HeadlessArgv: func(prompt string) []string {
			return []string{
				"claude", "-p", prompt,
				"--permission-mode", "acceptEdits",
				"--allowedTools", "Bash(caravan:*)", "Bash(./caravan:*)", "Read", "Write", "Edit",
			}
		},
	},
	{
		Name:   "codex",
		Binary: "codex",
		InteractiveArgv: func(prompt string) []string {
			return []string{"codex", prompt}
		},
		HeadlessArgv: func(prompt string) []string {
			return []string{"codex", "exec", prompt}
		},
	},
	{
		Name:   "gemini",
		Binary: "gemini",
		// gemini -i <prompt>  — executes the prompt and stays in interactive mode.
		// From `gemini --help`: "-i, --prompt-interactive  Execute the provided
		// prompt and continue in interactive mode".
		InteractiveArgv: func(prompt string) []string {
			return []string{"gemini", "-i", prompt}
		},
		// gemini -p <prompt>  — non-interactive (headless).
		HeadlessArgv: func(prompt string) []string {
			return []string{"gemini", "-p", prompt}
		},
	},
	{
		Name:   "opencode",
		Binary: "opencode",
		// opencode [project]  — TUI mode. We use `opencode run <prompt>` for
		// headless (confirmed from `opencode run --help`); for interactive we pass
		// the prompt as positional args to the default TUI command.
		InteractiveArgv: func(prompt string) []string {
			return []string{"opencode", prompt}
		},
		HeadlessArgv: func(prompt string) []string {
			return []string{"opencode", "run", prompt}
		},
	},
	{
		Name:   "cursor-agent",
		Binary: "cursor-agent",
		// cursor-agent <prompt>  — interactive session.
		// cursor-agent -p <prompt>  — print/non-interactive.
		// From `cursor-agent --help`: "-p, --print  Print responses to console
		// (for scripts or non-interactive use)."
		InteractiveArgv: func(prompt string) []string {
			return []string{"cursor-agent", prompt}
		},
		HeadlessArgv: func(prompt string) []string {
			return []string{"cursor-agent", "-p", prompt}
		},
	},
}

// LookPath is exec.LookPath by default; tests override it to inject fake binaries.
var LookPath = exec.LookPath

// ── context gathering ─────────────────────────────────────────────────────────

// toolInfo holds a discovered tool's name and version string.
type toolInfo struct {
	Name    string
	Version string
}

// promptContext is the data fed into prompt.md's text/template.
type promptContext struct {
	Version         string
	OS              string
	Arch            string
	Hostname        string
	ManifestPath    string
	ManifestExists  string
	ManifestContents string
	DoctorOutput    string
	Tools           []toolInfo
}

// gatherContext collects machine context for the prompt. All sub-operations are
// best-effort; failures degrade to "(unavailable)".
func gatherContext(manifestPath string) promptContext {
	ctx := promptContext{
		Version:      buildinfo.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		ManifestPath: manifestPath,
	}

	// Hostname
	if h, err := os.Hostname(); err == nil {
		ctx.Hostname = h
	} else {
		ctx.Hostname = "(unavailable)"
	}

	// Manifest existence + contents
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		ctx.ManifestExists = "no"
	} else if err != nil {
		ctx.ManifestExists = "error reading: " + err.Error()
	} else {
		ctx.ManifestExists = "yes"
		if len(data) < 4*1024 {
			ctx.ManifestContents = string(data)
		} else {
			ctx.ManifestContents = "(file exceeds 4 KB — run `caravan doctor` to inspect)"
		}
	}

	if out := RunDoctor(manifestPath); out != "" {
		ctx.DoctorOutput = strings.TrimRight(out, "\n")
	} else {
		ctx.DoctorOutput = "(unavailable)"
	}

	// Tool inventory
	type toolSpec struct {
		name string
		args []string
	}
	tools := []toolSpec{
		{"git", []string{"--version"}},
		{"ssh", []string{"-V"}},
		{"rsync", []string{"--version"}},
		{"mise", []string{"--version"}},
		{"direnv", []string{"version"}},
	}
	for _, t := range tools {
		if _, pathErr := LookPath(t.name); pathErr != nil {
			ctx.Tools = append(ctx.Tools, toolInfo{t.name, "not found"})
			continue
		}
		out, _ := exec.Command(t.name, t.args...).CombinedOutput() // #nosec G204
		ver := firstLine(strings.TrimSpace(string(out)))
		if ver == "" {
			ver = "installed"
		}
		ctx.Tools = append(ctx.Tools, toolInfo{t.name, ver})
	}

	return ctx
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return s
}

// ── prompt assembly ───────────────────────────────────────────────────────────

// assemblePrompt renders the embedded prompt.md template with ctx.
func assemblePrompt(ctx promptContext) (string, error) {
	tmpl, err := template.New("setup").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("rendering prompt template: %w", err)
	}
	return buf.String(), nil
}

// ── agent detection ───────────────────────────────────────────────────────────

// detectAgent returns the first agent whose binary is found in PATH.
func detectAgent() (*agent, string) {
	for i := range agents {
		a := &agents[i]
		path, err := LookPath(a.Binary)
		if err == nil && path != "" {
			return a, path
		}
	}
	return nil, ""
}

// findAgentByName looks up an agent by name, resolves its binary path, and
// returns an error if the binary cannot be found.
func knownAgentName(name string) bool {
	for _, a := range agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

func supportedAgentNames() string {
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

func findAgentByName(name string) (*agent, string, error) {
	for i := range agents {
		a := &agents[i]
		if a.Name == name {
			path, err := LookPath(a.Binary)
			if err != nil {
				return nil, "", fmt.Errorf("agent %q: binary %q not found in PATH", name, a.Binary)
			}
			return a, path, nil
		}
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return nil, "", fmt.Errorf("unknown agent %q; supported: %s", name, strings.Join(names, ", "))
}

// agentNames returns the display names of all registered agents.
func agentNames() []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// ── launch ────────────────────────────────────────────────────────────────────

// launchInteractive replaces the current process with the agent binary via
// syscall.Exec. If Exec fails (e.g. on Windows), it falls back to running the
// command as a child process with stdio passthrough.
func launchInteractive(binaryPath string, argv []string) int {
	// Resolve to an absolute path before Exec to be safe.
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		abs = binaryPath
	}

	// syscall.Exec replaces the process; on success it never returns.
	execErr := syscall.Exec(abs, argv, os.Environ())
	if execErr != nil {
		// Fallback: run as child, pass stdio through.
		cmd := exec.Command(abs, argv[1:]...) // #nosec G204
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if runErr := cmd.Run(); runErr != nil {
			var exitErr *exec.ExitError
			if ok := isExitError(runErr, &exitErr); ok {
				return exitErr.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "caravan setup: %v\n", runErr)
			return 1
		}
	}
	return 0
}

// launchHeadless runs the agent as a child process with stdio passthrough and
// returns its exit code.
func launchHeadless(binaryPath string, argv []string) int {
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		abs = binaryPath
	}
	cmd := exec.Command(abs, argv[1:]...) // #nosec G204
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(err, &exitErr); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "caravan setup: %v\n", err)
		return 1
	}
	return 0
}

// isExitError is a small helper to avoid a direct type assertion in two spots.
func isExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// ── exported test helpers ─────────────────────────────────────────────────────

// AgentInteractiveArgv returns the interactive argv for the named agent given a
// prompt. Returns nil if the agent name is unknown. Exported for tests.
func AgentInteractiveArgv(name, prompt string) []string {
	for _, a := range agents {
		if a.Name == name {
			return a.InteractiveArgv(prompt)
		}
	}
	return nil
}

// AgentHeadlessArgv returns the headless argv for the named agent given a
// prompt. Returns nil if the agent name is unknown or has no headless mode.
// Exported for tests.
func AgentHeadlessArgv(name, prompt string) []string {
	for _, a := range agents {
		if a.Name == name {
			if a.HeadlessArgv == nil {
				return nil
			}
			return a.HeadlessArgv(prompt)
		}
	}
	return nil
}

// ── CmdSetup ─────────────────────────────────────────────────────────────────

// CmdSetup implements `caravan setup [flags]`.
//
// Exit codes:
//
//	0  success (or --print-prompt succeeded)
//	1  no agent found / agent exited non-zero / other runtime error
//	2  bad flags
func CmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	f := fs.String("f", "", "manifest path (default: ~/.config/caravan/caravan.toml)")
	agentFlag := fs.String("agent", "", "force a specific agent by name (claude, codex, gemini, opencode, cursor-agent)")
	headless := fs.Bool("headless", false, "run agent non-interactively (one-shot) instead of handing over the TTY")
	printPrompt := fs.Bool("print-prompt", false, "print the assembled prompt and exit (for debugging or pasting into any chat UI)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: caravan setup [-f MANIFEST] [--agent NAME] [--headless] [--print-prompt]

Finds an AI coding agent already installed on this machine and hands it a
crafted prompt so it can walk you through configuring caravan interactively.
Inference runs on your existing Claude/OpenAI/Gemini subscription.

Supported agents (checked in this order): %s

Flags:
`, strings.Join(agentNames(), ", "))
		fs.PrintDefaults()
	}

	if _, err := cliargs.ParseAnywhere(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	manifestPath := manifest.ResolvePath(*f)

	// Gather machine context.
	ctx := gatherContext(manifestPath)

	// Render prompt.
	prompt, err := assemblePrompt(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "caravan setup: %v\n", err)
		return 1
	}

	// --print-prompt: output to stdout and exit.
	if *printPrompt {
		// Still catch an explicitly misspelled --agent name; a missing binary
		// is fine here (the whole point of --print-prompt is agent-less use).
		if *agentFlag != "" && !knownAgentName(*agentFlag) {
			fmt.Fprintf(os.Stderr, "caravan setup: unknown agent %q; supported: %s\n", *agentFlag, supportedAgentNames())
			return 1
		}
		fmt.Print(prompt)
		return 0
	}

	// Resolve agent.
	var chosenAgent *agent
	var binaryPath string
	if *agentFlag != "" {
		var findErr error
		chosenAgent, binaryPath, findErr = findAgentByName(*agentFlag)
		if findErr != nil {
			fmt.Fprintf(os.Stderr, "caravan setup: %v\n", findErr)
			fmt.Fprintf(os.Stderr, "Tip: use --print-prompt to paste the prompt into any chat UI instead.\n")
			return 1
		}
	} else {
		chosenAgent, binaryPath = detectAgent()
		if chosenAgent == nil {
			fmt.Fprintf(os.Stderr, "caravan setup: no supported AI coding agent found on this machine.\n")
			fmt.Fprintf(os.Stderr, "Supported agents: %s\n", strings.Join(agentNames(), ", "))
			fmt.Fprintf(os.Stderr, "\nInstall one (e.g. `npm i -g @anthropic-ai/claude-code`) or use\n")
			fmt.Fprintf(os.Stderr, "`caravan setup --print-prompt` to paste the prompt into any chat UI.\n")
			return 1
		}
	}

	// Build argv.
	if *headless {
		if chosenAgent.HeadlessArgv == nil {
			fmt.Fprintf(os.Stderr, "caravan setup: agent %q does not support --headless mode\n", chosenAgent.Name)
			return 1
		}
		argv := chosenAgent.HeadlessArgv(prompt)
		fmt.Fprintf(os.Stderr, "caravan setup: launching %s (headless)\n", chosenAgent.Name)
		return launchHeadless(binaryPath, argv)
	}

	argv := chosenAgent.InteractiveArgv(prompt)
	fmt.Fprintf(os.Stderr, "caravan setup: handing over to %s — enjoy the wizard!\n", chosenAgent.Name)
	return launchInteractive(binaryPath, argv)
}
