package engine

import (
	"fmt"
	"strings"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
)

const title = "Git Config Sync"

// events decides what to tell Home Assistant after a cycle. A problem that
// repeats cycle after cycle is notified once (the key resets when the sync is
// back to idle); the secret scanner's findings are notified when their set
// changes.
func (e *Engine) events(st *State, rep *Report) []ha.Event {
	var out []ha.Event
	if ev, ok := e.stateEvent(st, rep); ok {
		key := hashKey(ev.Type, ev.Message)
		switch {
		case !ev.Persistent:
			st.LastNotified = ""
			out = append(out, ev)
		case key != st.LastNotified:
			st.LastNotified = key
			out = append(out, ev)
		}
	}
	if rep.State != StateError {
		var fps []string
		for _, f := range st.Findings {
			fps = append(fps, f.Fingerprint)
		}
		key := ""
		if len(fps) > 0 {
			key = hashKey(fps...)
		}
		if key != st.FindingsKey {
			st.FindingsKey = key
			if key != "" && rep.State != StateBlockedSecrets {
				out = append(out, ha.Event{Type: ha.EventSecretsFound, Title: title + ": possible secrets in the repo",
					Message: "The secret scanner found possible secrets in: " + findingList(st) +
						". Move them out of the repo, or add the fingerprints to .gitleaksignore if they are false positives.",
					Data: map[string]any{"findings": findingLines(st)}, Persistent: true})
			}
		}
	}
	return out
}

func (e *Engine) stateEvent(st *State, rep *Report) (ha.Event, bool) {
	data := map[string]any{"state": rep.State}
	switch rep.State {
	case StateIdle:
		if rep.SnapshotCommit == "" && !rep.Applied && len(rep.Pushed) == 0 {
			return ha.Event{}, false
		}
		msg := "Sync complete."
		if rep.Restart {
			msg += " Some changes need a Home Assistant restart."
		}
		if len(rep.CheckWarnings) > 0 {
			msg += " Home Assistant's config check reported warnings:\n" + strings.Join(rep.CheckWarnings, "\n")
		}
		return ha.Event{Type: ha.EventSyncFinished, Title: title, Message: msg, Data: data,
			Persistent: rep.Restart || len(rep.CheckWarnings) > 0}, true
	case StateConflict:
		data["paths"] = rep.ConflictPaths
		return ha.Event{Type: ha.EventConflict, Title: title + ": conflict",
			Message: "The box and " + e.cfg.MainBranch + " changed the same lines in: " + strings.Join(rep.ConflictPaths, ", ") +
				". The " + e.cfg.BoxBranch + " branch was pushed: merge it into " + e.cfg.MainBranch + " like any branch.",
			Data: data, Persistent: true}, true
	case StateReview:
		data["review"] = rep.Review
		var why string
		switch rep.Review {
		case ReviewFirstSync:
			why = "This is the first sync."
		case ReviewRootChanged:
			why = fmt.Sprintf("The root changed from %s to %s.", st.PrevRoot, e.root)
		case ReviewMassDelete:
			why = "The box deleted many files."
		}
		return ha.Event{Type: ha.EventReview, Title: title + ": review needed",
			Message: why + " Only the " + e.cfg.BoxBranch + " branch was pushed. Leave the real state in " + e.cfg.MainBranch +
				" by merging " + e.cfg.BoxBranch + " (a PR or locally); syncing resumes after that.",
			Data: data, Persistent: true}, true
	case StateMassDelete:
		data["deleted"] = rep.Deleted
		return ha.Event{Type: ha.EventMassDelete, Title: title + ": many deletions from " + e.cfg.MainBranch,
			Message: fmt.Sprintf("Applying %s would delete %d files on the box (%s). Nothing was applied. If it is intended, raise delete_guard_files or delete_guard_percent for one sync.",
				e.cfg.MainBranch, len(rep.Deleted), shortList(rep.Deleted)),
			Data: data, Persistent: true}, true
	case StateTooLarge:
		data["files"] = rep.TooLarge
		return ha.Event{Type: ha.EventTooLarge, Title: title + ": file too large",
			Message: fmt.Sprintf("Nothing was committed: these files are larger than %d MB: %s. Add them to .gitignore, or raise max_file_size_mb.",
				e.cfg.MaxFileSizeMB, strings.Join(rep.TooLarge, ", ")),
			Data: data, Persistent: true}, true
	case StatePathCollision:
		data["paths"] = rep.Collisions
		return ha.Event{Type: ha.EventPathCollision, Title: title + ": path collides with the app",
			Message: e.cfg.MainBranch + " adds top-level paths that already exist in the app's container: " + strings.Join(rep.Collisions, ", ") +
				". Nothing was applied. Move or rename them in " + e.cfg.MainBranch + ".",
			Data: data, Persistent: true}, true
	case StateBlockedSecrets:
		data["findings"] = findingLines(st)
		return ha.Event{Type: ha.EventBlockedSecrets, Title: title + ": possible secrets",
			Message: "Nothing was committed (secret_scan.mode is block) because of possible secrets in: " + findingList(st) +
				". Move them out of the repo, or add the fingerprints to .gitleaksignore if they are false positives.",
			Data: data, Persistent: true}, true
	case StateCheckFailed:
		errs := rep.CheckErrors
		if errs == "" {
			errs = st.CheckErrors
		}
		return ha.Event{Type: ha.EventCheckFailed, Title: title + ": Home Assistant's config has errors",
			Message: "The changes from " + e.cfg.MainBranch + " were applied, but Home Assistant's config check reported errors, so it was not reloaded:\n" +
				errs + "\nFix them in " + e.cfg.MainBranch + ".",
			Data: data, Persistent: true}, true
	default:
		return ha.Event{Type: ha.EventError, Title: title + ": error", Message: rep.Error, Data: data, Persistent: true}, true
	}
}

func findingLines(st *State) []string {
	var lines []string
	for _, f := range st.Findings {
		lines = append(lines, fmt.Sprintf("%s:%d (%s)", f.Path, f.Line, f.Rule))
	}
	return lines
}

func findingList(st *State) string {
	return shortList(findingLines(st))
}

// shortList joins up to 10 items.
func shortList(items []string) string {
	if len(items) <= 10 {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:10], ", ") + fmt.Sprintf(" and %d more", len(items)-10)
}
