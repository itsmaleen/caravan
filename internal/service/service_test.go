package service

import (
	"encoding/xml"
	"strings"
	"testing"

	"caravan/internal/manifest"
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
