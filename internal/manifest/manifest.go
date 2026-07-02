// Package manifest defines the caravan.toml schema, loading/saving, and path
// resolution shared by every command.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Manifest struct {
	Version   int       `toml:"version"`
	Workspace Workspace `toml:"workspace"`
	Repos     []Repo    `toml:"repos"`
	Secrets   Secrets   `toml:"secrets"`
	Toolchain Toolchain `toml:"toolchain"`
	Sync      []Sync    `toml:"sync"`

	// Dir is the directory the manifest was loaded from (not serialized);
	// relative paths like Secrets.File resolve against it.
	Dir string `toml:"-"`
}

type Workspace struct {
	Root string `toml:"root"`
}

type Repo struct {
	Name   string `toml:"name"`
	URL    string `toml:"url"`
	Path   string `toml:"path,omitempty"`
	Branch string `toml:"branch,omitempty"`
	Sparse bool   `toml:"sparse,omitempty"`
}

type Secrets struct {
	File string `toml:"file,omitempty"`
}

type Toolchain struct {
	Mise bool `toml:"mise,omitempty"`
}

type Sync struct {
	Name          string   `toml:"name"`
	Local         string   `toml:"local"`
	Remote        string   `toml:"remote"`
	Exclude       []string `toml:"exclude,omitempty"`
	Checksum      bool     `toml:"checksum,omitempty"`
	DeltaMinBytes int64    `toml:"delta_min_bytes,omitempty"`
}

// defaultDeltaThreshold is 8 MiB, used when DeltaMinBytes is 0.
const defaultDeltaThreshold = 8 * 1024 * 1024

// DeltaThreshold returns the minimum file size that qualifies for rsync delta
// transfer. 0 → default 8 MiB; -1 → delta disabled (returns MaxInt64 so no
// file ever qualifies); any other positive value is used as-is.
func (s Sync) DeltaThreshold() int64 {
	switch {
	case s.DeltaMinBytes == -1:
		return int64(^uint64(0) >> 1) // math.MaxInt64 without importing math
	case s.DeltaMinBytes == 0:
		return defaultDeltaThreshold
	default:
		return s.DeltaMinBytes
	}
}

// DefaultExcludes are applied to every sync folder in addition to its own list.
var DefaultExcludes = []string{".git", "node_modules", ".DS_Store", "dist", "target", ".next", ".cache"}

// ExpandPath expands a leading "~/" (or bare "~") to the user home directory.
func ExpandPath(s string) string {
	if s == "~" {
		h, err := os.UserHomeDir()
		if err == nil {
			return h
		}
	}
	if strings.HasPrefix(s, "~/") {
		h, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(h, s[2:])
		}
	}
	return s
}

// DefaultPath returns ~/.config/caravan/caravan.toml.
func DefaultPath() string {
	return ExpandPath("~/.config/caravan/caravan.toml")
}

// ResolvePath applies the resolution order: explicit flag > $CARAVAN_MANIFEST > default.
func ResolvePath(flagValue string) string {
	if flagValue != "" {
		return ExpandPath(flagValue)
	}
	if env := os.Getenv("CARAVAN_MANIFEST"); env != "" {
		return ExpandPath(env)
	}
	return DefaultPath()
}

func Load(path string) (*Manifest, error) {
	var m Manifest
	meta, err := toml.DecodeFile(path, &m)
	if err != nil {
		return nil, fmt.Errorf("loading manifest %s: %w", path, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		fmt.Fprintf(os.Stderr, "warning: unknown manifest keys: %v\n", undec)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	m.Dir = filepath.Dir(abs)
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	return &m, nil
}

func Save(path string, m *Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".caravan-toml-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := toml.NewEncoder(f)
	enc.Indent = ""
	if err := enc.Encode(m); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (m *Manifest) Validate() error {
	seen := map[string]bool{}
	for _, r := range m.Repos {
		if r.Name == "" || r.URL == "" {
			return fmt.Errorf("repo entries need name and url (got name=%q url=%q)", r.Name, r.URL)
		}
		if seen["repo:"+r.Name] {
			return fmt.Errorf("duplicate repo name %q", r.Name)
		}
		seen["repo:"+r.Name] = true
	}
	for _, s := range m.Sync {
		if s.Name == "" || s.Local == "" || s.Remote == "" {
			return fmt.Errorf("sync entries need name, local, remote (got name=%q)", s.Name)
		}
		if seen["sync:"+s.Name] {
			return fmt.Errorf("duplicate sync name %q", s.Name)
		}
		seen["sync:"+s.Name] = true
	}
	return nil
}

// RepoDir returns the absolute directory for a repo.
func (m *Manifest) RepoDir(r Repo) string {
	p := r.Path
	if p == "" {
		p = r.Name
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ExpandPath(m.Workspace.Root), p)
}

// SecretsPath returns the absolute path of the secrets file, or "" if unset.
func (m *Manifest) SecretsPath() string {
	if m.Secrets.File == "" {
		return ""
	}
	p := ExpandPath(m.Secrets.File)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(m.Dir, p)
}

// Excludes returns the effective exclude list for a sync entry.
func (s Sync) Excludes() []string {
	out := append([]string{}, DefaultExcludes...)
	for _, e := range s.Exclude {
		dup := false
		for _, d := range out {
			if d == e {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, e)
		}
	}
	return out
}
