// Package engine runs the sync cycle (docs/redesign.md): fetch, snapshot,
// merge, apply, check, publish. Git does the work: the root is git's work
// tree, the repo is bare in the state dir and the index is persistent there
// too. The engine adds what git does not do: the guards, Home Assistant's
// check and reload, the secret scanner, notifications and state.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// maxAttempts is how many times a cycle is retried within a single run when
// the disk or main change midway.
const maxAttempts = 3

// Mounts are the folders the app maps (config.yaml map:), as they appear at
// the root of the app's filesystem.
var Mounts = []string{"homeassistant", "app_configs", "share", "ssl", "media", "backup", "local_apps"}

// Engine errors.
var (
	ErrBusy           = errors.New("a sync cycle is already running")
	ErrNotInitialized = errors.New("the state dir is not initialized (run hcs init)")
	ErrBoxDiverged    = errors.New("the remote box branch has commits the local one lacks: someone pushed to box")
	ErrTooManyRetries = errors.New("the disk or main changed on every attempt; will retry on the next cycle")
)

// Options configures the engine.
type Options struct {
	Config   *config.Config
	StateDir string
	// FSRoot is where the app's filesystem starts: "/" in the app, a
	// sandbox directory in tests. The root option is resolved under it.
	FSRoot          string
	AllowFileRemote bool
	Checker         ha.Checker
	Reloader        ha.Reloader
	Notifier        ha.Notifier
	Now             func() time.Time

	hooks hooks
}

// hooks are injection points for tests.
type hooks struct {
	// beforeApply runs between merge and apply (simulates an edit in the UI).
	beforeApply func(attempt int)
	// beforePush runs right before each push (simulates main moving ahead).
	beforePush func(attempt int)
}

// Engine is the sync engine.
type Engine struct {
	cfg     *config.Config
	dir     string
	fsRoot  string
	root    string // the root option, as an absolute path in the app
	repo    *gitx.Repo
	checker ha.Checker
	reload  ha.Reloader
	notify  ha.Notifier
	now     func() time.Time
	hooks   hooks
	scan    scanCache

	allowFile                   bool
	boxRef, trackMain, trackBox string
}

// New creates an engine. It validates the configuration.
func New(opts Options) (*Engine, error) {
	cfg := opts.Config
	if err := cfg.Validate(config.ValidateOptions{AllowFileRemote: opts.AllowFileRemote}); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}
	fsRoot := opts.FSRoot
	if fsRoot == "" {
		fsRoot = "/"
	}
	e := &Engine{
		cfg: cfg, dir: opts.StateDir, fsRoot: fsRoot, root: cfg.RootPath(),
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
	e.repo = gitx.Open(e.path("repo.git"), e.gitOptions())
	return e, nil
}

func (e *Engine) gitOptions() gitx.Options {
	return gitx.Options{
		AllowFile: e.allowFile, Token: e.cfg.Token, Username: e.cfg.Username, AskPass: e.path("askpass"),
	}
}

func (e *Engine) path(elem ...string) string {
	return filepath.Join(append([]string{e.dir}, elem...)...)
}

// workDir is the root on this filesystem.
func (e *Engine) workDir() string {
	return filepath.Join(e.fsRoot, filepath.FromSlash(e.root))
}

// Init creates the state dir, the bare repo, the askpass program and the API
// token. It is idempotent.
func (e *Engine) Init(ctx context.Context) error {
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return err
	}
	r, err := gitx.InitBare(ctx, e.path("repo.git"), e.gitOptions())
	if err != nil {
		return err
	}
	if err := r.RequireVersion(ctx); err != nil {
		return err
	}
	if err := gitx.WriteAskPass(e.path("askpass")); err != nil {
		return err
	}
	return ensureAPIToken(e.path("api_token"))
}

// SyncOptions tunes a cycle.
type SyncOptions struct {
	// DryRun computes what would happen without committing a ref, applying
	// or pushing.
	DryRun bool `json:"dry_run,omitempty"`
}

