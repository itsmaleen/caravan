package service

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
	"caravan/internal/tsserve"
)

// launchctlRun is the launchctl hook; tests replace it.
var launchctlRun = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

// tsSet is the exposure hook; tests replace it.
var tsSet = tsserve.Set

// tsLoad is the exposure-reading hook; tests replace it.
var tsLoad = tsserve.Load

func cmdApply(args []string) int {
	fs := flag.NewFlagSet("service apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFlag := fs.String("f", "", "manifest path")
	dryRun := fs.Bool("dry-run", false, "print what would change, touch nothing")
	prune := fs.Bool("prune", false, "also tear down exposure the manifest no longer declares")
	allowPublic := fs.Bool("allow-public", false, "permit visibility=public (funnel to the open internet)")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if runtime.GOOS != "darwin" && !*dryRun {
		fmt.Fprintln(os.Stderr, "service apply: only supported on macOS (use --dry-run elsewhere)")
		return 1
	}

	var nameFilter string
	if len(positionals) > 0 {
		nameFilter = positionals[0]
	}

	mpath := manifest.ResolvePath(*manifestFlag)
	m, err := manifest.Load(mpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service apply: %v\n", err)
		return 1
	}

	services := filterServices(m.Services, nameFilter)
	if len(services) == 0 {
		if nameFilter != "" {
			fmt.Fprintf(os.Stderr, "service apply: no service named %q\n", nameFilter)
		} else {
			fmt.Fprintln(os.Stderr, "service apply: no [[service]] entries in manifest")
		}
		return 1
	}

	// Refuse to widen exposure without an explicit human decision. This is the
	// whole lesson of the category encoded as a flag: a funnel is a thing you
	// choose, never a thing that happens.
	if !*allowPublic {
		var blocked []string
		for _, s := range services {
			if s.Vis() == manifest.VisPublic {
				blocked = append(blocked, s.Name)
			}
		}
		if len(blocked) > 0 {
			fmt.Fprintf(os.Stderr,
				"service apply: %v declared visibility=public.\n"+
					"  Funnel publishes to the open internet and does NOT forward identity headers.\n"+
					"  Re-run with --allow-public if that is what you mean.\n", blocked)
			return 1
		}
	}

	ports := ResolvePorts(m.Services)
	code := 0
	for _, s := range services {
		if err := applyOne(m, s, ports[s.Name], *dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "service apply %s: %v\n", s.Name, err)
			code = 1
		}
	}

	if *prune {
		if err := pruneUndeclared(m, *dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "service apply: prune: %v\n", err)
			code = 1
		}
	}
	return code
}

func applyOne(m *manifest.Manifest, s manifest.Service, port int, dryRun bool) error {
	label := Label(s.Name)
	plistPath := PlistPath(s.Name)
	logPath := LogPath(s.Name)
	dir := m.ServiceDir(s)
	command := ExpandCommand(s.Command, port)
	plist := Plist(label, dir, command, s.Env, logPath)

	if dryRun {
		fmt.Printf("would write  %s\n", plistPath)
		fmt.Printf("would run    /bin/sh -lc %q  (cwd %s)\n", command, dir)
		if s.Vis() != manifest.VisPrivate {
			fmt.Printf("would expose %s %s → %s  [%s]\n",
				s.Vis(), s.RoutePath(), Target(port), joinArgs(tsserve.SetArgs(s.Vis(), s.RoutePath(), Target(port))))
		} else {
			fmt.Printf("would expose nothing (visibility=private)\n")
		}
		return nil
	}

	if err := os.MkdirAll(filepathDir(logPath), 0o755); err != nil {
		return fmt.Errorf("mkdir logs: %w", err)
	}
	if err := os.MkdirAll(launchAgentsDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}

	uid := os.Getuid()
	if _, statErr := os.Stat(plistPath); statErr == nil {
		_, _ = launchctlRun("bootout", fmt.Sprintf("gui/%d/%s", uid, label))
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if _, err := launchctlRun("bootstrap", fmt.Sprintf("gui/%d", uid), plistPath); err != nil {
		if _, loadErr := launchctlRun("load", plistPath); loadErr != nil {
			return fmt.Errorf("launchctl: %w", err)
		}
	}
	fmt.Printf("service: %s running (port %d)\n", s.Name, port)

	if s.Vis() == manifest.VisPrivate {
		return nil
	}
	if err := tsSet(s.Vis(), s.RoutePath(), Target(port)); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	fmt.Printf("service: %s published %s at %s\n", s.Name, s.Vis(), s.RoutePath())
	return nil
}

// pruneUndeclared reports mounts that no service declares. It never removes
// them silently — it prints the exact command, because tearing down a route
// someone is relying on is not a thing to do behind their back.
func pruneUndeclared(m *manifest.Manifest, dryRun bool) error {
	st, err := tsLoad()
	if err != nil {
		if errors.Is(err, tsserve.ErrNoTailscale) {
			return nil
		}
		return err
	}
	declared := map[string]bool{}
	ports := ResolvePorts(m.Services)
	for _, s := range m.Services {
		if s.Vis() != manifest.VisPrivate {
			declared[s.RoutePath()+" "+Target(ports[s.Name])] = true
		}
	}
	bin := tsserve.Binary()
	if bin == "" {
		bin = "tailscale"
	}
	for _, mt := range st.Mounts() {
		if declared[mt.Path+" "+mt.Proxy] {
			continue
		}
		verb := "serve"
		if mt.Funnel {
			verb = "funnel"
		}
		fmt.Printf("undeclared: %s %s → %s\n", mt.Visibility(), mt.URL(), mt.Proxy)
		fmt.Printf("  remove with: %s %s --set-path=%s off\n", bin, verb, mt.Path)
	}
	return nil
}

func joinArgs(args []string) string {
	out := "tailscale"
	for _, a := range args {
		out += " " + a
	}
	return out
}
