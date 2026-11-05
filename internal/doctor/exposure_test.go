package doctor

import (
	"strings"
	"testing"

	"caravan/internal/manifest"
	"caravan/internal/tsserve"
)

// withStubExposure swaps the tailscale-state and process-table hooks.
func withStubExposure(t *testing.T, st *tsserve.Status, procs map[int]string) {
	t.Helper()
	origLoad, origList := tsLoad, listeners
	tsLoad = func() (*tsserve.Status, error) { return st, nil }
	listeners = func() map[int]string { return procs }
	t.Cleanup(func() { tsLoad, listeners = origLoad, origList })
}

// withStubBody swaps the HTTP fallback probe.
func withStubBody(t *testing.T, body string) {
	t.Helper()
	orig := httpGetBody
	httpGetBody = func(string) (string, error) { return body, nil }
	t.Cleanup(func() { httpGetBody = orig })
}

// status builds a Status from (hostPort, path, proxy, funnel) tuples.
func status(entries ...[4]string) *tsserve.Status {
	st := &tsserve.Status{
		Web:         map[string]tsserve.WebHost{},
		AllowFunnel: map[string]bool{},
	}
	for _, e := range entries {
		hostPort, path, proxy, funnel := e[0], e[1], e[2], e[3]
		if _, ok := st.Web[hostPort]; !ok {
			st.Web[hostPort] = tsserve.WebHost{Handlers: map[string]tsserve.Handler{}}
		}
		st.Web[hostPort].Handlers[path] = tsserve.Handler{Proxy: proxy}
		if funnel == "public" {
			st.AllowFunnel[hostPort] = true
		}
	}
	return st
}

func find(results []result, substr string) *result {
	for i := range results {
		if strings.Contains(results[i].name, substr) || strings.Contains(results[i].detail, substr) {
			return &results[i]
		}
	}
	return nil
}

func hasFail(results []result) bool {
	for _, r := range results {
		if r.status == statusFail {
			return true
		}
	}
	return false
}

// ── check 1: declared vs observed ────────────────────────────────────────────

// The motivating case: a manifest says tailnet, the machine says public.
func TestExposure_PublicWhenDeclaredTailnet_Fails(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:443", "/wrinkles", "http://127.0.0.1:18790", "public"}), map[int]string{18790: "python3 -m http.server 18790"})
	m := &manifest.Manifest{Services: []manifest.Service{
		{Name: "hub", Command: "x", Port: "18790", Route: "/wrinkles", Visibility: "tailnet"},
	}}
	res := checkExposure(m)
	r := find(res, "declares tailnet")
	if r == nil || r.status != statusFail {
		t.Fatalf("want a ✗ for tailnet-declared-but-public, got %+v", res)
	}
}

// Declared public and observed public is fine — someone decided.
func TestExposure_DeclaredPublicIsOK(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:443", "/app", "http://127.0.0.1:4000", "public"}), map[int]string{4000: "/usr/bin/myserver --prod"})
	m := &manifest.Manifest{Services: []manifest.Service{
		{Name: "app", Command: "x", Port: "4000", Route: "/app", Visibility: "public"},
	}}
	if hasFail(checkExposure(m)) {
		t.Errorf("declared public should not fail: %+v", checkExposure(m))
	}
}

// A public route no service declares is the drift case.
func TestExposure_UndeclaredPublicRoute_Fails(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:443", "/", "http://127.0.0.1:9999", "public"}), map[int]string{9999: "/usr/bin/mystery"})
	res := checkExposure(&manifest.Manifest{})
	r := find(res, "declared by no service")
	if r == nil || r.status != statusFail {
		t.Fatalf("want ✗ for undeclared public route, got %+v", res)
	}
}

// Undeclared but tailnet-only is a warning, not a failure.
func TestExposure_UndeclaredTailnetRoute_Warns(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:8443", "/", "http://127.0.0.1:80", "tailnet"}), map[int]string{80: "nginx"})
	res := checkExposure(&manifest.Manifest{})
	if hasFail(res) {
		t.Errorf("tailnet-only drift should warn, not fail: %+v", res)
	}
	if find(res, "declared by no service") == nil {
		t.Errorf("expected a drift row: %+v", res)
	}
}

// The checks must work with no manifest at all — that is when drift is worst.
func TestExposure_WorksWithNilManifest(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:443", "/", "http://127.0.0.1:5173", "public"}), map[int]string{5173: "node /x/node_modules/.bin/vite --port 5173 --host"})
	if !hasFail(checkExposure(nil)) {
		t.Error("want a failure with a nil manifest")
	}
}

