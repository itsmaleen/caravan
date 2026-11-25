package service

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"caravan/internal/manifest"
	"caravan/internal/tsserve"
)

// ── plist ─────────────────────────────────────────────────────────────────────

func stripDoctype(plist string) string {
	return strings.Replace(plist,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`+"\n"+
			`  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`, "", 1)
}

func TestPlist_WellFormedXML(t *testing.T) {
	p := Plist("dev.caravan.service.hub", "/Users/m/wrinkles",
		"python3 -m http.server 18790", map[string]string{"TZ": "UTC"}, "/tmp/hub.log")
	var v interface{}
	if err := xml.Unmarshal([]byte(stripDoctype(p)), &v); err != nil {
		t.Fatalf("plist is not valid XML: %v\n\n%s", err, p)
	}
}

func TestPlist_EscapesShellMetacharacters(t *testing.T) {
	// A command containing XML-significant characters must not break the plist.
	p := Plist("dev.caravan.service.x", "/tmp", `sh -c 'echo "a<b>c&d"'`, nil, "/tmp/x.log")
	var v interface{}
	if err := xml.Unmarshal([]byte(stripDoctype(p)), &v); err != nil {
		t.Fatalf("unescaped command broke the plist: %v\n\n%s", err, p)
	}
	if strings.Contains(p, "a<b>c&d") {
		t.Error("command was not XML-escaped")
	}
}

func TestPlist_RunsUnderShellWithKeepAlive(t *testing.T) {
	p := Plist("l", "/tmp", "echo hi", nil, "/tmp/l.log")
	for _, want := range []string{"<string>/bin/sh</string>", "<string>-lc</string>", "<key>KeepAlive</key>", "<key>RunAtLoad</key>"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %s\n%s", want, p)
		}
	}
}

func TestPlist_OmitsWorkingDirectoryWhenEmpty(t *testing.T) {
	if strings.Contains(Plist("l", "", "echo hi", nil, "/tmp/l.log"), "WorkingDirectory") {
		t.Error("empty dir should not emit WorkingDirectory")
	}
}

func TestPlist_EnvIsDeterministic(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1", "C": "3"}
	first := Plist("l", "/tmp", "x", env, "/tmp/l.log")
	for i := 0; i < 8; i++ {
		if Plist("l", "/tmp", "x", env, "/tmp/l.log") != first {
			t.Fatal("env map iteration leaked into plist output")
		}
	}
	if strings.Index(first, "<key>A</key>") > strings.Index(first, "<key>B</key>") {
		t.Error("env keys are not sorted")
	}
}

// ── ports ─────────────────────────────────────────────────────────────────────

func TestAutoPort_DeterministicAndInRange(t *testing.T) {
	a := AutoPort("wrinkles-hub", nil)
	b := AutoPort("wrinkles-hub", nil)
	if a != b {
		t.Errorf("AutoPort not deterministic: %d vs %d", a, b)
	}
	if a < autoPortLow || a > autoPortHigh {
		t.Errorf("port %d outside [%d,%d]", a, autoPortLow, autoPortHigh)
	}
}

func TestAutoPort_WalksPastTaken(t *testing.T) {
	base := AutoPort("svc", nil)
	got := AutoPort("svc", map[int]bool{base: true})
	if got == base {
		t.Fatal("AutoPort returned a taken port")
	}
	if got < autoPortLow || got > autoPortHigh {
		t.Errorf("port %d outside range", got)
	}
}

func TestResolvePorts_ExplicitWinsOverAuto(t *testing.T) {
	// Give "b" the port that "a" would auto-allocate; explicit must keep it.
	autoForA := AutoPort("a", nil)
	svcs := []manifest.Service{
		{Name: "a", Command: "x", Port: "auto"},
		{Name: "b", Command: "x", Port: itoa(autoForA)},
	}
	got := ResolvePorts(svcs)
	if got["b"] != autoForA {
		t.Errorf("explicit port lost: got %d want %d", got["b"], autoForA)
	}
	if got["a"] == got["b"] {
		t.Errorf("auto allocation collided with an explicit port (%d)", got["a"])
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

func TestExpandCommand(t *testing.T) {
	got := ExpandCommand("python3 -m http.server {{port}} --bind 127.0.0.1", 18790)
	if got != "python3 -m http.server 18790 --bind 127.0.0.1" {
		t.Errorf("got %q", got)
	}
	if ExpandCommand("no placeholder", 123) != "no placeholder" {
		t.Error("unexpected substitution")
	}
}

// ── exposure comparison ───────────────────────────────────────────────────────

func TestExposureExceeds(t *testing.T) {
	cases := []struct {
		observed, declared string
		want               bool
	}{
		{"public", "tailnet", true}, // the failure that motivated all of this
		{"public", "private", true},
		{"tailnet", "private", true},
		{"tailnet", "tailnet", false},
		{"public", "public", false},
		{"private", "public", false}, // narrower than declared is fine
		{"tailnet", "public", false},
	}
	for _, c := range cases {
		if got := exposureExceeds(c.observed, c.declared); got != c.want {
			t.Errorf("exposureExceeds(%q,%q) = %v, want %v", c.observed, c.declared, got, c.want)
		}
	}
}

// ── manifest wiring ───────────────────────────────────────────────────────────

func TestServiceDir_PrefersDirThenWorkspace(t *testing.T) {
	m := &manifest.Manifest{
		Workspace: manifest.Workspace{Root: "/root"},
		Sync:      []manifest.Sync{{Name: "wrinkles", Local: "/data/wrinkles", Remote: "h:/x"}},
	}
	if got := m.ServiceDir(manifest.Service{Name: "a", Dir: "/explicit"}); got != "/explicit" {
		t.Errorf("explicit dir: %q", got)
	}
	if got := m.ServiceDir(manifest.Service{Name: "b", Workspace: "wrinkles"}); got != "/data/wrinkles" {
		t.Errorf("workspace dir: %q", got)
	}
	if got := m.ServiceDir(manifest.Service{Name: "c"}); got != "/root" {
		t.Errorf("fallback dir: %q", got)
	}
	if got := m.ServiceDir(manifest.Service{Name: "d", Dir: "sub"}); got != "/root/sub" {
		t.Errorf("relative dir: %q", got)
	}
}

// ── prune output ──────────────────────────────────────────────────────────────

func TestPruneUndeclared_RemovalCommandTargetsMountPort(t *testing.T) {
	origLoad, origBin := tsLoad, tsBinary
	t.Cleanup(func() { tsLoad, tsBinary = origLoad, origBin })
	tsBinary = func() string { return "tailscale" }
	tsLoad = func() (*tsserve.Status, error) {
		return &tsserve.Status{
			Web: map[string]tsserve.WebHost{
				"h.ts.net:10000": {Handlers: map[string]tsserve.Handler{"/": {Proxy: "http://127.0.0.1:5173"}}},
			},
			AllowFunnel: map[string]bool{"h.ts.net:10000": true},
		}, nil
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	perr := pruneUndeclared(&manifest.Manifest{}, true)
	w.Close()
	os.Stdout = origStdout
	if perr != nil {
		t.Fatal(perr)
	}
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "tailscale funnel --https=10000 --set-path=/ off") {
		t.Errorf("removal command must name the mount's port, got:\n%s", out)
	}
}

// ── exposure read failure ─────────────────────────────────────────────────────

func TestReadExposure_FailureIsUnknownNotPrivate(t *testing.T) {
	orig, origLaunch := tsLoad, launchctlRun
	t.Cleanup(func() { tsLoad, launchctlRun = orig, origLaunch })
	launchctlRun = func(...string) ([]byte, error) { return nil, errors.New("not loaded") }
	boom := errors.New("tailscaled not running")
	tsLoad = func() (*tsserve.Status, error) { return nil, boom }

	observed, err := readExposure()
	if !errors.Is(err, boom) || observed != nil {
		t.Fatalf("want (nil, boom), got (%v, %v)", observed, err)
	}
	row := buildRow(manifest.Service{Name: "web", Visibility: manifest.VisTailnet}, 18790, observed)
	if row.Observed != "unknown" || row.Mismatched {
		t.Errorf("failed read must report unknown without a mismatch, got %+v", row)
	}

	tsLoad = func() (*tsserve.Status, error) { return &tsserve.Status{}, nil }
	observed, err = readExposure()
	if err != nil || observed == nil {
		t.Fatalf("successful empty read must be a non-nil map, got (%v, %v)", observed, err)
	}
	row = buildRow(manifest.Service{Name: "web", Visibility: manifest.VisTailnet}, 18790, observed)
	if row.Observed != manifest.VisPrivate {
		t.Errorf("nothing served is private, got %q", row.Observed)
	}
}

// ── logs ──────────────────────────────────────────────────────────────────────

func TestCmdLogs_RejectsNonPositiveLines(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(LogPath("web")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LogPath("web"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"-1", "0"} {
		if code := cmdLogs([]string{"web", "--lines", n}); code != 2 {
			t.Errorf("--lines %s: exit %d, want 2", n, code)
		}
	}
	if code := cmdLogs([]string{"web", "--lines", "2"}); code != 0 {
		t.Errorf("--lines 2: exit %d, want 0", code)
	}
}

// ── funnel check before serve publication ─────────────────────────────────────

func TestApplyOne_RefusesTailnetPublishWhenFunnelOn443(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origLoad, origSet, origLaunch := tsLoad, tsSet, launchctlRun
	t.Cleanup(func() { tsLoad, tsSet, launchctlRun = origLoad, origSet, origLaunch })
	launchctlRun = func(...string) ([]byte, error) { return nil, nil }
	published := 0
	tsSet = func(vis, path, target string) error { published++; return nil }

	funnelOn := func(hostPort string) {
		tsLoad = func() (*tsserve.Status, error) {
			return &tsserve.Status{AllowFunnel: map[string]bool{hostPort: true}}, nil
		}
	}
	svc := manifest.Service{Name: "web", Command: "x", Port: "18790", Visibility: manifest.VisTailnet}
	m := &manifest.Manifest{Workspace: manifest.Workspace{Root: t.TempDir()}}

	funnelOn("h.ts.net:443")
	if err := applyOne(m, svc, 18790, false, false); err == nil {
		t.Fatal("want refusal when Funnel is on for :443")
	}
	if published != 0 {
		t.Errorf("tsSet must not run after a refusal, ran %d times", published)
	}

	if err := applyOne(m, svc, 18790, false, true); err != nil || published != 1 {
		t.Errorf("--allow-public should proceed: err=%v published=%d", err, published)
	}

	funnelOn("h.ts.net:10000") // other port: serve on :443 is not widened
	if err := applyOne(m, svc, 18790, false, false); err != nil {
		t.Errorf("funnel on another port must not block: %v", err)
	}

	tsLoad = func() (*tsserve.Status, error) { return nil, errors.New("access denied") }
	if err := applyOne(m, svc, 18790, false, false); err == nil {
		t.Error("unreadable serve state must fail closed")
	}
}
