package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
)

// Fases del journal.
const (
	// PhaseWriting: se está escribiendo o validando. Si el proceso muere acá,
	// R no está validado y Recover vuelve a B.
	PhaseWriting = "writing"
	// PhaseChecked: el disco está en R y el check pasó; falta que el motor
	// mueva box a R. Si el proceso muere acá, Recover confirma R.
	PhaseChecked = "checked"
)

// Journal es el write-ahead log de un apply.
type Journal struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Phase string `json:"phase"`
	Ops   []Op   `json:"ops"`
}

func writeJournal(path string, j *Journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, b, 0o600)
}

func readJournal(path string) (*Journal, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("journal %s ilegible: %w", path, err)
	}
	return &j, nil
}

func removeJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Finish borra el journal después de que el motor movió box a R.
func Finish(journalPath string) error { return removeJournal(journalPath) }

// RecoverOutcome es lo que hizo Recover.
type RecoverOutcome string

const (
	// RecoverNone: no había un apply interrumpido.
	RecoverNone RecoverOutcome = "none"
	// RecoverAtTarget: el apply había terminado y validado; el disco está en
	// R. El motor tiene que mover box a Journal.To y llamar a Finish.
	RecoverAtTarget RecoverOutcome = "at_target"
	// RecoverRolledBack: el apply quedó a medias o sin validar; el disco
	// volvió a B (salvo paths editados por alguien más, que se dejan como
	// están y entran en el próximo snapshot).
	RecoverRolledBack RecoverOutcome = "rolled_back"
)

// Recover revisa si quedó un apply interrumpido y deja el disco en un estado
// consistente: todo en R (si ya estaba validado) o todo en B.
func Recover(ctx context.Context, repo *gitx.Repo, m *paths.Matcher, journalPath string) (RecoverOutcome, *Journal, error) {
	j, err := readJournal(journalPath)
	if errors.Is(err, fs.ErrNotExist) {
		return RecoverNone, nil, nil
	}
	if err != nil {
		return "", nil, err
	}

	if j.Phase == PhaseChecked {
		all := true
		for _, op := range j.Ops {
			ok, err := atState(op.BoxPath, op.NewOID, op.NewMode)
			if err != nil {
				return "", j, err
			}
			all = all && ok
		}
		if all {
			return RecoverAtTarget, j, nil
		}
	}

	// Volver a B solo los paths que quedaron en R: los que siguen en B no
	// necesitan nada, y los que no están ni en B ni en R los editó alguien y
	// no se pisan.
	var toUndo []Op
	for _, op := range j.Ops {
		ok, err := atState(op.BoxPath, op.NewOID, op.NewMode)
		if err != nil {
			return "", j, err
		}
		if ok {
			toUndo = append(toUndo, op)
		}
	}
	if err := rollback(ctx, repo, toUndo, m.Mappings()); err != nil {
		return "", j, fmt.Errorf("recuperando el apply interrumpido: %w", err)
	}
	if err := removeJournal(journalPath); err != nil {
		return "", j, err
	}
	return RecoverRolledBack, j, nil
}
