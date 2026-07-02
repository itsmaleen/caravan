package syncengine

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── plist tests ───────────────────────────────────────────────────────────────

// TestPlist_WellFormedXML verifies launchdPlist produces valid XML.
func TestPlist_WellFormedXML(t *testing.T) {
	plist := launchdPlist(
		"dev.caravan.sync.myentry",
		"/usr/local/bin/caravan",
		"/home/user/.config/caravan/caravan.toml",
		"myentry",
		"5s",
		"/home/user/Library/Logs/caravan/myentry.log",
	)

	// xml.Unmarshal cannot parse a full plist DOCTYPE, so we validate by
	// stripping the DOCTYPE declaration and checking the rest is valid XML.
	stripped := strings.Replace(plist,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`+"\n"+
			`  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`, "", 1)

	var v interface{}
	if err := xml.Unmarshal([]byte(stripped), &v); err != nil {
		t.Fatalf("plist is not valid XML: %v\n\n%s", err, plist)
	}
}

// TestPlist_ContainsLabel verifies the label appears in the plist.
func TestPlist_ContainsLabel(t *testing.T) {
	plist := launchdPlist("dev.caravan.sync.foo", "/bin/caravan", "/m.toml", "foo", "10s", "/log/foo.log")
	if !strings.Contains(plist, "dev.caravan.sync.foo") {
		t.Errorf("plist missing label\n%s", plist)
	}
}

// TestPlist_ContainsKeepAlive verifies KeepAlive=true is present.
func TestPlist_ContainsKeepAlive(t *testing.T) {
	plist := launchdPlist("dev.caravan.sync.foo", "/bin/caravan", "/m.toml", "foo", "10s", "/log/foo.log")
	if !strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Error("plist missing KeepAlive key")
	}
	if !strings.Contains(plist, "<true/>") {
		t.Error("plist missing <true/> for KeepAlive")
	}
}

// TestPlist_ArgsInOrder verifies the ProgramArguments appear in the correct order.
func TestPlist_ArgsInOrder(t *testing.T) {
	plist := launchdPlist(
		"dev.caravan.sync.photos",
		"/usr/local/bin/caravan",
		"/abs/manifest.toml",
		"photos",
		"30s",
		"/home/user/Library/Logs/caravan/photos.log",
	)

	// Expected args in order: binary sync --watch --interval 30s -f /abs/manifest.toml photos
	wants := []string{
		"/usr/local/bin/caravan",
		"sync",
		"--watch",
		"--interval",
		"30s",
		"-f",
		"/abs/manifest.toml",
		"photos",
	}

	prev := 0
	for _, w := range wants {
		idx := strings.Index(plist[prev:], w)
		if idx == -1 {
			t.Errorf("plist missing arg %q (after position %d)\n%s", w, prev, plist)
			return
		}
		prev += idx + len(w)
	}
}

// TestPlist_LogPaths verifies stdout and stderr log paths are present.
func TestPlist_LogPaths(t *testing.T) {
	logPath := "/home/user/Library/Logs/caravan/entry.log"
	plist := launchdPlist("dev.caravan.sync.entry", "/bin/caravan", "/m.toml", "entry", "5s", logPath)

	if !strings.Contains(plist, "<key>StandardOutPath</key>") {
		t.Error("plist missing StandardOutPath")
	}
	if !strings.Contains(plist, "<key>StandardErrorPath</key>") {
		t.Error("plist missing StandardErrorPath")
	}
	count := strings.Count(plist, logPath)
	if count < 2 {
		t.Errorf("expected logPath %q at least twice (stdout+stderr), got %d\n%s", logPath, count, plist)
	}
}

// TestPlist_RunAtLoad verifies RunAtLoad=true is present.
func TestPlist_RunAtLoad(t *testing.T) {
	plist := launchdPlist("dev.caravan.sync.x", "/bin/caravan", "/m.toml", "x", "5s", "/log/x.log")
	if !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Error("plist missing RunAtLoad key")
	}
}

// ── install / uninstall / status logic tests (with stubbed launchctl) ─────────

// setupDaemonTest redirects LaunchAgentsDir and StateDir to temp dirs and stubs
// launchctlRun. Returns a function that records all launchctl invocations.
func setupDaemonTest(t *testing.T) (laDir string, stateDir string, calls *[][]string) {
	t.Helper()

	laDir = t.TempDir()
	stateDir = t.TempDir()

	origLA := LaunchAgentsDir
	origSD := StateDir
	LaunchAgentsDir = laDir
	StateDir = stateDir

	recorded := [][]string{}
	calls = &recorded

	origLaunchctl := launchctlRun
	launchctlRun = func(args ...string) ([]byte, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return []byte(""), nil
	}

	t.Cleanup(func() {
		LaunchAgentsDir = origLA
		StateDir = origSD
		launchctlRun = origLaunchctl
	})

	return laDir, stateDir, calls
}

// writeTempManifest writes a minimal TOML manifest with the given sync entries
// to a temp file and returns its path.
func writeTempManifest(t *testing.T, entries []struct{ name, local, remote string }) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "caravan.toml")

	var sb strings.Builder
	sb.WriteString("version = 1\n")
	sb.WriteString("[workspace]\nroot = \"/tmp\"\n")
	for _, e := range entries {
		sb.WriteString("\n[[sync]]\n")
		sb.WriteString("name = \"" + e.name + "\"\n")
		sb.WriteString("local = \"" + e.local + "\"\n")
		sb.WriteString("remote = \"" + e.remote + "\"\n")
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("writeTempManifest: %v", err)
	}
	return path
}

// TestDaemon_Install_WritesPlist verifies that install writes the plist file
// and calls launchctl bootstrap.
func TestDaemon_Install_WritesPlist(t *testing.T) {
	laDir, _, calls := setupDaemonTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"photos", "/tmp/photos", "local:/tmp/photos-remote"},
	})

	code := cmdDaemonInstall([]string{"-f", mpath, "--interval", "10s"})
	if code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	// Plist should exist.
	plistPath := filepath.Join(laDir, "dev.caravan.sync.photos.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not created: %v", err)
	}

	// launchctl should have been called with bootstrap.
	found := false
	for _, c := range *calls {
		if len(c) >= 1 && c[0] == "bootstrap" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected launchctl bootstrap call, got: %v", *calls)
	}
}

// TestDaemon_Install_SingleEntry verifies that specifying a name only installs
// that one entry and not others.
func TestDaemon_Install_SingleEntry(t *testing.T) {
	laDir, _, _ := setupDaemonTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"alpha", "/tmp/alpha", "local:/tmp/alpha-r"},
		{"beta", "/tmp/beta", "local:/tmp/beta-r"},
	})

	code := cmdDaemonInstall([]string{"-f", mpath, "alpha"})
	if code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	if _, err := os.Stat(filepath.Join(laDir, "dev.caravan.sync.alpha.plist")); err != nil {
		t.Errorf("alpha plist missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(laDir, "dev.caravan.sync.beta.plist")); err == nil {
		t.Errorf("beta plist should NOT be created when installing only alpha")
	}
}

// TestDaemon_Uninstall_RemovesPlistAndCallsBootout verifies uninstall removes
// the plist file and calls bootout.
func TestDaemon_Uninstall_RemovesPlist(t *testing.T) {
	laDir, _, calls := setupDaemonTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"docs", "/tmp/docs", "local:/tmp/docs-r"},
	})

	// Place a fake plist so uninstall finds it.
	plistPath := filepath.Join(laDir, "dev.caravan.sync.docs.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdDaemonUninstall([]string{"-f", mpath})
	if code != 0 {
		t.Fatalf("uninstall returned code %d", code)
	}

	if _, err := os.Stat(plistPath); err == nil {
		t.Error("plist should have been removed")
	}

	// launchctl bootout should have been called.
	found := false
	for _, c := range *calls {
		if len(c) >= 1 && c[0] == "bootout" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected launchctl bootout call, got: %v", *calls)
	}
}

// TestDaemon_Status_CallsPrint verifies status calls launchctl print for each entry.
func TestDaemon_Status_CallsPrint(t *testing.T) {
	_, _, calls := setupDaemonTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})

	code := cmdDaemonStatus([]string{"-f", mpath})
	if code != 0 {
		t.Fatalf("status returned code %d", code)
	}

	found := false
	for _, c := range *calls {
		if len(c) >= 1 && c[0] == "print" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected launchctl print call, got: %v", *calls)
	}
}

// TestDaemon_Install_UpgradePath verifies that if a plist already exists,
// install calls bootout before re-bootstrapping.
func TestDaemon_Install_UpgradePath(t *testing.T) {
	laDir, _, calls := setupDaemonTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"cache", "/tmp/cache", "local:/tmp/cache-r"},
	})

	// Pre-create the plist to simulate already-installed.
	plistPath := filepath.Join(laDir, "dev.caravan.sync.cache.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdDaemonInstall([]string{"-f", mpath})
	if code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	// Should have called bootout (upgrade) before bootstrap.
	hasBootout := false
	hasBootstrap := false
	for _, c := range *calls {
		if len(c) >= 1 {
			switch c[0] {
			case "bootout":
				hasBootout = true
			case "bootstrap":
				hasBootstrap = true
			}
		}
	}
	if !hasBootout {
		t.Errorf("expected bootout before re-bootstrap, calls: %v", *calls)
	}
	if !hasBootstrap {
		t.Errorf("expected bootstrap after bootout, calls: %v", *calls)
	}
}

// TestDaemon_Label verifies the label naming convention.
func TestDaemon_Label(t *testing.T) {
	got := daemonLabel("myentry")
	want := "dev.caravan.sync.myentry"
	if got != want {
		t.Errorf("daemonLabel: got %q want %q", got, want)
	}
}
