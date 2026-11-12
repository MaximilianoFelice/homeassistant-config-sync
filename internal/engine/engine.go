// Package engine runs the full sync cycle (design.md §6): fetch, snapshot,
// merge, apply, publish, reload. It persists its state in the state dir and
// guarantees that only one cycle runs at a time.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/apply"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/merge"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/scan"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/snapshot"
)

// maxAttempts is how many times a cycle is retried within a single run when
// the disk or main change midway.
const maxAttempts = 3

// Engine errors.
var (
	ErrBusy           = errors.New("a sync cycle is already running")
	ErrNotInitialized = errors.New("the state dir is not initialized (run hcs init)")
	ErrBoxDiverged    = errors.New("the remote box branch has commits the local one lacks: someone pushed to box")
	ErrTooManyRetries = errors.New("the disk or main changed on every attempt; will retry on the next cycle")
	ErrPullOnly       = errors.New("mode: pull_only is not implemented yet")
)

// Options configures the engine.
type Options struct {
	Config          *config.Config
	StateDir        string
	AllowFileRemote bool
	Checker         ha.Checker
	Reloader        ha.Reloader
	Notifier        ha.Notifier
	Now             func() time.Time

	hooks hooks
}

// hooks are injection points for tests.
type hooks struct {
	// beforePush runs right before each push (simulates main moving ahead).
	beforePush func(attempt int)
	// beforeApply runs between merge and apply (simulates an edit in the UI).
	beforeApply func(attempt int)
	// afterApply runs after a successful apply; if it returns an error, the
	// cycle exits without moving box (simulates the process dying).
	afterApply func() error
}

// Engine is the sync engine.
type Engine struct {
	cfg     *config.Config
	dir     string
	m       *paths.Matcher
	repo    *gitx.Repo
	scanner *secrets.Scanner
	checker ha.Checker
	reload  ha.Reloader
	notify  ha.Notifier
	now     func() time.Time
	hooks   hooks

	allowFile                   bool
	boxRef, trackMain, trackBox string
}

// New creates an engine. It validates the configuration.
func New(opts Options) (*Engine, error) {
	cfg := opts.Config
	if err := cfg.Validate(config.ValidateOptions{AllowFileRemote: opts.AllowFileRemote}); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}
	m, err := paths.New(cfg.Mappings)
	if err != nil {
		return nil, err
	}
	sc, err := secrets.New(cfg.SecretScan)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg: cfg, dir: opts.StateDir, m: m, scanner: sc,
		checker: opts.Checker, reload: opts.Reloader, notify: opts.Notifier,
		now: opts.Now, hooks: opts.hooks, allowFile: opts.AllowFileRemote,
		boxRef:    "refs/heads/" + cfg.BoxBranch,
		trackMain: "refs/remotes/origin/" + cfg.MainBranch,
		trackBox:  "refs/remotes/origin/" + cfg.BoxBranch,
	}
	if e.checker == nil {
		e.checker = ha.Noop{}
	}
	if e.reload == nil {
		e.reload = ha.Noop{}
	}
	if e.notify == nil {
		e.notify = ha.Noop{}
	}
	if e.now == nil {
		e.now = time.Now
	}
	gopts := gitx.Options{AllowFile: opts.AllowFileRemote, KnownHosts: e.path("ssh", "known_hosts")}
	if _, err := os.Stat(e.path("ssh", "id_ed25519")); err == nil {
		gopts.SSHKey = e.path("ssh", "id_ed25519")
	}
	e.repo = gitx.Open(e.path("repo.git"), gopts)
	return e, nil
}

func (e *Engine) path(elem ...string) string {
	return filepath.Join(append([]string{e.dir}, elem...)...)
}

// InitResult is the result of Init.
type InitResult struct {
	DeployKey string
	Created   bool
}