// Report describes what happened in a cycle.
type Report struct {
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	DryRun   bool   `json:"dry_run,omitempty"`
	Review   string `json:"review,omitempty"`

	SnapshotCommit string `json:"snapshot_commit,omitempty"`
	// Outgoing are the box's changes (box → repo).
	Outgoing []gitx.Change `json:"outgoing,omitempty"`
	// Incoming are main's changes applied (or to apply) to the box.
	Incoming      []gitx.Change     `json:"incoming,omitempty"`
	R             string            `json:"r,omitempty"`
	ConflictPaths []string          `json:"conflict_paths,omitempty"`
	Deleted       []string          `json:"deleted,omitempty"`
	TooLarge      []string          `json:"too_large,omitempty"`
	Collisions    []string          `json:"collisions,omitempty"`
	Findings      []secrets.Finding `json:"findings,omitempty"`
	Applied       bool              `json:"applied,omitempty"`
	Checked       bool              `json:"checked,omitempty"`
	CheckErrors   string            `json:"check_errors,omitempty"`
	CheckWarnings []string          `json:"check_warnings,omitempty"`
	Reloaded      bool              `json:"reloaded,omitempty"`
	Pushed        []string          `json:"pushed,omitempty"`
	Restart       bool              `json:"restart_recommended,omitempty"`
	Error         string            `json:"error,omitempty"`

	// checkRan says Home Assistant's config check ran (or was attempted).
	checkRan bool
}

// Sync runs a full cycle. States that need a person (conflict, review…) are
// not errors: they come in Report.State. The error is for unexpected
// failures (network, git, disk).
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

	st.State, st.LastSync, st.LastError = rep.State, e.now(), rep.Error
	st.ConflictPaths, st.Deleted, st.TooLarge, st.Collisions = rep.ConflictPaths, rep.Deleted, rep.TooLarge, rep.Collisions
	if rep.State != StateError || rep.Findings != nil {
		st.Findings = rep.Findings
	}
	if rep.checkRan {
		st.CheckErrors, st.CheckWarnings = rep.CheckErrors, rep.CheckWarnings
	}
	rep.Restart = st.RestartRecommended
	st.BoxHead, _ = e.repo.ResolveRef(ctx, e.boxRef)
	st.MainHead, _ = e.repo.ResolveRef(ctx, e.trackMain)

	events := e.events(st, rep)
	if err := saveState(e.path("state.json"), st); err != nil && cerr == nil {
		cerr = err
	}
	for _, ev := range events {
		if err := e.notify.Notify(ctx, ev); err != nil && rep.Error == "" {
			rep.Error = "could not notify Home Assistant: " + err.Error()
		}
	}
	return rep, cerr
}

