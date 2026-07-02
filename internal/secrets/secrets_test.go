package secrets_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"caravan/internal/manifest"
	"caravan/internal/secrets"
)

// captureStdout redirects os.Stdout during f and returns the captured string.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	outC := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()
	f()
	w.Close()
	os.Stdout = orig
	out := <-outC
	r.Close()
	return out
}

// setupSecrets creates a temp dir, overrides KeyPath, builds a manifest with
// secrets.file = "secrets.enc.json", and returns (dir, manifestPath, secretsPath).
func setupSecrets(t *testing.T) (dir, manifestPath, secretsPath string) {
	t.Helper()
	dir = t.TempDir()
	secrets.KeyPath = filepath.Join(dir, "age.key")
	t.Cleanup(func() { secrets.KeyPath = "" })

	secretsPath = filepath.Join(dir, "secrets.enc.json")

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: dir},
		Repos:     []manifest.Repo{{Name: "hello", URL: "https://example.com/hello.git"}},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
	}
	manifestPath = filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatalf("Save manifest: %v", err)
	}
	return dir, manifestPath, secretsPath
}

// ── LoadStore nil cases ───────────────────────────────────────────────────

func TestLoadStoreEmpty(t *testing.T) {
	store, err := secrets.LoadStore("")
	if err != nil {
		t.Fatalf("LoadStore(\"\") error: %v", err)
	}
	if store != nil {
		t.Errorf("LoadStore(\"\") = %v, want nil", store)
	}
}

func TestLoadStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	store, err := secrets.LoadStore(filepath.Join(dir, "nonexistent.enc.json"))
	if err != nil {
		t.Fatalf("LoadStore(missing) error: %v", err)
	}
	if store != nil {
		t.Errorf("LoadStore(missing) = %v, want nil", store)
	}
}

func TestDecryptRepoEnvEmpty(t *testing.T) {
	env, err := secrets.DecryptRepoEnv("", "anyrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Errorf("expected nil map, got %v", env)
	}
}

// ── secrets init ──────────────────────────────────────────────────────────

func TestSecretsInit(t *testing.T) {
	dir, manifestPath, secretsPath := setupSecrets(t)

	out := captureStdout(t, func() {
		code := secrets.CmdSecrets([]string{"init", "-f", manifestPath})
		if code != 0 {
			t.Errorf("secrets init returned %d", code)
		}
	})

	// Key file should exist with 0600 permissions.
	fi, err := os.Stat(filepath.Join(dir, "age.key"))
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perm = %o, want 0600", fi.Mode().Perm())
	}

	// Secrets file should exist.
	if _, err := os.Stat(secretsPath); err != nil {
		t.Fatalf("secrets file not created: %v", err)
	}

	// Output should mention the public key.
	if !strings.Contains(out, "age1") {
		t.Errorf("expected public key in output; got:\n%s", out)
	}
}

func TestSecretsInitIdempotent(t *testing.T) {
	_, manifestPath, _ := setupSecrets(t)

	captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"init", "-f", manifestPath}); code != 0 {
			t.Errorf("first init returned %d", code)
		}
	})
	out := captureStdout(t, func() {
		if code := secrets.CmdSecrets([]string{"init", "-f", manifestPath}); code != 0 {
			t.Errorf("second init returned %d", code)
		}
	})

	// Should say "existing key" and "already exists".
	if !strings.Contains(out, "existing") {
		t.Errorf("expected 'existing' in second init output; got:\n%s", out)
	}
}

// ── secrets set / show round-trip ─────────────────────────────────────────

func TestSecretsSetShow(t *testing.T) {
	_, manifestPath, secretsPath := setupSecrets(t)

	captureStdout(t, func() {
		secrets.CmdSecrets([]string{"init", "-f", manifestPath})
	})

	// Set a value.
	captureStdout(t, func() {
		code := secrets.CmdSecrets([]string{"set", "-f", manifestPath, "hello", "API_KEY", "supersecret"})
		if code != 0 {
			t.Errorf("secrets set returned %d", code)
		}
	})

	// Show masked.
	maskedOut := captureStdout(t, func() {
		code := secrets.CmdSecrets([]string{"show", "-f", manifestPath, "hello"})
		if code != 0 {
			t.Errorf("secrets show returned %d", code)
		}
	})
	if strings.Contains(maskedOut, "supersecret") {
		t.Errorf("value should be masked; got:\n%s", maskedOut)
	}
	if !strings.Contains(maskedOut, "****") {
		t.Errorf("expected **** mask; got:\n%s", maskedOut)
	}

	// Show revealed (--reveal must precede the positional repo arg).
	revealedOut := captureStdout(t, func() {
		secrets.CmdSecrets([]string{"show", "-f", manifestPath, "--reveal", "hello"})
	})
	if !strings.Contains(revealedOut, "supersecret") {
		t.Errorf("expected revealed value; got:\n%s", revealedOut)
	}

	// Verify via DecryptRepoEnv.
	env, err := secrets.DecryptRepoEnv(secretsPath, "hello")
	if err != nil {
		t.Fatalf("DecryptRepoEnv: %v", err)
	}
	if env["API_KEY"] != "supersecret" {
		t.Errorf("API_KEY = %q, want supersecret", env["API_KEY"])
	}
}

