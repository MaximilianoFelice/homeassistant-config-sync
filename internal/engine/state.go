package engine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// Engine states. Every state other than idle needs a person.
const (
	StateIdle = "idle"
	// StateConflict: main and the box changed the same lines.
	StateConflict = "conflict"
	// StateReview: only box was pushed; the user resolves in main (first
	// sync, root change, or many deletions from the box).
	StateReview = "review"
	// StateMassDelete: applying main would delete many files on the box.
	StateMassDelete = "mass_delete"
	// StateTooLarge: a file above max_file_size_mb would enter the repo.
	StateTooLarge = "too_large"
	// StatePathCollision: main brings a top-level path that collides with
	// the app's container.
	StatePathCollision = "path_collision"
	// StateBlockedSecrets: secret_scan.mode is block and there are findings.
	StateBlockedSecrets = "blocked_secrets"
	// StateCheckFailed: main was applied but Home Assistant's config check
	// reported errors, so it was not reloaded.
	StateCheckFailed = "check_failed"
	StateError       = "error"
)

// Review reasons.
const (
	ReviewFirstSync   = "first_sync"
	ReviewRootChanged = "root_changed"
	ReviewMassDelete  = "mass_delete"
)

// State is what the engine persists between cycles in state.json.
type State struct {
	State     string    `json:"state"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error,omitempty"`

	BoxHead  string `json:"box_head,omitempty"`
	MainHead string `json:"main_head,omitempty"`

	// Review is why only box is being pushed until the user resolves in main.
	Review string `json:"review,omitempty"`
	// PrevRoot is the root before a root change (for the review message).
	PrevRoot string `json:"prev_root,omitempty"`

	ConflictPaths []string `json:"conflict_paths,omitempty"`
	Deleted       []string `json:"deleted,omitempty"`
	TooLarge      []string `json:"too_large,omitempty"`
	Collisions    []string `json:"collisions,omitempty"`
	CheckErrors   string   `json:"check_errors,omitempty"`
	CheckWarnings []string `json:"check_warnings,omitempty"`
	// Findings are the secret scanner's current findings over the whole repo.
	Findings           []secrets.Finding `json:"findings,omitempty"`
	RestartRecommended bool              `json:"restart_recommended,omitempty"`

	// LastNotified identifies the last notification that needed a person,
	// so it is not repeated every cycle. Cleared when the sync is back to idle.
	LastNotified string `json:"last_notified,omitempty"`
	// FindingsKey identifies the set of findings last notified.
	FindingsKey string `json:"findings_key,omitempty"`
	// LastRoot is the root of the last cycle, to notice a change.
	LastRoot string `json:"last_root,omitempty"`
	// IndexAt is the commit the persistent index was last loaded from.
	IndexAt string `json:"index_at,omitempty"`
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{State: StateIdle}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveState(path string, s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadState reads the persisted state of a state dir (for `hcs status`).
func LoadState(stateDir string) (*State, error) {
	return loadState(filepath.Join(stateDir, "state.json"))
}
