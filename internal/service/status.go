package service

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"caravan/internal/cliargs"
	"caravan/internal/manifest"
	"caravan/internal/tsserve"
)

// probe is the health-check hook; tests replace it.
var probe = httpProbe

// Row is one service's observed state.
type Row struct {
	Name       string `json:"name"`
	Plist      bool   `json:"plist"`
	Running    bool   `json:"running"`
	PID        string `json:"pid,omitempty"`
	Port       int    `json:"port,omitempty"`
	Declared   string `json:"declaredVisibility"`
	Observed   string `json:"observedVisibility"`
	Health     string `json:"health"`
	URL        string `json:"url,omitempty"`
	Mismatched bool   `json:"exposureMismatch"`
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("service status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestFlag := fs.String("f", "", "manifest path")
	asJSON := fs.Bool("json", false, "emit JSON")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var nameFilter string
	if len(positionals) > 0 {
		nameFilter = positionals[0]
	}

	m, err := manifest.Load(manifest.ResolvePath(*manifestFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "service status: %v\n", err)
		return 1
	}
	services := filterServices(m.Services, nameFilter)
	if len(services) == 0 {
		if nameFilter != "" {
			fmt.Fprintf(os.Stderr, "service status: no service named %q\n", nameFilter)
			return 1
		}
		fmt.Fprintln(os.Stderr, "service status: no [[service]] entries in manifest")
		return 1
	}

	// Observed exposure, keyed by upstream port. Absent tailscale is not an
	// error — it just means we cannot say anything about exposure.
	observed := map[int]tsserve.Mount{}
	if st, err := tsLoad(); err == nil {
		for _, mt := range st.Mounts() {
			if p := mt.Port(); p != 0 {
				// Public wins: if any mount for this port is funnelled, the
				// port is reachable publicly.
				if prev, ok := observed[p]; !ok || (mt.Funnel && !prev.Funnel) {
					observed[p] = mt
				}
			}
		}
	}

	ports := ResolvePorts(m.Services)
	rows := make([]Row, 0, len(services))
	for _, s := range services {
		rows = append(rows, buildRow(s, ports[s.Name], observed))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "service status: %v\n", err)
			return 1
		}
	} else {
		renderRows(os.Stdout, rows)
	}

	for _, r := range rows {
		if r.Mismatched {
			return 1
		}
	}
	return 0
}

func buildRow(s manifest.Service, port int, observed map[int]tsserve.Mount) Row {
	r := Row{
		Name:     s.Name,
		Port:     port,
		Declared: s.Vis(),
		Observed: manifest.VisPrivate,
		Health:   "-",
	}
	if _, err := os.Stat(PlistPath(s.Name)); err == nil {
		r.Plist = true
	}
	r.Running, r.PID = launchdState(Label(s.Name))

	if mt, ok := observed[port]; ok {
		r.Observed = mt.Visibility()
		r.URL = mt.URL()
	}
	r.Mismatched = exposureExceeds(r.Observed, r.Declared)

	if s.Health != "" && port != 0 {
		method, path, want, err := manifest.ParseHealth(s.Health)
		if err != nil {
			r.Health = "bad spec"
		} else {
			got, perr := probe(method, "http://127.0.0.1:"+strconv.Itoa(port)+path)
			switch {
			case perr != nil:
				r.Health = "unreachable"
			case got == want:
				r.Health = "ok"
			default:
				r.Health = fmt.Sprintf("%d≠%d", got, want)
			}
		}
	}
	return r
}

// visRank orders visibility from closed to open so they can be compared.
func visRank(v string) int {
	switch v {
	case manifest.VisTailnet:
		return 1
	case manifest.VisPublic:
		return 2
	default:
		return 0
	}
}

// exposureExceeds reports whether observed exposure is wider than declared.
// Narrower-than-declared is not an error — it just means apply has not run.
func exposureExceeds(observed, declared string) bool {
	return visRank(observed) > visRank(declared)
}

func renderRows(out *os.File, rows []Row) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tPLIST\tRUNNING\tPID\tPORT\tDECLARED\tOBSERVED\tHEALTH\tURL")
	for _, r := range rows {
		glyph := "✓"
		switch {
		case r.Mismatched:
			glyph = "✗"
		case !r.Plist || !r.Running || r.Health == "unreachable":
			glyph = "~"
		}
		port := "-"
		if r.Port != 0 {
			port = strconv.Itoa(r.Port)
		}
		observed := r.Observed
		if r.Mismatched {
			observed += " ⚠"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			glyph, r.Name, tick(r.Plist), tick(r.Running), dash(r.PID),
			port, r.Declared, observed, r.Health, dash(r.URL))
	}
	w.Flush()
	for _, r := range rows {
		if r.Mismatched {
			fmt.Fprintf(out, "\n✗ %s is reachable as %q but declares %q — run `caravan doctor` for the full exposure report.\n",
				r.Name, r.Observed, r.Declared)
		}
	}
}

func tick(b bool) string {
	if b {
		return "✓"
	}
	return "✗"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// launchdState reports whether a label is loaded and its pid.
func launchdState(label string) (bool, string) {
	out, err := launchctlRun("print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	if err != nil {
		return false, ""
	}
	running, pid := false, ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
			if _, convErr := strconv.Atoi(candidate); convErr == nil {
				pid, running = candidate, true
			}
		}
		if strings.Contains(line, "state = running") {
			running = true
		}
	}
	return running, pid
}

// httpProbe performs the health request against a loopback URL.
func httpProbe(method, url string) (int, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// ── logs / rm ─────────────────────────────────────────────────────────────────

func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("service logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	lines := fs.Int("lines", 50, "number of trailing lines")
	_ = fs.String("f", "", "manifest path")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "service logs: need a service name")
		return 2
	}
	path := LogPath(positionals[0])
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service logs: %v\n", err)
		return 1
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > *lines {
		all = all[len(all)-*lines:]
	}
	fmt.Println(strings.Join(all, "\n"))
	return 0
}

func cmdRemove(args []string) int {
	fs := flag.NewFlagSet("service rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_ = fs.String("f", "", "manifest path")

	positionals, err := cliargs.ParseAnywhere(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "service rm: need a service name")
		return 2
	}
	name := positionals[0]
	_, _ = launchctlRun("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label(name)))
	if err := os.Remove(PlistPath(name)); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "service rm: %v\n", err)
		return 1
	}
	fmt.Printf("service: removed %s\n", Label(name))
	fmt.Println("note: any tailscale serve/funnel route for it is left in place — remove it deliberately, or run `caravan doctor`.")
	return 0
}

// filepathDir is a tiny indirection so apply.go does not import path/filepath
// just for one call.
func filepathDir(p string) string { return filepath.Dir(p) }