// Init creates the state dir, the bare repo, the deploy key and known_hosts.
// It is idempotent: it never overwrites an existing deploy key.
func (e *Engine) Init(ctx context.Context) (*InitResult, error) {
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return nil, err
	}
	r, err := gitx.InitBare(ctx, e.path("repo.git"), gitx.Options{AllowFile: true})
	if err != nil {
		return nil, err
	}
	if err := r.RequireVersion(ctx); err != nil {
		return nil, err
	}
	pub, created, err := ensureDeployKey(e.path("ssh"))
	if err != nil {
		return nil, err
	}
	if err := writeKnownHosts(e.path("ssh", "known_hosts"), e.cfg.SSHKnownHosts); err != nil {
		return nil, err
	}
	if err := ensureAPIToken(e.path("api_token")); err != nil {
		return nil, err
	}
	// Reopen with the newly created key.
	e.repo = gitx.Open(e.path("repo.git"), gitx.Options{
		AllowFile: e.allowFile, SSHKey: e.path("ssh", "id_ed25519"), KnownHosts: e.path("ssh", "known_hosts"),
	})
	return &InitResult{DeployKey: pub, Created: created}, nil
}

// SyncOptions tunes a cycle.
type SyncOptions struct {
	// DryRun computes what would happen without committing, applying or pushing.
	DryRun bool
	// AcceptBootstrap confirms the first merge of box into an existing main.
	AcceptBootstrap bool
	// AllowMassDelete confirms a mass delete in the snapshot.
	AllowMassDelete bool
}

// Report describes what happened in a cycle.
type Report struct {
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	DryRun   bool   `json:"dry_run,omitempty"`

	SnapshotCommit string               `json:"snapshot_commit,omitempty"`
	Changes        []snapshot.Change    `json:"changes,omitempty"`
	Skipped        []scan.Skipped       `json:"skipped,omitempty"`
	MergeKind      merge.Kind           `json:"merge_kind,omitempty"`
	R              string               `json:"r,omitempty"`
	Overridden     []string             `json:"overridden,omitempty"`
	ConflictPaths  []string             `json:"conflict_paths,omitempty"`
	Plan           *apply.Plan          `json:"plan,omitempty"`
	Applied        bool                 `json:"applied,omitempty"`
	Checked        bool                 `json:"checked,omitempty"`
	Reloaded       bool                 `json:"reloaded,omitempty"`
	Pushed         []string             `json:"pushed,omitempty"`
	Findings       []secrets.Finding    `json:"findings,omitempty"`
	Warnings       []secrets.Finding    `json:"warnings,omitempty"`
	Recovered      apply.RecoverOutcome `json:"recovered,omitempty"`
	Restart        bool                 `json:"restart_recommended,omitempty"`
	Error          string               `json:"error,omitempty"`
}

