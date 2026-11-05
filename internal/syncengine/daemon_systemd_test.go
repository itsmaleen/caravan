package syncengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── unit generation tests ─────────────────────────────────────────────────────

// TestSystemdUnit_Sections verifies the three sections systemd requires for an
// enable-able long-running service are all present.
func TestSystemdUnit_Sections(t *testing.T) {
	unit := systemdUnit(
		"caravan-sync-myentry.service",
		"/home/user/.local/bin/caravan",
		"/home/user/.config/caravan/caravan.toml",
		"myentry",
		"5s",
		"/home/user/.local/state/caravan/logs/myentry.log",
	)

	for _, section := range []string{"[Unit]", "[Service]", "[Install]"} {
		if !strings.Contains(unit, section) {
			t.Errorf("unit missing %s section\n%s", section, unit)
		}
	}
	// Without WantedBy, `systemctl --user enable` has nothing to link and the
	// unit silently fails to come back after a reboot.
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("unit missing WantedBy — enable would be a no-op\n%s", unit)
	}
}

// TestSystemdUnit_RestartAlways verifies the KeepAlive equivalent is set.
func TestSystemdUnit_RestartAlways(t *testing.T) {
	unit := systemdUnit("caravan-sync-foo.service", "/bin/caravan", "/m.toml", "foo", "10s", "/log/foo.log")
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("unit missing Restart=always\n%s", unit)
	}
}

// TestSystemdUnit_ArgsInOrder verifies ExecStart carries the sync command with
// its arguments in the right order.
func TestSystemdUnit_ArgsInOrder(t *testing.T) {
	unit := systemdUnit(
		"caravan-sync-photos.service",
		"/usr/local/bin/caravan",
		"/abs/manifest.toml",
		"photos",
		"30s",
		"/log/photos.log",
	)

	var execLine string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			execLine = strings.TrimPrefix(line, "ExecStart=")
		}
	}
	if execLine == "" {
		t.Fatalf("unit has no ExecStart\n%s", unit)
	}

	want := "/usr/local/bin/caravan sync --watch --interval 30s -f /abs/manifest.toml photos"
	if execLine != want {
		t.Errorf("ExecStart:\n got %q\nwant %q", execLine, want)
	}
}

// TestSystemdUnit_LogPaths verifies stdout and stderr both append to the log.
func TestSystemdUnit_LogPaths(t *testing.T) {
	logPath := "/home/user/.local/state/caravan/logs/entry.log"
	unit := systemdUnit("caravan-sync-entry.service", "/bin/caravan", "/m.toml", "entry", "5s", logPath)

	if !strings.Contains(unit, "StandardOutput=append:"+logPath) {
		t.Errorf("unit missing StandardOutput append\n%s", unit)
	}
	if !strings.Contains(unit, "StandardError=append:"+logPath) {
		t.Errorf("unit missing StandardError append\n%s", unit)
	}
}

// TestSystemdUnit_EscapesSpecials verifies a manifest path containing a space
// and a percent sign survives into ExecStart intact. An unquoted space would
// split the path into two arguments, and a bare % would be read as a systemd
// specifier — both turn into a daemon that starts and syncs the wrong thing.
func TestSystemdUnit_EscapesSpecials(t *testing.T) {
	unit := systemdUnit(
		"caravan-sync-odd.service",
		"/bin/caravan",
		"/home/user/My Configs/100% caravan.toml",
		"odd",
		"5s",
		"/log/odd.log",
	)

	if !strings.Contains(unit, `"/home/user/My Configs/100%% caravan.toml"`) {
		t.Errorf("manifest path not quoted+escaped in ExecStart\n%s", unit)
	}
}