func (e *Engine) cycle(ctx context.Context, st *State, rep *Report, so SyncOptions) error {
	if err := e.writeExclude(); err != nil {
		return err
	}
	if fi, err := os.Stat(e.workDir()); err != nil || !fi.IsDir() {
		return fmt.Errorf("the root %s is not a folder on this box", e.root)
	}
	if st.LastRoot != "" && st.LastRoot != e.root {
		st.Review, st.PrevRoot, st.IndexAt = ReviewRootChanged, st.LastRoot, ""
	}
	st.LastRoot = e.root

	index := e.path("index")
	if so.DryRun {
		// A dry run works on a copy, so the real index keeps matching box.
		tmp, err := copyIndex(index)
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp) }()
		index = tmp
	}
	wt := e.repo.WorkTree(e.workDir(), index)
	url := e.cfg.Repository
	var lastErr error

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
		if localBox == "" && originBox == "" && !so.DryRun {
			// First sync: without a .gitignore in the repo, commit the base one.
			if err := e.ensureBaseGitignore(ctx, originMain); err != nil {
				return err
			}
		}
		if localBox == "" {
			switch {
			case originBox != "":
				localBox = originBox
			case originMain != "":
				// First sync against a main with content: the snapshot goes
				// on top of main, so main..box shows how the box differs.
				localBox = originMain
				if st.Review == "" {
					st.Review = ReviewFirstSync
				}
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

		// 2. Snapshot: the index follows box; git add -A brings it to the disk.
		// Top-level files the container lost are restored only when the box
		// already holds this box's state (not on a first sync or root change).
		_, ierr := os.Stat(index)
		synced := ierr == nil && localBox != "" && st.IndexAt == localBox
		if !synced {
			if err := wt.ReadTree(ctx, localBox); err != nil {
				return err
			}
			if !so.DryRun {
				st.IndexAt = localBox
			}
		}
		if synced {
			if err := e.restoreTopLevel(ctx, wt); err != nil {
				return err
			}
		}
		tree, err := e.stage(ctx, wt)
		if err != nil {
			return err
		}
		boxTree := ""
		if localBox != "" {
			if boxTree, err = e.repo.TreeOf(ctx, localBox); err != nil {
				return err
			}
		}
		if rep.Outgoing, err = e.repo.DiffTrees(ctx, boxTree, tree); err != nil {
			return err
		}
		if rep.TooLarge, err = e.tooLarge(ctx, rep.Outgoing); err != nil {
			return err
		}
		if len(rep.TooLarge) > 0 {
			rep.State = StateTooLarge
			return nil
		}
		if e.cfg.SecretScan.Mode != config.SecretsOff {
			if rep.Findings, err = e.scanTree(ctx, tree); err != nil {
				return err
			}
			if len(rep.Findings) > 0 && e.cfg.SecretScan.Mode == config.SecretsBlock {
				rep.State = StateBlockedSecrets
				return nil
			}
		}
		b := localBox
		if len(rep.Outgoing) > 0 {
			var parents []string
			if localBox != "" {
				parents = []string{localBox}
			}
			c, err := e.commit(ctx, tree, parents, snapshotMessage(rep.Outgoing), e.cfg.CommitAuthor)
			if err != nil {
				return err
			}
			rep.SnapshotCommit, b = c, c
			if !so.DryRun {
				if err := e.repo.UpdateRef(ctx, e.boxRef, c, localBox); err != nil {
					return err
				}
				st.IndexAt = c
			}
			total, err := e.countFiles(ctx, boxTree)
			if err != nil {
				return err
			}
			if e.massDelete(len(deletions(rep.Outgoing)), total) && st.Review == "" {
				st.Review = ReviewMassDelete
			}
		}
		if b == "" {
			// Nothing on the box, nothing in the repo.
			rep.State = StateIdle
			return nil
		}

		// 3. Review: only box is pushed until the user resolves in main.
		if st.Review != "" {
			resolved, err := e.reviewResolved(ctx, st.Review, b, originMain)
			if err != nil {
				return err
			}
			if !resolved {
				rep.State, rep.Review = StateReview, st.Review
				return e.pushBox(ctx, url, b, originBox, rep, so)
			}
			st.Review, st.PrevRoot = "", ""
		}

		// 4. Merge.
		r, conflicts, err := e.merge(ctx, b, originMain)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			rep.State, rep.ConflictPaths = StateConflict, conflicts
			return e.pushBox(ctx, url, b, originBox, rep, so)
		}
		rep.R = r
		if rep.Incoming, err = e.repo.DiffTrees(ctx, b, r); err != nil {
			return err
		}

		// 5. Guards on what main brings.
		if dels := deletions(rep.Incoming); len(dels) > 0 {
			total, err := e.countFiles(ctx, b)
			if err != nil {
				return err
			}
			if e.massDelete(len(dels), total) {
				rep.State, rep.Deleted = StateMassDelete, dels
				return e.pushBox(ctx, url, b, originBox, rep, so)
			}
		}
		if rep.Collisions, err = e.collisions(ctx, rep.Incoming, b); err != nil {
			return err
		}
		if len(rep.Collisions) > 0 {
			rep.State = StatePathCollision
			return e.pushBox(ctx, url, b, originBox, rep, so)
		}
		if so.DryRun {
			rep.State = st.State
			if rep.State == "" || rep.State == StateError {
				rep.State = StateIdle
			}
			return nil
		}

		// 6. Apply: git moves the work tree from b to r, or refuses.
		if len(rep.Incoming) > 0 {
			if e.hooks.beforeApply != nil {
				e.hooks.beforeApply(attempt)
			}
			if err := wt.Update(ctx, b, r); err != nil {
				if errors.Is(err, gitx.ErrWorkTreeChanged) {
					lastErr = err
					continue // someone edited midway: take another snapshot
				}
				return err
			}
			rep.Applied = true
			st.IndexAt = r
			e.afterApply(ctx, st, rep)
		}

		// 7. Publish: local box first (the disk is already at r), then the remote.
		if r != b {
			if err := e.repo.UpdateRef(ctx, e.boxRef, r, b); err != nil {
				return err
			}
		}
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
				lastErr = err
				continue // main moved ahead: merge again against the new one
			}
			return err
		}
		rep.Pushed = refspecs
		if err := e.track(ctx, e.trackMain, r, originMain); err != nil {
			return err
		}
		if err := e.track(ctx, e.trackBox, r, originBox); err != nil {
			return err
		}
		// A config check that failed earlier is repeated until it passes:
		// the fix may come from main or be made on the box by hand.
		if !rep.checkRan && st.CheckErrors != "" && e.cfg.CheckConfig {
			e.checkAndReload(ctx, rep)
		}
		rep.State = StateIdle
		if rep.CheckErrors != "" || (!rep.checkRan && st.CheckErrors != "") {
			rep.State = StateCheckFailed
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("%w (last: %w)", ErrTooManyRetries, lastErr)
	}
	return ErrTooManyRetries
}

