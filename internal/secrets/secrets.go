// Package secrets implements age-encrypted JSON secret management for caravan.
//
// File layout on disk (plaintext JSON before encryption):
//
//	{"recipients":["age1..."],"repos":{"reponame":{"KEY":"VALUE"}}}
//
// Encrypted with filippo.io/age in binary format to all listed recipients.
// Machine identity lives at ~/.config/caravan/age.key (or secrets.KeyPath).
package secrets

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"filippo.io/age"

	"caravan/internal/manifest"
)

// KeyPath overrides the machine key file path.
// Empty means ~/.config/caravan/age.key is used at call time.
// Tests should set this to a temp path and reset it via t.Cleanup.
var KeyPath = ""

// keyPath resolves the effective key file path.
func keyPath() string {
	if KeyPath != "" {
		return KeyPath
	}
	return manifest.ExpandPath("~/.config/caravan/age.key")
}

// Store is the plaintext representation of the secrets file.
type Store struct {
	Recipients []string                     `json:"recipients"`
	Repos      map[string]map[string]string `json:"repos"`
}

// LoadStore decrypts and parses the secrets file at path.
// Returns (nil, nil) when path == "" or the file does not exist.
// Returns an error if the file exists but cannot be decrypted or parsed.
func LoadStore(path string) (*Store, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	id, err := loadIdentity()
	if err != nil {
		return nil, fmt.Errorf("loading machine key: %w", err)
	}

	r, err := age.Decrypt(f, id)
	if err != nil {
		return nil, fmt.Errorf(
			"decrypting secrets: %w\n"+
				"  hint: run 'caravan secrets init' to generate your machine key,\n"+
				"        then have an existing recipient run 'caravan secrets add-machine <your-pubkey>'",
			err)
	}

	var store Store
	if err := json.NewDecoder(r).Decode(&store); err != nil {
		return nil, fmt.Errorf("parsing secrets JSON: %w", err)
	}
	return &store, nil
}

// DecryptRepoEnv returns the environment map for repo from the secrets store.
// Returns (nil, nil) when path == "" or the file does not exist.
func DecryptRepoEnv(path, repo string) (map[string]string, error) {
	store, err := LoadStore(path)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, nil
	}
	return store.Repos[repo], nil
}

// ── Internal helpers ──────────────────────────────────────────────────────

// loadIdentity reads the X25519 identity from the key file.
func loadIdentity() (*age.X25519Identity, error) {
	kp := keyPath()
	data, err := os.ReadFile(kp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("key file not found at %s (hint: run 'caravan secrets init')", kp)
		}
		return nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return age.ParseX25519Identity(line)
	}
	return nil, fmt.Errorf("no identity found in key file %s", kp)
}

// generateIdentity creates a new X25519 identity and persists it (0600) to
// the key file, writing the public key as a comment.
func generateIdentity() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}

	kp := keyPath()
	if err := os.MkdirAll(filepath.Dir(kp), 0o755); err != nil {
		return nil, err
	}

	content := fmt.Sprintf(
		"# caravan machine key — keep private, never share\n"+
			"# public key: %s\n"+
			"%s\n",
		id.Recipient().String(),
		id.String(),
	)
	if err := os.WriteFile(kp, []byte(content), 0o600); err != nil {
		return nil, err
	}
	return id, nil
}

// saveStore encrypts store and writes it atomically to path.
func saveStore(path string, store *Store) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var recipients []age.Recipient
	for _, r := range store.Recipients {
		recip, err := age.ParseX25519Recipient(r)
		if err != nil {
			return fmt.Errorf("invalid recipient %q: %w", r, err)
		}
		recipients = append(recipients, recip)
	}
	if len(recipients) == 0 {
		return fmt.Errorf("cannot save store with no recipients")
	}

	// Encrypt into a buffer then write atomically.
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return fmt.Errorf("setting up encryption: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(store); err != nil {
		return fmt.Errorf("encoding store: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalizing encryption: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".caravan-secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, &buf); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// resolveSecretsPath returns the effective secrets file path.
// It tries: manifest → Secrets.File; falls back to the caravan default.
// defaulted is true when the fallback was used.
func resolveSecretsPath(flagValue string) (path string, defaulted bool) {
	mPath := manifest.ResolvePath(flagValue)
	if m, err := manifest.Load(mPath); err == nil {
		if sp := m.SecretsPath(); sp != "" {
			return sp, false
		}
	}
	return manifest.ExpandPath("~/.config/caravan/secrets.enc.json"), true
}

// ── CmdSecrets dispatch ───────────────────────────────────────────────────

// CmdSecrets dispatches to the secrets subcommands.
func CmdSecrets(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: caravan secrets <init|set|show|add-machine>")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "init":
		return cmdSecretsInit(rest)
	case "set":
		return cmdSecretsSet(rest)
	case "show":
		return cmdSecretsShow(rest)
	case "add-machine":
		return cmdSecretsAddMachine(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown secrets subcommand: %q\nusage: caravan secrets <init|set|show|add-machine>\n", sub)
		return 2
	}
}

// ── init ──────────────────────────────────────────────────────────────────

func cmdSecretsInit(args []string) int {
	fs := flag.NewFlagSet("secrets init", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	secretsPath, defaulted := resolveSecretsPath(*f)
	if defaulted {
		fmt.Printf("no secrets file configured in manifest; using default %s\n", secretsPath)
	}

	// Load or generate the machine key.
	kp := keyPath()
	var id *age.X25519Identity
	if _, err := os.Stat(kp); os.IsNotExist(err) {
		var genErr error
		id, genErr = generateIdentity()
		if genErr != nil {
			fmt.Fprintf(os.Stderr, "error generating key: %v\n", genErr)
			return 1
		}
		fmt.Printf("generated machine key at %s\n", kp)
	} else {
		var loadErr error
		id, loadErr = loadIdentity()
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "error loading existing key: %v\n", loadErr)
			return 1
		}
		fmt.Printf("using existing key at %s\n", kp)
	}

	pubkey := id.Recipient().String()
	fmt.Printf("public key: %s\n", pubkey)

	// Create empty store if the file doesn't exist yet.
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		store := &Store{
			Recipients: []string{pubkey},
			Repos:      map[string]map[string]string{},
		}
		if err := saveStore(secretsPath, store); err != nil {
			fmt.Fprintf(os.Stderr, "error creating secrets store: %v\n", err)
			return 1
		}
		fmt.Printf("created secrets store at %s\n", secretsPath)
	} else {
		fmt.Printf("secrets store already exists at %s\n", secretsPath)
	}
	return 0
}