// ── check 2: funnel → dev server ─────────────────────────────────────────────

// The exact configuration found on the real machine on 2026-07-29.
func TestExposure_FunnelToViteDevServer_Fails(t *testing.T) {
	withStubExposure(t,
		status([4]string{"h.ts.net:443", "/", "https+insecure://127.0.0.1:5173", "public"}),
		map[int]string{5173: "node /Users/m/ironhand/secondironhand/node_modules/.bin/vite --port 5173 --strictPort --host"})
	res := checkExposure(nil)
	r := find(res, "dev server")
	if r == nil || r.status != statusFail {
		t.Fatalf("want ✗ for a funnelled vite dev server, got %+v", res)
	}
	if !strings.Contains(r.detail, "funnel --set-path=/ off") {
		t.Errorf("failure should name the remedy, got %q", r.detail)
	}
}

// A dev server behind serve (not funnel) is not this check's business.
func TestExposure_DevServerOnTailnetOnly_NoDevServerFailure(t *testing.T) {
	withStubExposure(t,
		status([4]string{"h.ts.net:8443", "/", "http://127.0.0.1:5173", "tailnet"}),
		map[int]string{5173: "node node_modules/.bin/vite --host"})
	m := &manifest.Manifest{Services: []manifest.Service{
		{Name: "dev", Command: "x", Port: "5173", Route: "/", Visibility: "tailnet"},
	}}
	for _, r := range checkExposure(m) {
		if strings.Contains(r.name, "dev server") {
			t.Errorf("dev server on tailnet should not be flagged: %+v", r)
		}
	}
}

func TestMatchDevServer(t *testing.T) {
	cases := map[string]bool{
		"node /x/node_modules/.bin/vite --port 5173 --host": true,
		"npm run dev -- next dev -p 3000":                   true,
		"node /x/node_modules/.bin/astro dev":               true,
		"python3 -m flask run":                              true,
		"node /x/node_modules/.bin/serve --host build":      true, // --host + node_modules/.bin
		"python3 -m http.server 18790 --bind 127.0.0.1":     false,
		"/usr/local/bin/caddy run":                          false,
		"":                                                  false,
	}
	for cmd, want := range cases {
		if got := matchDevServer(cmd) != ""; got != want {
			t.Errorf("matchDevServer(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// When the process table is unavailable, the HMR marker in the body is the
// fallback signal.
func TestExposure_DevServerDetectedFromBodyWhenProcessUnknown(t *testing.T) {
	withStubExposure(t, status([4]string{"h.ts.net:443", "/", "http://127.0.0.1:5173", "public"}), map[int]string{})
	withStubBody(t, `<html><script type="module" src="/@vite/client"></script></html>`)
	res := checkExposure(nil)
	r := find(res, "dev server")
	if r == nil || r.status != statusFail {
		t.Fatalf("want ✗ from the body probe, got %+v", res)
	}
}

// ── absence cases ────────────────────────────────────────────────────────────

func TestExposure_NothingServed(t *testing.T) {
	withStubExposure(t, &tsserve.Status{}, map[int]string{})
	res := checkExposure(nil)
	if len(res) != 1 || res[0].status != statusOK {
		t.Errorf("want a single ✓ when nothing is served, got %+v", res)
	}
}

func TestExposure_NoTailscaleIsNotAFailure(t *testing.T) {
	orig := tsLoad
	tsLoad = func() (*tsserve.Status, error) { return nil, tsserve.ErrNoTailscale }
	t.Cleanup(func() { tsLoad = orig })
	res := checkExposure(nil)
	if hasFail(res) {
		t.Errorf("missing tailscale should not fail doctor: %+v", res)
	}
}

// ── lsof parsing ─────────────────────────────────────────────────────────────

func TestParseLsofFields(t *testing.T) {
	// Real `lsof -nP -iTCP -sTCP:LISTEN -Fpcn` shape.
	out := "p82109\ncnode\nn*:5173\np702\ncPython\nn127.0.0.1:18790\np99\ncssh\nn10.0.0.1:22->10.0.0.2:5555\n"
	ports, names := parseLsofFields(out)
	if got := ports["82109"]; len(got) != 1 || got[0] != 5173 {
		t.Errorf("node ports = %v", got)
	}
	if got := ports["702"]; len(got) != 1 || got[0] != 18790 {
		t.Errorf("python ports = %v", got)
	}
	if len(ports["99"]) != 0 {
		t.Errorf("established connection should not count as a listener: %v", ports["99"])
	}
	if names["82109"] != "node" {
		t.Errorf("names = %v", names)
	}
}
