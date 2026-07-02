package syncengine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateDir is the directory for per-sync-name state files.
// If empty the default (~/.config/caravan/sync-state) is used lazily.
// Tests set this to a t.TempDir() before calling Load/SaveState.
var StateDir = ""

func resolvedStateDir() string {
	if StateDir != "" {
		return StateDir
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".caravan-sync-state"
	}
	return filepath.Join(h, ".config", "caravan", "sync-state")
}

func statePath(name string) string {
	return filepath.Join(resolvedStateDir(), name+".json")
}

// BaseEntry records what each side looked like after the last successful sync
// for a single path.  Hash (sha256 hex) is populated when checksum mode is
// enabled; after a successful sync both sides have identical content so a
// single hash suffices.
type BaseEntry struct {
	Hash   string `json:"hash,omitempty"`
	LSize  int64  `json:"lsize"`
	LMtime int64  `json:"lmtime"`
	RSize  int64  `json:"rsize"`
	RMtime int64  `json:"rmtime"`
	Dir    bool   `json:"dir"`
}

// State is the full persisted snapshot for one [[sync]] pair.
type State struct {
	Pairs    map[string]BaseEntry `json:"pairs"`
	LastSync int64                `json:"lastSync"` // UnixNano
}

// StateInfo is a summary consumed by provision.CmdStatus.
type StateInfo struct {
	LastSync time.Time // zero if never synced
}

// LoadState loads the state file for the given sync name.
// Returns an empty State on first run (no file yet).
func LoadState(name string) (*State, error) {
	data, err := os.ReadFile(statePath(name))
	if os.IsNotExist(err) {
		return &State{Pairs: map[string]BaseEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load state %q: %w", name, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %q: %w", name, err)
	}
	if s.Pairs == nil {
		s.Pairs = map[string]BaseEntry{}
	}
	return &s, nil
}

// SaveState persists state atomically via tmp-file + rename.
func SaveState(name string, s *State) error {
	dir := resolvedStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("create tmp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp state: %w", err)
	}
	return os.Rename(tmpName, statePath(name))
}

// GetStateInfo returns summary info for provision.CmdStatus.
func GetStateInfo(name string) StateInfo {
	s, err := LoadState(name)
	if err != nil || s.LastSync == 0 {
		return StateInfo{}
	}
	return StateInfo{LastSync: time.Unix(0, s.LastSync)}
}
