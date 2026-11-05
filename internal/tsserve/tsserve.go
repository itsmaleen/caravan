// Package tsserve reads and edits Tailscale's serve/funnel configuration.
//
// It exists because exposure is the one piece of a small-software host that
// normally lives outside every manifest: `tailscale serve` state is stored by
// tailscaled, so nothing reconciles "what should be reachable" against "what
// is reachable". Parsing that state is what lets caravan close the loop.
//
// The critical asymmetry this package encodes: Serve forwards the caller's
// identity headers (Tailscale-User-Login), Funnel does not — Funnel traffic is
// the public internet. A handler is public iff AllowFunnel is set for its
// host:port.
package tsserve

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// runCmd is the exec hook; tests replace it.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// candidateBinaries are searched in order. The macOS app bundle ships the CLI
// somewhere PATH usually misses, which is exactly the machine this targets.
var candidateBinaries = []string{
	"tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/tailscale",
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
}

// Binary returns the first usable tailscale CLI path, or "" if none works.
func Binary() string {
	for _, c := range candidateBinaries {
		if _, err := runCmd(c, "version"); err == nil {
			return c
		}
	}
	return ""
}

// Status is the parsed shape of `tailscale serve status --json`.
type Status struct {
	TCP         map[string]struct{ HTTPS bool } `json:"TCP"`
	Web         map[string]WebHost              `json:"Web"`
	AllowFunnel map[string]bool                 `json:"AllowFunnel"`
}

// WebHost is one host:port's handler table.
type WebHost struct {
	Handlers map[string]Handler `json:"Handlers"`
}

// Handler is a single path handler. Proxy is the upstream target URL.
type Handler struct {
	Proxy string `json:"Proxy"`
	Path  string `json:"Path,omitempty"`
	Text  string `json:"Text,omitempty"`
}

// Mount is one flattened (host:port, path) → target entry, with the resolved
// question of whether the public internet can reach it.
type Mount struct {
	HostPort string // "ms-mac-mini.tail901772.ts.net:443"
	Path     string // "/wrinkles"
	Proxy    string // "http://127.0.0.1:18790"
	Funnel   bool   // true = reachable from the public internet
}

// Port extracts the upstream port from the proxy target, or 0 if it has none.
func (m Mount) Port() int {
	s := m.Proxy
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return 0
	}
	return p
}

// Visibility returns "public" or "tailnet" for this mount, matching the
// manifest's vocabulary.
func (m Mount) Visibility() string {
	if m.Funnel {
		return "public"
	}
	return "tailnet"
}

// URL renders the externally visible URL for the mount.
func (m Mount) URL() string {
	host := m.HostPort
	if strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	path := m.Path
	if path == "/" {
		path = ""
	}
	return "https://" + host + path
}

// Mounts flattens the status into a sorted, stable list.
func (s *Status) Mounts() []Mount {
	var out []Mount
	for hostPort, web := range s.Web {
		for path, h := range web.Handlers {
			out = append(out, Mount{
				HostPort: hostPort,
				Path:     path,
				Proxy:    h.Proxy,
				Funnel:   s.AllowFunnel[hostPort],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// ParseStatus decodes `tailscale serve status --json` output.
func ParseStatus(data []byte) (*Status, error) {
	trimmed := strings.TrimSpace(string(data))
	// A node with nothing served prints a human string rather than JSON.
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return &Status{}, nil
	}
	var st Status
	if err := json.Unmarshal([]byte(trimmed), &st); err != nil {
		return nil, fmt.Errorf("parsing tailscale serve status: %w", err)
	}
	return &st, nil
}

// Load shells out to the tailscale CLI and parses the result. A nil error with
// an empty Status means "tailscale is present but serving nothing".
func Load() (*Status, error) {
	bin := Binary()
	if bin == "" {
		return nil, ErrNoTailscale
	}
	out, err := runCmd(bin, "serve", "status", "--json")
	if err != nil {
		// `serve status` exits non-zero when nothing is configured.
		if st, perr := ParseStatus(out); perr == nil {
			return st, nil
		}
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	return ParseStatus(out)
}

// ErrNoTailscale is returned when no tailscale CLI can be found.
var ErrNoTailscale = fmt.Errorf("tailscale CLI not found")

// SetArgs returns the argv that publishes target at path with the given
// visibility. It is a pure function so the command is testable and so
// --dry-run can print exactly what would run.
func SetArgs(visibility, path, target string) []string {
	verb := "serve"
	if visibility == "public" {
		verb = "funnel"
	}
	return []string{verb, "--bg", "--set-path=" + path, target}
}

// Set publishes target at path. Callers are responsible for refusing to widen
// exposure without an explicit human decision.
func Set(visibility, path, target string) error {
	bin := Binary()
	if bin == "" {
		return ErrNoTailscale
	}
	out, err := runCmd(bin, SetArgs(visibility, path, target)...)
	if err != nil {
		return fmt.Errorf("tailscale %s: %v: %s", visibility, err, strings.TrimSpace(string(out)))
	}
	return nil
}
