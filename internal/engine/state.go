package engine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/scan"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// Estados del motor (design.md §12).
const (
	StateIdle               = "idle"
	StateConflict           = "conflict"
	StateRejected           = "rejected"
	StateBlockedSecrets     = "blocked_secrets"
	StateBootstrapReview    = "bootstrap_review"
	StateSuspiciousSnapshot = "suspicious_snapshot"
	StateError              = "error"
)

// Rejected recuerda un merge que no pasó el check: no se reintenta hasta
// que cambie main o box.
type Rejected struct {
	Main   string `json:"main"`
	Box    string `json:"box"`
	Errors string `json:"errors"`
}

// State es lo que el motor persiste entre ciclos en state.json.
type State struct {
	State     string    `json:"state"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error,omitempty"`

	BoxHead  string `json:"box_head,omitempty"`
	MainHead string `json:"main_head,omitempty"`

	ConflictPaths      []string          `json:"conflict_paths,omitempty"`
	Rejected           *Rejected         `json:"rejected,omitempty"`
	Findings           []secrets.Finding `json:"findings,omitempty"`
	Warnings           []secrets.Finding `json:"warnings,omitempty"`
	BootstrapPending   bool              `json:"bootstrap_pending,omitempty"`
	RestartRecommended bool              `json:"restart_recommended,omitempty"`
	// LastNotified identifica el último aviso que pedía intervención, para
	// no repetirlo en cada ciclo. Se limpia cuando el sync vuelve a idle.
	LastNotified string `json:"last_notified,omitempty"`

	Cache scan.Cache `json:"cache,omitempty"`
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

// LoadState lee el estado persistido de un state dir (para `hcs status`).
func LoadState(stateDir string) (*State, error) {
	return loadState(filepath.Join(stateDir, "state.json"))
}