// ── set ───────────────────────────────────────────────────────────────────

func cmdSecretsSet(args []string) int {
	fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintln(os.Stderr, "usage: caravan secrets set [-f MANIFEST] <repo> <KEY> <VALUE>")
		return 2
	}
	repo, key, value := pos[0], pos[1], pos[2]

	secretsPath, defaulted := resolveSecretsPath(*f)
	if defaulted {
		fmt.Printf("no secrets file configured; using default %s\n", secretsPath)
	}

	store, err := LoadStore(secretsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading store: %v\n", err)
		return 1
	}
	if store == nil {
		fmt.Fprintln(os.Stderr, "secrets store not initialized; run 'caravan secrets init' first")
		return 1
	}

	if store.Repos == nil {
		store.Repos = map[string]map[string]string{}
	}
	if store.Repos[repo] == nil {
		store.Repos[repo] = map[string]string{}
	}
	store.Repos[repo][key] = value

	if err := saveStore(secretsPath, store); err != nil {
		fmt.Fprintf(os.Stderr, "error saving store: %v\n", err)
		return 1
	}
	fmt.Printf("set %s.%s\n", repo, key)
	return 0
}

// ── show ──────────────────────────────────────────────────────────────────

func cmdSecretsShow(args []string) int {
	fs := flag.NewFlagSet("secrets show", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	reveal := fs.Bool("reveal", false, "show actual values instead of masking")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	pos := fs.Args()
	var filterRepo string
	if len(pos) > 0 {
		filterRepo = pos[0]
	}

	secretsPath, defaulted := resolveSecretsPath(*f)
	if defaulted {
		fmt.Printf("no secrets file configured; using default %s\n", secretsPath)
	}

	store, err := LoadStore(secretsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading store: %v\n", err)
		return 1
	}
	if store == nil {
		fmt.Println("(no secrets file)")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tKEY\tVALUE")
	for repo, kvs := range store.Repos {
		if filterRepo != "" && repo != filterRepo {
			continue
		}
		for k, v := range kvs {
			display := "****"
			if *reveal {
				display = v
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", repo, k, display)
		}
	}
	w.Flush()
	return 0
}

// ── add-machine ───────────────────────────────────────────────────────────

func cmdSecretsAddMachine(args []string) int {
	fs := flag.NewFlagSet("secrets add-machine", flag.ContinueOnError)
	f := fs.String("f", "", "manifest path")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	pos := fs.Args()
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: caravan secrets add-machine [-f MANIFEST] <age-pubkey>")
		return 2
	}
	pubkey := pos[0]

	// Validate the public key.
	if _, err := age.ParseX25519Recipient(pubkey); err != nil {
		fmt.Fprintf(os.Stderr, "invalid age public key %q: %v\n", pubkey, err)
		return 1
	}

	secretsPath, defaulted := resolveSecretsPath(*f)
	if defaulted {
		fmt.Printf("no secrets file configured; using default %s\n", secretsPath)
	}

	store, err := LoadStore(secretsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading store: %v\n", err)
		return 1
	}
	if store == nil {
		fmt.Fprintln(os.Stderr, "secrets store not initialized; run 'caravan secrets init' first")
		return 1
	}

	// Idempotent: skip if already present.
	for _, r := range store.Recipients {
		if r == pubkey {
			fmt.Println("already a recipient — nothing to do")
			return 0
		}
	}

	store.Recipients = append(store.Recipients, pubkey)

	if err := saveStore(secretsPath, store); err != nil {
		fmt.Fprintf(os.Stderr, "error saving store: %v\n", err)
		return 1
	}

	fmt.Printf("added %s as recipient and re-encrypted.\n\n", pubkey)
	fmt.Printf("Sync %s to the other machine — it can decrypt with the key\n", secretsPath)
	fmt.Println("created by 'caravan secrets init' on that machine.")
	return 0
}