// TestSystemdQuote covers the argument renderer directly.
func TestSystemdQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"/abs/path.toml", "/abs/path.toml"},
		{"has space", `"has space"`},
		{"100%", "100%%"},
		{`quo"te`, `"quo\"te"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := systemdQuote(c.in); got != c.want {
			t.Errorf("systemdQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSystemdLabel verifies the unit naming convention.
func TestSystemdLabel(t *testing.T) {
	got := systemdSupervisor{}.Label("myentry")
	want := "caravan-sync-myentry.service"
	if got != want {
		t.Errorf("Label: got %q want %q", got, want)
	}
}

// ── install / uninstall / status tests (with stubbed systemctl) ───────────────

// setupSystemdTest redirects SystemdUnitDir and StateDir to temp dirs, forces
// the systemd backend, and stubs systemctl + loginctl so the tests are
// hermetic and run on macOS too.
func setupSystemdTest(t *testing.T) (unitDir string, calls *[][]string) {
	t.Helper()

	unitDir = t.TempDir()
	stateDir := t.TempDir()
	logHome := t.TempDir()

	origUD := SystemdUnitDir
	origSD := StateDir
	origBackend := DaemonBackend
	SystemdUnitDir = unitDir
	StateDir = stateDir
	DaemonBackend = "systemd"
	t.Setenv("XDG_STATE_HOME", logHome)

	recorded := [][]string{}
	calls = &recorded

	origSystemctl := systemctlRun
	systemctlRun = func(args ...string) ([]byte, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recorded = append(recorded, cp)
		return []byte(""), nil
	}

	origLoginctl := loginctlRun
	loginctlRun = func(args ...string) ([]byte, error) {
		return []byte("yes\n"), nil
	}

	t.Cleanup(func() {
		SystemdUnitDir = origUD
		StateDir = origSD
		DaemonBackend = origBackend
		systemctlRun = origSystemctl
		loginctlRun = origLoginctl
	})

	return unitDir, calls
}

// hasCall reports whether systemctl was invoked with the given leading args.
func hasCall(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if len(c) < len(want) {
			continue
		}
		match := true
		for i, w := range want {
			if c[i] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSystemdDaemon_Install_WritesUnit verifies install writes the unit file
// and enables it, with daemon-reload happening before enable — the other order
// enables a unit systemd has not read yet.
func TestSystemdDaemon_Install_WritesUnit(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"photos", "/tmp/photos", "local:/tmp/photos-remote"},
	})

	if code := cmdDaemonInstall([]string{"-f", mpath, "--interval", "10s"}); code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	unitPath := filepath.Join(unitDir, "caravan-sync-photos.service")
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not created: %v", err)
	}
	if !strings.Contains(string(body), "--interval 10s") {
		t.Errorf("unit did not pick up --interval\n%s", body)
	}

	reloadIdx, enableIdx := -1, -1
	for i, c := range *calls {
		if len(c) > 0 && c[0] == "daemon-reload" && reloadIdx == -1 {
			reloadIdx = i
		}
		if len(c) > 1 && c[0] == "enable" && c[1] == "--now" && enableIdx == -1 {
			enableIdx = i
		}
	}
	if reloadIdx == -1 {
		t.Errorf("expected systemctl daemon-reload, got: %v", *calls)
	}
	if enableIdx == -1 {
		t.Errorf("expected systemctl enable --now, got: %v", *calls)
	}
	if reloadIdx != -1 && enableIdx != -1 && reloadIdx > enableIdx {
		t.Errorf("daemon-reload must precede enable, got order: %v", *calls)
	}
}

// TestSystemdDaemon_Install_SingleEntry verifies a name filter installs only
// that entry.
func TestSystemdDaemon_Install_SingleEntry(t *testing.T) {
	unitDir, _ := setupSystemdTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"alpha", "/tmp/alpha", "local:/tmp/alpha-r"},
		{"beta", "/tmp/beta", "local:/tmp/beta-r"},
	})

	if code := cmdDaemonInstall([]string{"-f", mpath, "alpha"}); code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	if _, err := os.Stat(filepath.Join(unitDir, "caravan-sync-alpha.service")); err != nil {
		t.Errorf("alpha unit missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "caravan-sync-beta.service")); err == nil {
		t.Error("beta unit should NOT be created when installing only alpha")
	}
}

// TestSystemdDaemon_Install_UpgradePath verifies an existing unit is stopped
// before being rewritten, so the running process is not left on the old
// definition.
func TestSystemdDaemon_Install_UpgradePath(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"cache", "/tmp/cache", "local:/tmp/cache-r"},
	})

	unitPath := filepath.Join(unitDir, "caravan-sync-cache.service")
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := cmdDaemonInstall([]string{"-f", mpath}); code != 0 {
		t.Fatalf("install returned code %d", code)
	}

	if !hasCall(*calls, "disable", "--now") {
		t.Errorf("expected disable --now before rewrite, got: %v", *calls)
	}
	if !hasCall(*calls, "enable", "--now") {
		t.Errorf("expected enable --now after rewrite, got: %v", *calls)
	}
}

// TestSystemdDaemon_Uninstall_RemovesUnit verifies uninstall disables the unit,
// deletes the file, and reloads so systemd forgets it.
func TestSystemdDaemon_Uninstall_RemovesUnit(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"docs", "/tmp/docs", "local:/tmp/docs-r"},
	})

	unitPath := filepath.Join(unitDir, "caravan-sync-docs.service")
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := cmdDaemonUninstall([]string{"-f", mpath}); code != 0 {
		t.Fatalf("uninstall returned code %d", code)
	}

	if _, err := os.Stat(unitPath); err == nil {
		t.Error("unit file should have been removed")
	}
	if !hasCall(*calls, "disable", "--now") {
		t.Errorf("expected disable --now, got: %v", *calls)
	}
	if !hasCall(*calls, "daemon-reload") {
		t.Errorf("expected daemon-reload after removal, got: %v", *calls)
	}
}

// TestSystemdDaemon_Status_Probes verifies status queries systemctl show and
// reports a running unit as running.
func TestSystemdDaemon_Status_Probes(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)

	// Override the stub with real systemd 245 output: MainPID comes first even
	// though ActiveState is asked for first.
	systemctlRun = func(args ...string) ([]byte, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		*calls = append(*calls, cp)
		if len(args) > 0 && args[0] == "show" {
			return []byte("MainPID=4242\nActiveState=active\n"), nil
		}
		return []byte(""), nil
	}

	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})
	if err := os.WriteFile(filepath.Join(unitDir, "caravan-sync-work.service"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := cmdDaemonStatus([]string{"-f", mpath}); code != 0 {
		t.Fatalf("status returned code %d", code)
	}

	if !hasCall(*calls, "show") {
		t.Errorf("expected systemctl show call, got: %v", *calls)
	}

	running, pid := systemdSupervisor{}.Probe("work")
	if !running {
		t.Error("Probe should report active unit as running")
	}
	if pid != "4242" {
		t.Errorf("Probe pid = %q, want 4242", pid)
	}
}

// TestSystemdProbe_InactiveAndZeroPID verifies an inactive unit is not reported
// as running and a MainPID of 0 is not shown as a PID.
func TestSystemdProbe_InactiveAndZeroPID(t *testing.T) {
	_, _ = setupSystemdTest(t)

	systemctlRun = func(args ...string) ([]byte, error) {
		return []byte("MainPID=0\nActiveState=inactive\n"), nil
	}

	running, pid := systemdSupervisor{}.Probe("work")
	if running {
		t.Error("inactive unit should not be reported as running")
	}
	if pid != "" {
		t.Errorf("MainPID 0 should yield no pid, got %q", pid)
	}
}
