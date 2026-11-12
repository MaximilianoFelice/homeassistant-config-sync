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

// Journal phases.
const (
	// PhaseWriting: writing or validating. If the process dies here, R is
	// not validated and Recover goes back to B.
	PhaseWriting = "writing"
	// PhaseChecked: the disk is at R and the check passed; the engine still
	// has to move box to R. If the process dies here, Recover confirms R.
	PhaseChecked = "checked"
)

// Journal is the write-ahead log of an apply.
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
		return nil, fmt.Errorf("journal %s is unreadable: %w", path, err)
	}
	return &j, nil
}

func removeJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Finish deletes the journal after the engine moved box to R.
func Finish(journalPath string) error { return removeJournal(journalPath) }

// Undo reverts a finished apply (journal in the "checked" phase) back to B,
// for a problem found after the check, such as automations that became
// unavailable after the reload. Paths someone edited since are left alone,
// like in Recover. The journal is removed only if the rollback succeeds.
func Undo(ctx context.Context, repo *gitx.Repo, m *paths.Matcher, journalPath string) error {
	j, err := readJournal(journalPath)
	if err != nil {
		return err
	}
	var toUndo []Op
	for _, op := range j.Ops {
		ok, err := atState(op.BoxPath, op.NewOID, op.NewMode)
		if err != nil {
			return err
		}
		if ok {
			toUndo = append(toUndo, op)
		}
	}
	if err := rollback(ctx, repo, toUndo, m.Mappings()); err != nil {
		return fmt.Errorf("undoing the apply: %w", err)
	}
	return removeJournal(journalPath)
}

// RecoverOutcome is what Recover did.
type RecoverOutcome string

const (
	// RecoverNone: there was no interrupted apply.
	RecoverNone RecoverOutcome = "none"
	// RecoverAtTarget: the apply had finished and been validated; the disk is
	// at R. The engine must move box to Journal.To and call Finish.
	RecoverAtTarget RecoverOutcome = "at_target"
	// RecoverRolledBack: the apply was left half-done or unvalidated; the disk
	// went back to B (except paths edited by someone else, which are left as
	// they are and go into the next snapshot).
	RecoverRolledBack RecoverOutcome = "rolled_back"
)

// Recover checks for an interrupted apply and leaves the disk in a
// consistent state: everything at R (if already validated) or everything at B.
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

	// Revert to B only the paths left at R: those still at B need nothing,
	// and those at neither B nor R were edited by someone and are not
	// overwritten.
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
		return "", j, fmt.Errorf("recovering the interrupted apply: %w", err)
	}
	if err := removeJournal(journalPath); err != nil {
		return "", j, err
	}
	return RecoverRolledBack, j, nil
}