// stage brings the index to the disk, like `git add -A`, then drops the
// tracked paths that a .gitignore now ignores (they leave the repo, the files
// stay on disk), and writes the tree.
func (e *Engine) stage(ctx context.Context, wt *gitx.WorkTree) (string, error) {
	if err := wt.AddAll(ctx); err != nil {
		return "", err
	}
	ignored, err := wt.TrackedIgnored(ctx)
	if err != nil {
		return "", err
	}
	if err := wt.RemoveCached(ctx, ignored); err != nil {
		return "", err
	}
	return wt.WriteTree(ctx)
}

// merge merges main into box like `git merge main` on box, with the
// configured -X options and main's .gitattributes. It returns the result
// commit, or the conflicting paths.
func (e *Engine) merge(ctx context.Context, b, main string) (string, []string, error) {
	if main == "" || main == b {
		return b, nil, nil
	}
	if ok, err := e.repo.IsAncestor(ctx, main, b); err != nil || ok {
		return b, nil, err // only the box changed
	}
	if ok, err := e.repo.IsAncestor(ctx, b, main); err != nil || ok {
		return main, nil, err // only main changed: fast-forward
	}
	mt, err := e.repo.MergeTree(ctx, b, main, e.cfg.MergeOptions, main)
	if err != nil {
		return "", nil, err
	}
	if !mt.Clean {
		paths := mt.Conflicted
		if len(paths) == 0 {
			for _, m := range mt.Messages {
				if strings.HasPrefix(m.Type, "CONFLICT") {
					paths = append(paths, m.Paths...)
				}
			}
		}
		slices.Sort(paths)
		return "", slices.Compact(paths), nil
	}
	// The first parent is main, so `git log --first-parent main` tells the
	// human history.
	r, err := e.commit(ctx, mt.Tree, []string{main, b}, "Merge branch '"+e.cfg.BoxBranch+"'\n", gitx.EngineIdent)
	return r, nil, err
}