func TestSecretsSetMultiple(t *testing.T) {
	_, manifestPath, secretsPath := setupSecrets(t)
	captureStdout(t, func() { secrets.CmdSecrets([]string{"init", "-f", manifestPath}) })

	keys := map[string]string{
		"KEY_ONE": "value1",
		"KEY_TWO": "value2",
	}
	for k, v := range keys {
		captureStdout(t, func() {
			secrets.CmdSecrets([]string{"set", "-f", manifestPath, "myrepo", k, v})
		})
	}

	env, err := secrets.DecryptRepoEnv(secretsPath, "myrepo")
	if err != nil {
		t.Fatalf("DecryptRepoEnv: %v", err)
	}
	for k, want := range keys {
		if got := env[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestDecryptRepoEnvMissingRepo(t *testing.T) {
	_, manifestPath, secretsPath := setupSecrets(t)
	captureStdout(t, func() { secrets.CmdSecrets([]string{"init", "-f", manifestPath}) })
	captureStdout(t, func() {
		secrets.CmdSecrets([]string{"set", "-f", manifestPath, "hello", "K", "V"})
	})

	env, err := secrets.DecryptRepoEnv(secretsPath, "noexist")
	if err != nil {
		t.Fatalf("DecryptRepoEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("expected empty map for missing repo, got %v", env)
	}
}

// ── add-machine / decrypt with second key ─────────────────────────────────

func TestSecretsAddMachine(t *testing.T) {
	dir, manifestPath, secretsPath := setupSecrets(t)
	captureStdout(t, func() { secrets.CmdSecrets([]string{"init", "-f", manifestPath}) })
	captureStdout(t, func() {
		secrets.CmdSecrets([]string{"set", "-f", manifestPath, "hello", "SECRET", "hello-world"})
	})

	// Generate a second identity externally.
	id2, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	pubkey2 := id2.Recipient().String()

	// Add second machine.
	out := captureStdout(t, func() {
		code := secrets.CmdSecrets([]string{"add-machine", "-f", manifestPath, pubkey2})
		if code != 0 {
			t.Errorf("add-machine returned %d", code)
		}
	})
	if !strings.Contains(out, pubkey2[:10]) {
		t.Errorf("expected pubkey mention in output; got:\n%s", out)
	}

	// Write id2 to a temp key file and switch KeyPath.
	keyFile2 := filepath.Join(dir, "age2.key")
	if err := os.WriteFile(keyFile2, []byte(id2.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets.KeyPath = keyFile2

	// Decrypt with second key.
	env, err := secrets.DecryptRepoEnv(secretsPath, "hello")
	if err != nil {
		t.Fatalf("DecryptRepoEnv with key2: %v", err)
	}
	if env["SECRET"] != "hello-world" {
		t.Errorf("SECRET = %q, want hello-world", env["SECRET"])
	}
}

func TestSecretsAddMachineIdempotent(t *testing.T) {
	_, manifestPath, _ := setupSecrets(t)
	captureStdout(t, func() { secrets.CmdSecrets([]string{"init", "-f", manifestPath}) })

	id2, _ := age.GenerateX25519Identity()
	pubkey2 := id2.Recipient().String()

	captureStdout(t, func() {
		secrets.CmdSecrets([]string{"add-machine", "-f", manifestPath, pubkey2})
	})
	out := captureStdout(t, func() {
		code := secrets.CmdSecrets([]string{"add-machine", "-f", manifestPath, pubkey2})
		if code != 0 {
			t.Errorf("idempotent add-machine returned %d", code)
		}
	})

	if !strings.Contains(out, "already") {
		t.Errorf("expected 'already' message; got:\n%s", out)
	}
}

func TestSecretsAddMachineInvalidKey(t *testing.T) {
	_, manifestPath, _ := setupSecrets(t)
	captureStdout(t, func() { secrets.CmdSecrets([]string{"init", "-f", manifestPath}) })

	code := secrets.CmdSecrets([]string{"add-machine", "-f", manifestPath, "not-a-valid-key"})
	if code == 0 {
		t.Error("expected non-zero exit for invalid pubkey")
	}
}

// ── subcommand dispatch ───────────────────────────────────────────────────

func TestSecretsUnknownSubcommand(t *testing.T) {
	code := secrets.CmdSecrets([]string{"bogus"})
	if code == 0 {
		t.Error("expected non-zero exit for unknown subcommand")
	}
}

func TestSecretsNoSubcommand(t *testing.T) {
	code := secrets.CmdSecrets([]string{})
	if code == 0 {
		t.Error("expected non-zero exit for no subcommand")
	}
}

// ── secrets set before init ───────────────────────────────────────────────

func TestSecretsSetBeforeInit(t *testing.T) {
	dir := t.TempDir()
	secrets.KeyPath = filepath.Join(dir, "age.key")
	t.Cleanup(func() { secrets.KeyPath = "" })

	m := &manifest.Manifest{
		Version:   1,
		Workspace: manifest.Workspace{Root: dir},
		Repos:     []manifest.Repo{{Name: "r", URL: "https://x.com/r.git"}},
		Secrets:   manifest.Secrets{File: "secrets.enc.json"},
	}
	manifestPath := filepath.Join(dir, "caravan.toml")
	if err := manifest.Save(manifestPath, m); err != nil {
		t.Fatal(err)
	}

	// set without init should return non-zero (file missing → store nil).
	code := secrets.CmdSecrets([]string{"set", "-f", manifestPath, "r", "K", "V"})
	if code == 0 {
		t.Error("expected non-zero for set before init")
	}
}
