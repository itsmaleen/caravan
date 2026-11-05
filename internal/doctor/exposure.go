package doctor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"caravan/internal/manifest"
	"caravan/internal/service"
	"caravan/internal/tsserve"
)

// This file implements the two exposure checks. They are deliberately useful
// on a machine with zero [[service]] blocks, because the failure they catch —
// a route that was right when it was created and became wrong when the thing
// behind it changed — happens to people who have not adopted the manifest yet.

// tsLoad is the exposure-reading hook; tests replace it.
var tsLoad = tsserve.Load

// listeners returns "port → process command line" for listening TCP sockets.
// Replaced in tests.
var listeners = defaultListeners

// devServerSignatures are command-line markers of a development server. These
// serve unminified source and expose filesystem handlers; none of them belong
// on a public funnel.
var devServerSignatures = []string{
	"vite", "next dev", "nuxt dev", "webpack-dev-server", "webpack serve",
	"ng serve", "react-scripts start", "remix dev", "astro dev", "svelte-kit dev",
	"rails server", "flask run", "manage.py runserver", "php -S",
	"--watch-poll", "hmr",
}

// devServerBodyMarkers are HMR client markers served by dev servers. Used only
// when the process table is unavailable (remote or restricted environments).
var devServerBodyMarkers = []string{
	"/@vite/client", "__vite_ping", "webpack-hmr", "__nextjs_original-stack-frame",
	"/_next/static/chunks/react-refresh", "sockjs-node",
}

// checkExposure runs both exposure checks. m may be nil.
func checkExposure(m *manifest.Manifest) []result {
	st, err := tsLoad()
	if err != nil {
		if errors.Is(err, tsserve.ErrNoTailscale) {
			return []result{{"exposure", "tailscale", statusNA, "tailscale CLI not found — exposure not checked"}}
		}
		return []result{{"exposure", "tailscale", statusWarn, err.Error()}}
	}
	mounts := st.Mounts()
	if len(mounts) == 0 {
		return []result{{"exposure", "published routes", statusOK, "nothing served"}}
	}

	var out []result
	out = append(out, checkDeclaredExposure(m, mounts)...)
	out = append(out, checkFunnelDevServers(mounts)...)
	return out
}

// ── check 1: observed exposure vs declared ───────────────────────────────────

// checkDeclaredExposure compares every live mount against the manifest. Two
// distinct failures: exposure wider than declared, and exposure nothing
// declares at all (drift).
func checkDeclaredExposure(m *manifest.Manifest, mounts []tsserve.Mount) []result {
	declared := map[int]declaredService{}
	if m != nil {
		ports := service.ResolvePorts(m.Services)
		for _, s := range m.Services {
			if p := ports[s.Name]; p != 0 {
				declared[p] = declaredService{name: s.Name, vis: s.Vis(), route: s.RoutePath()}
			}
		}
	}

	var out []result
	for _, mt := range mounts {
		port := mt.Port()
		label := mt.Path + " → " + shortTarget(mt.Proxy)
		d, known := declared[port]

		switch {
		case !known:
			detail := fmt.Sprintf("%s reachable as %s, declared by no service", mt.URL(), mt.Visibility())
			status := statusWarn
			if mt.Funnel {
				// Public and undeclared is the case worth failing on: nobody
				// wrote down that the internet should reach this.
				status = statusFail
				detail = fmt.Sprintf("%s is PUBLIC and declared by no service", mt.URL())
			}
			out = append(out, result{"exposure", label, status, detail})

		case rank(mt.Visibility()) > rank(d.vis):
			out = append(out, result{"exposure", label, statusFail, fmt.Sprintf(
				"service %q declares %s but is reachable as %s (%s)", d.name, d.vis, mt.Visibility(), mt.URL())})

		default:
			out = append(out, result{"exposure", label, statusOK, fmt.Sprintf(
				"service %q %s — %s", d.name, d.vis, mt.URL())})
		}
	}
	return out
}

type declaredService struct {
	name  string
	vis   string
	route string
}

func rank(v string) int {
	switch v {
	case manifest.VisTailnet:
		return 1
	case manifest.VisPublic:
		return 2
	default:
		return 0
	}
}

// ── check 2: funnel pointing at a dev server ─────────────────────────────────

// checkFunnelDevServers flags public funnels whose upstream is a development
// server. Dev servers serve unminified source and expose filesystem handlers;
// on a funnel that is a source leak, not a misconfiguration.
func checkFunnelDevServers(mounts []tsserve.Mount) []result {
	var public []tsserve.Mount
	for _, mt := range mounts {
		if mt.Funnel && mt.Port() != 0 {
			public = append(public, mt)
		}
	}
	if len(public) == 0 {
		return []result{{"exposure", "public funnels", statusOK, "no funnel is publishing a local port"}}
	}

	procs := listeners()
	var out []result
	for _, mt := range public {
		port := mt.Port()
		cmdline := procs[port]
		if sig := matchDevServer(cmdline); sig != "" {
			out = append(out, result{"exposure", "funnel → dev server", statusFail, fmt.Sprintf(
				"%s serves a dev server (%s) — unminified source and fs handlers are public. Run: tailscale funnel --set-path=%s off",
				mt.URL(), sig, mt.Path)})
			continue
		}
		if cmdline == "" {
			// Cannot see the process table; fall back to asking the port what
			// it serves. A dev server answers with its HMR client.
			if marker := probeDevServer(port); marker != "" {
				out = append(out, result{"exposure", "funnel → dev server", statusFail, fmt.Sprintf(
					"%s responds with %s — that is a dev server on a public funnel", mt.URL(), marker)})
				continue
			}
			out = append(out, result{"exposure", "funnel " + mt.Path, statusWarn, fmt.Sprintf(
				"%s is PUBLIC; could not identify the process on port %d", mt.URL(), port)})
			continue
		}
		out = append(out, result{"exposure", "funnel " + mt.Path, statusWarn, fmt.Sprintf(
			"%s is PUBLIC → %s", mt.URL(), firstWord(cmdline))})
	}
	return out
}

// matchDevServer returns the signature that matched, or "".
func matchDevServer(cmdline string) string {
	lower := strings.ToLower(cmdline)
	if lower == "" {
		return ""
	}
	for _, sig := range devServerSignatures {
		if strings.Contains(lower, sig) {
			return sig
		}
	}
	// `--host` alone is ambiguous, but `--host` on a node process in a
	// node_modules/.bin path is a dev server nearly every time.
	if strings.Contains(lower, "--host") && strings.Contains(lower, "node_modules/.bin") {
		return "--host"
	}
	return ""
}

// probeDevServer fetches the port's root and looks for HMR client markers.
func probeDevServer(port int) string {
	body, err := httpGetBody("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return ""
	}
	for _, marker := range devServerBodyMarkers {
		if strings.Contains(body, marker) {
			return marker
		}
	}
	return ""
}

func shortTarget(proxy string) string {
	if i := strings.Index(proxy, "://"); i >= 0 {
		return proxy[i+3:]
	}
	return proxy
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
