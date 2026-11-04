package syncengine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSystemdQuote_EscapesDollar verifies a literal $ in an ExecStart argument
// is doubled, since systemd expands $VAR there even inside double quotes.
func TestSystemdQuote_EscapesDollar(t *testing.T) {
	if got := systemdQuote("/srv/${ARCHIVE}/caravan.toml"); got != "/srv/$${ARCHIVE}/caravan.toml" {
		t.Errorf("systemdQuote = %q, want $ doubled", got)
	}
}

// TestDaemonInstall_RejectsBadInterval verifies an interval that `sync` could
// not parse is refused before anything is written.
func TestDaemonInstall_RejectsBadInterval(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)
	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})

	if code := cmdDaemonInstall([]string{"-f", mpath, "--interval", "5s\nExecStartPre=/bin/true"}); code == 0 {
		t.Fatal("install accepted an interval containing a newline")
	}
	if _, err := os.Stat(filepath.Join(unitDir, "caravan-sync-work.service")); err == nil {
		t.Error("unit file was written for a rejected interval")
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl was called for a rejected interval: %v", *calls)
	}
}

// TestDaemonInstall_RejectsUnsafeName verifies entry names that would escape
// the unit directory, or that systemd rejects, never reach the filesystem.
func TestDaemonInstall_RejectsUnsafeName(t *testing.T) {
	for _, name := range []string{"x/../victim", "my photos", ".."} {
		t.Run(name, func(t *testing.T) {
			unitDir, _ := setupSystemdTest(t)
			mpath := writeTempManifest(t, []struct{ name, local, remote string }{
				{name, "/tmp/work", "local:/tmp/work-r"},
			})
			if code := cmdDaemonInstall([]string{"-f", mpath}); code == 0 {
				t.Fatalf("install accepted entry name %q", name)
			}
			if entries, _ := os.ReadDir(unitDir); len(entries) != 0 {
				t.Errorf("unit dir is not empty after rejecting %q: %v", name, entries)
			}
		})
	}
}

// stubStopFails makes `disable --now` fail and `show` report the given state.
func stubStopFails(calls *[][]string, show string) {
	systemctlRun = func(args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		switch args[0] {
		case "disable":
			return []byte("Failed to connect to bus"), errors.New("exit status 1")
		case "show":
			return []byte(show), nil
		}
		return nil, nil
	}
}

func writeUnit(t *testing.T, unitDir, body string) string {
	t.Helper()
	p := filepath.Join(unitDir, "caravan-sync-work.service")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDaemonUninstall_KeepsUnitWhenStopFails verifies a daemon that could not
// be stopped keeps its unit file, so it can still be managed.
func TestDaemonUninstall_KeepsUnitWhenStopFails(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)
	stubStopFails(calls, "MainPID=4242\nActiveState=active\n")
	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})
	unitPath := writeUnit(t, unitDir, "[Service]\n")

	if code := cmdDaemonUninstall([]string{"-f", mpath}); code == 0 {
		t.Fatal("uninstall reported success though the daemon is still running")
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Errorf("unit file was removed though the daemon is still running: %v", err)
	}
}

// TestDaemonUninstall_StopFailsWhenAlreadyStopped verifies a failed stop of a
// daemon that is not running still uninstalls.
func TestDaemonUninstall_StopFailsWhenAlreadyStopped(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)
	stubStopFails(calls, "MainPID=0\nActiveState=inactive\n")
	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})
	unitPath := writeUnit(t, unitDir, "[Service]\n")

	if code := cmdDaemonUninstall([]string{"-f", mpath}); code != 0 {
		t.Fatalf("uninstall returned %d for a stopped daemon", code)
	}
	if _, err := os.Stat(unitPath); err == nil {
		t.Error("unit file should have been removed")
	}
}

// TestDaemonInstall_UpgradeAbortsWhenStopFails verifies an upgrade does not
// rewrite the unit while the old daemon could not be stopped: starting a
// running unit does not restart it, so the upgrade would silently not apply.
func TestDaemonInstall_UpgradeAbortsWhenStopFails(t *testing.T) {
	unitDir, calls := setupSystemdTest(t)
	stubStopFails(calls, "MainPID=4242\nActiveState=active\n")
	mpath := writeTempManifest(t, []struct{ name, local, remote string }{
		{"work", "/tmp/work", "local:/tmp/work-r"},
	})
	unitPath := writeUnit(t, unitDir, "old\n")

	if code := cmdDaemonInstall([]string{"-f", mpath}); code == 0 {
		t.Fatal("install reported success though the old daemon could not be stopped")
	}
	if b, _ := os.ReadFile(unitPath); string(b) != "old\n" {
		t.Errorf("unit was rewritten during a failed upgrade: %q", b)
	}
	if hasCall(*calls, "enable", "--now") {
		t.Error("enable --now ran during a failed upgrade")
	}
}