// Sync runs a full cycle. States that need user intervention (conflict,
// rejection, secrets…) are not errors: they come in Report.State. The error
// is for unexpected failures (network, git, disk).
func (e *Engine) Sync(ctx context.Context, so SyncOptions) (*Report, error) {
	if _, err := os.Stat(e.path("repo.git")); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	unlock, err := lock(e.path("lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := loadState(e.path("state.json"))
	if err != nil {
		return nil, err
	}
	rep := &Report{DryRun: so.DryRun}
	cerr := e.cycle(ctx, st, rep, so)
	if cerr != nil {
		rep.State, rep.Error = StateError, cerr.Error()
	}
	if so.DryRun {
		return rep, cerr
	}

	st.State = rep.State
	st.LastSync = e.now()
	st.LastError = rep.Error
	st.Findings, st.Warnings, st.ConflictPaths = rep.Findings, rep.Warnings, rep.ConflictPaths
	st.BoxHead, _ = e.repo.ResolveRef(ctx, e.boxRef)
	st.MainHead, _ = e.repo.ResolveRef(ctx, e.trackMain)

	// A problem that repeats cycle after cycle (for example, an unresolved
	// conflict or the network being down) is reported only once: otherwise,
	// with the 5-minute interval a notification would arrive every 5 minutes.
	ev, notify := eventFor(rep)
	if notify {
		key := notifyKey(ev)
		switch {
		case !ev.Persistent:
			st.LastNotified = ""
		case key == st.LastNotified:
			notify = false
		default:
			st.LastNotified = key
		}
	}
	if err := saveState(e.path("state.json"), st); err != nil && cerr == nil {
		cerr = err
	}
	if notify {
		if err := e.notify.Notify(ctx, ev); err != nil && cerr == nil {
			rep.Error = "could not notify Home Assistant: " + err.Error()
		}
	}
	return rep, cerr
}

// notifyKey identifies a notification so it is not repeated.
func notifyKey(ev ha.Event) string {
	h := sha256.Sum256([]byte(ev.Type + "\x00" + ev.Message))
	return hex.EncodeToString(h[:8])
}

func (e *Engine) cycle(ctx context.Context, st *State, rep *Report, so SyncOptions) error {
	if e.cfg.Mode == config.ModePullOnly {
		return ErrPullOnly
	}

	// 0. An interrupted apply is resolved before anything else.
	if !so.DryRun {
		outcome, j, err := apply.Recover(ctx, e.repo, e.m, e.path("apply.json"))
		if err != nil {
			return err
		}
		rep.Recovered = outcome
		if outcome == apply.RecoverAtTarget {
			if err := e.repo.UpdateRef(ctx, e.boxRef, j.To, j.From); err != nil && !errors.Is(err, gitx.ErrRefMoved) {
				return err
			}
			if err := apply.Finish(e.path("apply.json")); err != nil {
				return err
			}
		}
	}

	url := e.cfg.Repository
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		rep.Attempts = attempt

		// 1. Fetch.
		originMain, originBox, err := e.fetch(ctx, url)
		if err != nil {
			return err
		}
		localBox, err := e.repo.ResolveRef(ctx, e.boxRef)
		if err != nil {
			return err
		}
		if localBox == "" {
			switch {
			case originBox != "":
				localBox = originBox
			case originMain != "":
				// Bootstrap on an existing main: the snapshot is built with
				// main as its parent, so the main..box diff shows exactly
				// how the box differs from the repo.
				localBox = originMain
				st.BootstrapPending = true
			}
			if localBox != "" && !so.DryRun {
				if err := e.repo.UpdateRef(ctx, e.boxRef, localBox, ""); err != nil {
					return err
				}
			}
		}
		if originBox != "" && localBox != "" {
			ok, err := e.repo.IsAncestor(ctx, originBox, localBox)
			if err != nil {
				return err
			}
			if !ok {
				return ErrBoxDiverged
			}
		}

		// 2. Snapshot.
		snapRef := e.boxRef
		if so.DryRun && localBox != "" {
			snapRef = localBox // the ref may not exist yet in dry-run
		}
		sres, err := snapshot.Take(ctx, e.repo, snapshot.Options{
			Matcher: e.m, Ref: snapRef, MaxFileSize: int64(e.cfg.MaxFileSizeKB) << 10,
			DeleteGuardFiles: e.cfg.DeleteGuardFiles, DeleteGuardPercent: e.cfg.DeleteGuardPercent,
			AllowMassDelete: so.AllowMassDelete, Author: e.cfg.CommitAuthor, Now: e.now,
			Cache: st.Cache, DryRun: so.DryRun,
			BeforeCommit: func(_ context.Context, _ []snapshot.Change, contents map[string][]byte) error {
				warnings, err := e.scanner.Check(contents)
				rep.Warnings = warnings
				var be *secrets.BlockedError
				if errors.As(err, &be) {
					rep.Findings = be.Findings
				}
				return err
			},
		})
		if sres != nil {
			if !so.DryRun {
				st.Cache = sres.Cache
			}
			rep.Changes, rep.Skipped, rep.SnapshotCommit = sres.Changes, sres.Skipped, sres.Commit
		}
		switch {
		case errors.Is(err, secrets.ErrSecretsFound):
			rep.State = StateBlockedSecrets
			return nil
		case errors.Is(err, snapshot.ErrSuspicious), errors.Is(err, snapshot.ErrMassDelete):
			rep.State, rep.Error = StateSuspiciousSnapshot, err.Error()
			return nil
		case err != nil:
			return err
		}
		b := localBox
		if sres.Commit != "" {
			b = sres.Commit
		}
		if b == "" {
			// Neither remote nor box: nothing is managed yet (dry-run of an
			// empty box).
			rep.State = StateIdle
			return nil
		}

		// Pending bootstrap: publish box and wait for the review.
		if st.BootstrapPending {
			merged := false
			if originMain != "" {
				if merged, err = e.repo.IsAncestor(ctx, b, originMain); err != nil {
					return err
				}
			}
			if merged || so.AcceptBootstrap {
				st.BootstrapPending = false
			} else {
				rep.State = StateBootstrapReview
				return e.pushBox(ctx, url, b, originBox, rep, so)
			}
		}

		if e.cfg.Mode == config.ModePushOnly {
			rep.State = StateIdle
			return e.pushBox(ctx, url, b, originBox, rep, so)
		}

		// 3. Merge.
		mres, err := merge.Merge(ctx, e.repo, b, originMain, merge.Options{BoxWins: e.boxWins, Now: e.now})
		if err != nil {
			return err
		}
		rep.MergeKind, rep.R, rep.Overridden = mres.Kind, mres.R, mres.Overridden
		if mres.Kind == merge.Conflict {
			rep.State, rep.ConflictPaths = StateConflict, mres.ConflictPaths
			return e.pushBox(ctx, url, b, originBox, rep, so)
		}
		r := mres.R
		if st.Rejected != nil && st.Rejected.Main == originMain && st.Rejected.Box == b {
			rep.State, rep.Error = StateRejected, st.Rejected.Errors
			return e.pushBox(ctx, url, b, originBox, rep, so)
		}

		// 4. Apply.
		plan, err := apply.MakePlan(ctx, e.repo, e.m, b, r)
		if err != nil {
			return err
		}
		rep.Plan = plan
		if so.DryRun {
			rep.State = st.State
			if rep.State == "" {
				rep.State = StateIdle
			}
			return nil
		}
		var ares *apply.Result
		if len(plan.Ops) > 0 {
			if e.hooks.beforeApply != nil {
				e.hooks.beforeApply(attempt)
			}
			ares, err = apply.Apply(ctx, e.repo, plan, apply.Options{
				Matcher: e.m, Checker: e.checker, JournalPath: e.path("apply.json"), Warnings: e.cfg.CheckWarnings,
			})
			if ares != nil {
				rep.Checked = ares.Checked
			}
			switch {
			case errors.Is(err, apply.ErrDiskChanged):
				continue // someone edited midway: take another snapshot
			case errors.Is(err, apply.ErrPrecheck), errors.Is(err, apply.ErrRejected), errors.Is(err, apply.ErrCheckUnavailable):
				st.Rejected = &Rejected{Main: originMain, Box: b, Errors: err.Error()}
				rep.State, rep.Error = StateRejected, err.Error()
				return e.pushBox(ctx, url, b, originBox, rep, so)
			case err != nil:
				return err
			}
			rep.Applied = ares.Applied
			if e.hooks.afterApply != nil {
				if err := e.hooks.afterApply(); err != nil {
					return err
				}
			}
		}

		// 5. Publish: local box first (the disk is already at R), then the remote.
		if r != b {
			if err := e.repo.UpdateRef(ctx, e.boxRef, r, b); err != nil {
				return err
			}
		}
		if err := apply.Finish(e.path("apply.json")); err != nil {
			return err
		}
		st.Rejected = nil
		if rep.Applied {
			st.RestartRecommended = e.needsRestart(plan)
		}
		rep.Restart = st.RestartRecommended

		var refspecs []string
		if originMain != r {
			refspecs = append(refspecs, r+":refs/heads/"+e.cfg.MainBranch)
		}
		if originBox != r {
			refspecs = append(refspecs, r+":refs/heads/"+e.cfg.BoxBranch)
		}
		if e.hooks.beforePush != nil {
			e.hooks.beforePush(attempt)
		}
		if err := e.repo.Push(ctx, url, refspecs...); err != nil {
			if errors.Is(err, gitx.ErrPushRejected) {
				continue // main moved ahead: merge again against the new one
			}
			return err
		}
		rep.Pushed = refspecs
		// The tracking refs reflect what we just published.
		if err := e.track(ctx, e.trackMain, r, originMain); err != nil {
			return err
		}
		if err := e.track(ctx, e.trackBox, r, originBox); err != nil {
			return err
		}

		// 6. Reload.
		if rep.Applied && rep.Checked && e.cfg.Reload == config.ReloadAuto {
			if err := e.reload.ReloadAll(ctx); err != nil {
				rep.Error = "the sync finished, but the reload failed: " + err.Error()
			} else {
				rep.Reloaded = true
			}
		}
		rep.State = StateIdle
		return nil
	}
	return ErrTooManyRetries
}

// fetch syncs the tracking refs with the remote and returns their tips (""
// if the branch does not exist).
func (e *Engine) fetch(ctx context.Context, url string) (originMain, originBox string, err error) {
	heads, err := e.repo.LsRemote(ctx, url)
	if err != nil {
		return "", "", err
	}
	var refspecs []string
	for _, b := range []struct{ name, track string }{{e.cfg.MainBranch, e.trackMain}, {e.cfg.BoxBranch, e.trackBox}} {
		if _, ok := heads[b.name]; ok {
			refspecs = append(refspecs, "+refs/heads/"+b.name+":"+b.track)
		} else if err := e.repo.DeleteRef(ctx, b.track); err != nil {
			return "", "", err
		}
	}
	if len(refspecs) > 0 {
		if err := e.repo.Fetch(ctx, url, refspecs...); err != nil {
			return "", "", err
		}
	}
	if originMain, err = e.repo.ResolveRef(ctx, e.trackMain); err != nil {
		return "", "", err
	}
	if originBox, err = e.repo.ResolveRef(ctx, e.trackBox); err != nil {
		return "", "", err
	}
	return originMain, originBox, nil
}

// pushBox publishes only the box branch (states that stop before main).
func (e *Engine) pushBox(ctx context.Context, url, b, originBox string, rep *Report, so SyncOptions) error {
	if so.DryRun || b == originBox {
		return nil
	}
	spec := b + ":refs/heads/" + e.cfg.BoxBranch
	if err := e.repo.Push(ctx, url, spec); err != nil {
		return err
	}
	rep.Pushed = []string{spec}
	if err := e.track(ctx, e.trackBox, b, originBox); err != nil {
		return err
	}
	return nil
}

// track moves a tracking ref after a successful push.
func (e *Engine) track(ctx context.Context, ref, to, from string) error {
	if to == from {
		return nil
	}
	return e.repo.UpdateRef(ctx, ref, to, from)
}

func (e *Engine) boxWins(repoPath string) bool {
	r := e.m.ClassifyRepoPath(repoPath)
	return r.Managed && r.Direction == paths.PushOnly
}

// needsRestart flags the changes that reload_all does not cover (design.md §7).
func (e *Engine) needsRestart(plan *apply.Plan) bool {
	mappings := e.m.Mappings()
	for _, op := range plan.Ops {
		if !mappings[op.Mapping].Check {
			continue
		}
		rel := e.m.ClassifyRepoPath(op.RepoPath).Rel
		if rel == "configuration.yaml" || strings.HasPrefix(rel, "packages/") || strings.HasPrefix(rel, "custom_components/") {
			return true
		}
	}
	return false
}

// eventFor decides what to tell Home Assistant after a cycle.
func eventFor(rep *Report) (ha.Event, bool) {
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
		return ha.Event{Type: ha.EventSyncFinished, Title: "Git Config Sync", Message: msg, Data: data, Persistent: rep.Restart}, true
	case StateConflict:
		data["paths"] = rep.ConflictPaths
		return ha.Event{Type: ha.EventConflict, Title: "Git Config Sync: conflict",
			Message: "There are incompatible changes between the box and main in: " + strings.Join(rep.ConflictPaths, ", ") +
				". The box branch was pushed; resolve the merge locally or on GitHub.", Data: data, Persistent: true}, true
	case StateRejected:
		return ha.Event{Type: ha.EventRejected, Title: "Git Config Sync: change rejected",
			Message: "The change from main was not applied because the config is not valid:\n" + rep.Error, Data: data, Persistent: true}, true
	case StateBlockedSecrets:
		var lines []string
		for _, f := range rep.Findings {
			lines = append(lines, f.Path+":"+fmt.Sprint(f.Line)+" ("+f.Rule+")")
		}
		data["findings"] = lines
		return ha.Event{Type: ha.EventBlockedSecrets, Title: "Git Config Sync: possible secret",
			Message: "Nothing was pushed because there are possible secrets in: " + strings.Join(lines, ", ") +
				". Move them to secrets.yaml and use !secret.", Data: data, Persistent: true}, true
	case StateBootstrapReview:
		return ha.Event{Type: ha.EventBootstrap, Title: "Git Config Sync: review the first sync",
			Message: "The box branch was pushed with the differences between your Home Assistant and the repo. Review box against main and merge it, or confirm with accept_bootstrap.",
			Data:    data, Persistent: true}, true
	case StateSuspiciousSnapshot:
		return ha.Event{Type: ha.EventSuspicious, Title: "Git Config Sync: snapshot stopped",
			Message: rep.Error, Data: data, Persistent: true}, true
	default:
		return ha.Event{Type: ha.EventError, Title: "Git Config Sync: error", Message: rep.Error, Data: data, Persistent: true}, true
	}
}