func (e *Engine) commit(ctx context.Context, tree string, parents []string, msg, author string) (string, error) {
	now := e.now()
	a, err := gitx.ParseIdent(author, now)
	if err != nil {
		return "", err
	}
	c, err := gitx.ParseIdent(gitx.EngineIdent, now)
	if err != nil {
		return "", err
	}
	return e.repo.CommitTree(ctx, tree, parents, msg, a, c)
}

// reviewResolved reports whether the user resolved a review in main.
func (e *Engine) reviewResolved(ctx context.Context, reason, b, main string) (bool, error) {
	if main == "" {
		// Nothing to resolve against: an empty repo takes the box as is.
		return reason != ReviewMassDelete, nil
	}
	if ok, err := e.repo.IsAncestor(ctx, b, main); err != nil || ok {
		return ok, err
	}
	if reason != ReviewMassDelete {
		return false, nil
	}
	// The deletions were undone on the box (or main already has them).
	changes, err := e.repo.DiffTrees(ctx, main, b)
	if err != nil {
		return false, err
	}
	total, err := e.countFiles(ctx, main)
	if err != nil {
		return false, err
	}
	return !e.massDelete(len(deletions(changes)), total), nil
}

// afterApply asks Home Assistant to check and reload when main's changes
// touch its configuration. Nothing is ever rolled back.
func (e *Engine) afterApply(ctx context.Context, st *State, rep *Report) {
	var touched []string
	for _, c := range rep.Incoming {
		if rel, ok := e.haRel(c.Path); ok {
			touched = append(touched, rel)
		}
	}
	if len(touched) == 0 {
		return
	}
	e.checkAndReload(ctx, rep)
	// Changes the reload does not pick up. The flag reflects the last
	// change applied to Home Assistant's configuration.
	st.RestartRecommended = slices.ContainsFunc(touched, func(rel string) bool {
		return rel == "configuration.yaml" || strings.HasPrefix(rel, "packages/") || strings.HasPrefix(rel, "custom_components/")
	})
}

// checkAndReload runs Home Assistant's config check (if enabled) and the
// reload (if enabled and the check reported no errors).
func (e *Engine) checkAndReload(ctx context.Context, rep *Report) {
	if e.cfg.CheckConfig {
		rep.checkRan = true
		res, err := e.checker.CheckConfig(ctx)
		switch {
		case err != nil:
			rep.CheckErrors = "could not run Home Assistant's config check: " + err.Error()
		case !res.Valid:
			rep.Checked, rep.CheckErrors, rep.CheckWarnings = true, res.Errors, res.Warnings
		default:
			rep.Checked, rep.CheckWarnings = true, res.Warnings
		}
	}
	if e.cfg.Reload && rep.CheckErrors == "" {
		if err := e.reload.ReloadAll(ctx); err != nil {
			rep.Error = "the changes were applied, but the reload failed: " + err.Error()
		} else {
			rep.Reloaded = true
		}
	}
}

// haRel returns a repo path relative to Home Assistant's config folder, if
// it is inside it.
func (e *Engine) haRel(repoPath string) (string, bool) {
	abs := path.Join(e.root, repoPath)
	return strings.CutPrefix(abs, config.HomeAssistantDir+"/")
}

// massDelete applies the mass-deletion thresholds.
func (e *Engine) massDelete(n, total int) bool {
	if n == 0 {
		return false
	}
	return n > e.cfg.DeleteGuardFiles || (n >= 5 && n*100 > e.cfg.DeleteGuardPercent*total)
}

func deletions(changes []gitx.Change) []string {
	var out []string
	for _, c := range changes {
		if c.Status == "D" {
			out = append(out, c.Path)
		}
	}
	return out
}

func (e *Engine) countFiles(ctx context.Context, treeish string) (int, error) {
	entries, err := e.repo.LsTree(ctx, treeish)
	return len(entries), err
}

