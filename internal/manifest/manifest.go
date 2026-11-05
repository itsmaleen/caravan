// Package manifest defines the caravan.toml schema, loading/saving, and path
// resolution shared by every command.
package manifest

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
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
	Services  []Service `toml:"service"`

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

	// Split-remote routing (optional).
	PublicRemote    string   `toml:"public_remote,omitempty"`
	PrivateRemote   string   `toml:"private_remote,omitempty"`
	PrivateURL      string   `toml:"private_url,omitempty"`
	PrivateBranches []string `toml:"private_branches,omitempty"`
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

// Service is a long-running process caravan supervises on this machine, plus
// the exposure it is *declared* to have. Exposure living in the manifest —
// next to how the thing runs — is the whole point: it makes "who can reach
// this" reconcilable, so drift is detectable instead of accumulating.
type Service struct {
	Name string `toml:"name"`

	// Working directory. Dir wins; otherwise Workspace names a [[sync]] entry
	// and its Local path is used; otherwise workspace root.
	Dir       string `toml:"dir,omitempty"`
	Workspace string `toml:"workspace,omitempty"`

	// Command is run via `/bin/sh -lc`. The literal token {{port}} is replaced
	// with the resolved port.
	Command string            `toml:"command"`
	Env     map[string]string `toml:"env,omitempty"`

	// Port is a number, "auto" (deterministically allocated), or empty (the
	// service listens on nothing caravan needs to know about).
	Port string `toml:"port,omitempty"`

	// Route is the path prefix to publish under (e.g. "/wrinkles"). Empty
	// means "/" when a visibility other than private is set.
	Route string `toml:"route,omitempty"`

	// Visibility is private | tailnet | public. Defaults to private.
	Visibility string `toml:"visibility,omitempty"`

	// Health is a probe spec, "GET / 200". Empty means no probe.
	Health string `toml:"health,omitempty"`

	// Auth is an advisory marker; "tailscale-identity" records that the
	// service rejects requests without a Tailscale-User-Login header.
	Auth string `toml:"auth,omitempty"`
}

// Visibility levels.
const (
	VisPrivate = "private" // localhost only, never published
	VisTailnet = "tailnet" // tailscale serve — identity headers ARE forwarded
	VisPublic  = "public"  // tailscale funnel — public internet, NO identity headers
)

// Vis returns the effective visibility, defaulting to private. Defaulting to
// the closed state is deliberate: an omitted field must never widen exposure.
func (s Service) Vis() string {
	if s.Visibility == "" {
		return VisPrivate
	}
	return s.Visibility
}

// RoutePath returns the effective route, defaulting to "/".
func (s Service) RoutePath() string {
	if s.Route == "" {
		return "/"
	}
	return s.Route
}

// PortIsAuto reports whether the port should be allocated by caravan.
func (s Service) PortIsAuto() bool { return strings.EqualFold(s.Port, "auto") }

// ParseHealth splits a health spec like "GET /healthz 200" into its parts.
// A bare path ("/") is accepted and means "GET <path> 200".
func ParseHealth(spec string) (method, path string, code int, err error) {
	f := strings.Fields(spec)
	switch len(f) {
	case 1:
		return "GET", f[0], 200, nil
	case 2:
		c, convErr := strconv.Atoi(f[1])
		if convErr != nil {
			return "", "", 0, fmt.Errorf("health %q: %q is not a status code", spec, f[1])
		}
		return "GET", f[0], c, nil
	case 3:
		c, convErr := strconv.Atoi(f[2])
		if convErr != nil {
			return "", "", 0, fmt.Errorf("health %q: %q is not a status code", spec, f[2])
		}
		return strings.ToUpper(f[0]), f[1], c, nil
	default:
		return "", "", 0, fmt.Errorf("health %q: want \"[METHOD] PATH [CODE]\"", spec)
	}
}