// tooLarge lists the added or modified files above max_file_size_mb.
func (e *Engine) tooLarge(ctx context.Context, changes []gitx.Change) ([]string, error) {
	var oids []string
	for _, c := range changes {
		if c.OID != "" {
			oids = append(oids, c.OID)
		}
	}
	sizes, err := e.repo.ObjectSizes(ctx, oids)
	if err != nil {
		return nil, err
	}
	limit := int64(e.cfg.MaxFileSizeMB) << 20
	var out []string
	for _, c := range changes {
		if c.OID != "" && sizes[c.OID] > limit {
			out = append(out, fmt.Sprintf("%s (%.1f MB)", c.Path, float64(sizes[c.OID])/(1<<20)))
		}
	}
	return out, nil
}

// collisions lists top-level paths that main adds and that would land on a
// folder or file of the app's own container (only with root /).
func (e *Engine) collisions(ctx context.Context, changes []gitx.Change, b string) ([]string, error) {
	if e.root != "/" {
		return nil, nil
	}
	entries, err := e.repo.LsTree(ctx, b)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for _, en := range entries {
		owned[topLevel(en.Path)] = true
	}
	var out []string
	for _, c := range changes {
		top := topLevel(c.Path)
		if c.Status == "D" || slices.Contains(Mounts, top) || owned[top] || slices.Contains(out, top) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(e.fsRoot, top)); err == nil {
			out = append(out, top)
		}
	}
	return out, nil
}

func topLevel(p string) string {
	top, _, _ := strings.Cut(p, "/")
	return top
}

// writeExclude keeps git looking only at the mounts when the root is /.
func (e *Engine) writeExclude() error {
	p := e.path("repo.git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	content := "# Managed by the app.\n"
	if e.root == "/" {
		content += "/*\n"
		for _, m := range Mounts {
			content += "!/" + m + "/\n"
		}
		// The repo's own config files may be created on the box (the base
		// .gitignore on the first sync); other top-level files only come
		// from main.
		for _, f := range []string{".gitignore", ".gitattributes", ".gitleaks.toml", ".gitleaksignore"} {
			content += "!/" + f + "\n"
		}
	}
	return os.WriteFile(p, []byte(content), 0o600)
}

// restoreTopLevel rewrites the repo's top-level files (with root /) that the
// container lost when it was recreated.
func (e *Engine) restoreTopLevel(ctx context.Context, wt *gitx.WorkTree) error {
	if e.root != "/" {
		return nil
	}
	missing, err := wt.Missing(ctx)
	if err != nil {
		return err
	}
	var restore []string
	for _, p := range missing {
		if !slices.Contains(Mounts, topLevel(p)) {
			restore = append(restore, p)
		}
	}
	return wt.Restore(ctx, restore)
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
	return e.track(ctx, e.trackBox, b, originBox)
}

// track moves a tracking ref after a successful push.
func (e *Engine) track(ctx context.Context, ref, to, from string) error {
	if to == from {
		return nil
	}
	return e.repo.UpdateRef(ctx, ref, to, from)
}

// maxMessagePaths is how many paths are listed in a snapshot's message.
const maxMessagePaths = 100

// snapshotMessage is the snapshot commit's message: a summary line and the
// paths with their status letter, like `git status --short`.
func snapshotMessage(changes []gitx.Change) string {
	var b strings.Builder
	if len(changes) == 1 {
		fmt.Fprintf(&b, "box: %s\n\n", changes[0].Path)
	} else {
		fmt.Fprintf(&b, "box: %d changes\n\n", len(changes))
	}
	for i, c := range changes {
		if i == maxMessagePaths {
			fmt.Fprintf(&b, "… and %d more\n", len(changes)-maxMessagePaths)
			break
		}
		fmt.Fprintf(&b, "%s %s\n", c.Status, c.Path)
	}
	return b.String()
}

func copyIndex(index string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(index), "index-dry-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_ = tmp.Close()
	b, err := os.ReadFile(index)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		_ = os.Remove(name) // read-tree creates it
		return name, nil
	case err != nil:
		return "", err
	}
	return name, os.WriteFile(name, b, 0o600)
}

func hashKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}