// ServiceDir returns the absolute working directory for a service.
func (m *Manifest) ServiceDir(s Service) string {
	if s.Dir != "" {
		p := ExpandPath(s.Dir)
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(ExpandPath(m.Workspace.Root), p)
	}
	if s.Workspace != "" {
		for _, sy := range m.Sync {
			if sy.Name == s.Workspace {
				return ExpandPath(sy.Local)
			}
		}
		for _, r := range m.Repos {
			if r.Name == s.Workspace {
				return m.RepoDir(r)
			}
		}
	}
	return ExpandPath(m.Workspace.Root)
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
//
// "._*" is AppleDouble sidecars. macOS tar never archives a "._X" member (it
// treats it as X's metadata), so a pull of one exits 0 having moved nothing,
// the next scan sees it missing again, and it is re-pulled on every pass
// forever. Excluding it also stops one host's sidecars spreading to the rest.
var DefaultExcludes = []string{".git", "node_modules", ".DS_Store", "._*", "dist", "target", ".next", ".cache", ".caravan-staging"}

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

		// Split-remote validation.
		if (r.PrivateRemote == "") != (r.PrivateURL == "") {
			return fmt.Errorf("repo %q: private_remote and private_url must both be set or both be empty", r.Name)
		}
		if r.RoutingEnabled() {
			for _, glob := range r.PrivateBranchGlobs() {
				if matched, _ := path.Match(glob, "main"); matched {
					return fmt.Errorf("repo %q: private_branches glob %q would match 'main'", r.Name, glob)
				}
			}
		}
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
	ports := map[int]string{}
	for _, s := range m.Services {
		if s.Name == "" || s.Command == "" {
			return fmt.Errorf("service entries need name and command (got name=%q)", s.Name)
		}
		if seen["service:"+s.Name] {
			return fmt.Errorf("duplicate service name %q", s.Name)
		}
		seen["service:"+s.Name] = true

		switch s.Vis() {
		case VisPrivate, VisTailnet, VisPublic:
		default:
			return fmt.Errorf("service %q: visibility %q must be one of private, tailnet, public", s.Name, s.Visibility)
		}
		if s.Port != "" && !s.PortIsAuto() {
			p, err := strconv.Atoi(s.Port)
			if err != nil || p < 1 || p > 65535 {
				return fmt.Errorf("service %q: port %q must be a number in 1-65535 or \"auto\"", s.Name, s.Port)
			}
			if other, dup := ports[p]; dup {
				return fmt.Errorf("service %q: port %d already claimed by service %q", s.Name, p, other)
			}
			ports[p] = s.Name
		}
		if s.Vis() != VisPrivate && s.Port == "" {
			return fmt.Errorf("service %q: visibility %q needs a port to publish", s.Name, s.Vis())
		}
		if s.Route != "" && !strings.HasPrefix(s.Route, "/") {
			return fmt.Errorf("service %q: route %q must start with /", s.Name, s.Route)
		}
		if s.Health != "" {
			if _, _, _, err := ParseHealth(s.Health); err != nil {
				return fmt.Errorf("service %q: %w", s.Name, err)
			}
		}
	}
	return nil
}

// RoutingEnabled reports whether split-remote routing is configured for this repo.
func (r Repo) RoutingEnabled() bool {
	return r.PrivateRemote != "" && r.PrivateURL != ""
}

// defaultPrivateGlobs are the default private-branch patterns when routing is
// enabled but no explicit globs are configured.
// DefaultPrivateGlobs are the private-branch patterns applied when routing is
// enabled but no explicit globs are set. Exported so the pre-push guardrail can
// fail closed against them even when the manifest can't be loaded.
var DefaultPrivateGlobs = []string{"private/*", "embargo/*", "secret/*"}

// PrivateBranchGlobs returns the configured private-branch globs, or the
// defaults when routing is enabled but no globs are set.
func (r Repo) PrivateBranchGlobs() []string {
	if !r.RoutingEnabled() {
		return nil
	}
	if len(r.PrivateBranches) == 0 {
		return DefaultPrivateGlobs
	}
	return r.PrivateBranches
}

// IsPrivateBranch reports whether name matches any of the repo's private-branch globs.
func (r Repo) IsPrivateBranch(name string) bool {
	for _, glob := range r.PrivateBranchGlobs() {
		if matched, _ := path.Match(glob, name); matched {
			return true
		}
	}
	return false
}

// PublicRemoteOr returns the configured public remote name, or "origin" if unset.
func (r Repo) PublicRemoteOr() string {
	if r.PublicRemote != "" {
		return r.PublicRemote
	}
	return "origin"
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
