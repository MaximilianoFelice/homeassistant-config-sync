package apply

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/scan"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/yamlcheck"
)

// Apply errors. Compare them with errors.Is.
var (
	// ErrPrecheck: a YAML/JSON file in R does not parse. The disk was not touched.
	ErrPrecheck = errors.New("the syntax pre-check failed")
	// ErrDiskChanged: the disk no longer matches B (someone edited since the
	// snapshot). The disk was not touched; snapshot again.
	ErrDiskChanged = errors.New("the disk changed since the snapshot")
	// ErrUnsafePath: a path to write goes through a symlink. The disk was not touched.
	ErrUnsafePath = errors.New("unsafe path in the box")
	// ErrRejected: Home Assistant said the config is invalid. Rolled back
	// to B.
	ErrRejected = errors.New("home assistant rejected the configuration")
	// ErrCheckUnavailable: the check could not run. Rolled back to B.
	ErrCheckUnavailable = errors.New("could not validate the configuration")
)

// errCrash simulates, in tests, the process dying in the middle of the apply.
var errCrash = errors.New("simulated crash")

// Options configures an apply.
type Options struct {
	Matcher     *paths.Matcher
	Checker     ha.Checker
	JournalPath string
	// Warnings is the policy for check_config warnings
	// (config.WarningsNew by default if empty).
	Warnings string

	// afterOp is a test hook: if it returns true after operation i, Apply
	// returns as if the process had died (no rollback).
	afterOp func(i int) bool
}

// Result is the result of an apply.
type Result struct {
	Plan *Plan
	// Applied means the disk is at R (with the check passed if one was
	// needed). The journal stays in phase "checked" until the engine moves
	// box and calls Finish.
	Applied bool
	// Checked means the config check ran.
	Checked bool
	Check   ha.CheckResult
	// RolledBack means something was written and then reverted to B.
	RolledBack bool
}

// Apply takes the disk from B to R according to the plan.
func Apply(ctx context.Context, repo *gitx.Repo, plan *Plan, opts Options) (*Result, error) {
	res := &Result{Plan: plan}
	if len(plan.Ops) == 0 {
		return res, nil
	}
	mappings := opts.Matcher.Mappings()

	// 1. Syntax pre-check, before touching anything.
	contents := map[string][]byte{}
	var preErrs []error
	for _, op := range plan.Ops {
		if op.IsDelete() {
			continue
		}
		b, err := repo.ReadBlob(ctx, op.NewOID)
		if err != nil {
			return res, err
		}
		contents[op.RepoPath] = b
		if err := yamlcheck.Check(op.RepoPath, b); err != nil {
			preErrs = append(preErrs, fmt.Errorf("%s: %w", op.RepoPath, err))
		}
	}
	if len(preErrs) > 0 {
		return res, fmt.Errorf("%w:\n%w", ErrPrecheck, errors.Join(preErrs...))
	}

	// 2. Safe paths and compare-and-swap against B.
	ops := make([]Op, len(plan.Ops))
	copy(ops, plan.Ops)
	var changed []string
	for i := range ops {
		root := mappings[ops[i].Mapping].Box
		if err := safeParents(root, ops[i].BoxPath); err != nil {
			return res, err
		}
		ok, perm, err := matchesOld(ops[i])
		if err != nil {
			return res, err
		}
		if !ok {
			changed = append(changed, ops[i].RepoPath)
		}
		ops[i].OldPerm = perm
	}
	if len(changed) > 0 {
		return res, fmt.Errorf("%w: %s", ErrDiskChanged, strings.Join(changed, ", "))
	}

	needsCheck := false
	for _, op := range ops {
		needsCheck = needsCheck || mappings[op.Mapping].Check
	}
	policy := opts.Warnings
	if policy == "" {
		policy = config.WarningsNew
	}
	// With the "new" policy we need to know which warnings exist before
	// writing. If the check fails here, nothing was touched.
	var baseline []string
	if needsCheck && policy == config.WarningsNew {
		cr, err := opts.Checker.CheckConfig(ctx)
		if err != nil {
			return res, fmt.Errorf("%w (before writing): %w", ErrCheckUnavailable, err)
		}
		baseline = cr.Warnings
	}

	// 3. Write-ahead journal.
	j := &Journal{From: plan.From, To: plan.To, Phase: PhaseWriting, Ops: ops}
	if err := writeJournal(opts.JournalPath, j); err != nil {
		return res, err
	}

	// 4. Write.
	for i, op := range ops {
		if err := doOp(op, contents[op.RepoPath], mappings[op.Mapping].Box); err != nil {
			return res, fail(ctx, repo, res, ops[:i+1], opts, fmt.Errorf("%s: %w", op.RepoPath, err))
		}
		if opts.afterOp != nil && opts.afterOp(i) {
			return res, errCrash
		}
	}

	// 5. Config check, if a mapping that requires it was touched.
	if needsCheck {
		res.Checked = true
		cr, err := opts.Checker.CheckConfig(ctx)
		if err != nil {
			return res, fail(ctx, repo, res, ops, opts, fmt.Errorf("%w: %w", ErrCheckUnavailable, err))
		}
		res.Check = cr
		if !cr.Valid {
			return res, fail(ctx, repo, res, ops, opts, fmt.Errorf("%w:\n%s", ErrRejected, cr.Errors))
		}
		var bad []string
		switch policy {
		case config.WarningsAny:
			bad = cr.Warnings
		case config.WarningsNew:
			bad = ha.NewWarnings(baseline, cr.Warnings)
		}
		if len(bad) > 0 {
			return res, fail(ctx, repo, res, ops, opts, fmt.Errorf("%w: the change adds warnings (an integration would be disabled):\n%s", ErrRejected, strings.Join(bad, "\n")))
		}
	}
	if opts.afterOp != nil && opts.afterOp(len(ops)) {
		return res, errCrash
	}

	j.Phase = PhaseChecked
	if err := writeJournal(opts.JournalPath, j); err != nil {
		return res, fail(ctx, repo, res, ops, opts, err)
	}
	res.Applied = true
	return res, nil
}

// fail rolls back the operations done and deletes the journal. If the
// rollback fails, the journal is kept so Recover can retry it.
func fail(ctx context.Context, repo *gitx.Repo, res *Result, done []Op, opts Options, cause error) error {
	if err := rollback(ctx, repo, done, opts.Matcher.Mappings()); err != nil {
		return fmt.Errorf("%w; the rollback also failed (the journal is kept for recovery): %w", cause, err)
	}
	res.RolledBack = true
	if err := removeJournal(opts.JournalPath); err != nil {
		return fmt.Errorf("%w; also: %w", cause, err)
	}
	return cause
}

// rollback undoes operations in reverse order, leaving each path as in B.
func rollback(ctx context.Context, repo *gitx.Repo, ops []Op, mappings []config.Mapping) error {
	var errs []error
	for i := len(ops) - 1; i >= 0; i-- {
		if err := restoreOld(ctx, repo, ops[i], mappings[ops[i].Mapping].Box); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ops[i].RepoPath, err))
		}
	}
	return errors.Join(errs...)
}

func restoreOld(ctx context.Context, repo *gitx.Repo, op Op, root string) error {
	if op.OldOID == "" {
		if err := os.Remove(op.BoxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		pruneEmptyDirs(filepath.Dir(op.BoxPath), root)
		return nil
	}
	content, err := repo.ReadBlob(ctx, op.OldOID)
	if err != nil {
		return err
	}
	// A non-empty OldOID makes permFor honor OldPerm.
	old := Op{BoxPath: op.BoxPath, NewOID: op.OldOID, NewMode: op.OldMode, OldOID: op.OldOID, OldPerm: op.OldPerm}
	return doOp(old, content, root)
}

// doOp performs one operation on the disk.
func doOp(op Op, content []byte, root string) error {
	if op.IsDelete() {
		if err := os.Remove(op.BoxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		pruneEmptyDirs(filepath.Dir(op.BoxPath), root)
		return nil
	}
	// If there is a directory in its place (directory to file change), the
	// plan's deletions already emptied it; if it is not empty, os.Remove
	// fails and the apply rolls back.
	if fi, err := os.Lstat(op.BoxPath); err == nil && fi.IsDir() {
		if err := os.Remove(op.BoxPath); err != nil {
			return fmt.Errorf("found a directory with unmanaged content: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(op.BoxPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(op.BoxPath, content, permFor(op))
}

// permFor keeps the file's existing permissions and adjusts only the
// executable bit according to the git mode.
func permFor(op Op) fs.FileMode {
	perm := fs.FileMode(0o644)
	if op.OldOID != "" && op.OldPerm != 0 {
		perm = fs.FileMode(op.OldPerm).Perm()
	}
	if op.NewMode == scan.ModeExec {
		return perm | 0o111
	}
	return perm &^ 0o111
}

func writeAtomic(path string, content []byte, perm fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".hcs-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pruneEmptyDirs deletes empty directories from dir upwards, without touching
// root.
func pruneEmptyDirs(dir, root string) {
	root = filepath.Clean(root)
	for d := filepath.Clean(dir); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			return
		}
	}
}

// safeParents checks that no existing component between root and p's
// parent directory is a symlink: writing through one could escape the
// mapping.
func safeParents(root, p string) error {
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: root %s is not a directory", ErrUnsafePath, root)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: %s is outside %s", ErrUnsafePath, p, root)
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, c := range parts {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if notExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrUnsafePath, cur)
		}
	}
	return nil
}

// matchesOld reports whether the disk matches B for this operation and
// returns the file's current permissions.
func matchesOld(op Op) (bool, uint32, error) {
	st, err := diskState(op.BoxPath)
	if err != nil {
		return false, 0, err
	}
	if op.OldOID == "" {
		// New file: it must not exist (a directory may exist, because the
		// same plan's deletions can empty it).
		return !st.exists || st.dir, 0, nil
	}
	return st.exists && !st.dir && st.oid == op.OldOID && st.mode == op.OldMode, st.perm, nil
}

// atState reports whether the disk is in the given (oid, mode) state.
func atState(boxPath, oid, mode string) (bool, error) {
	st, err := diskState(boxPath)
	if err != nil {
		return false, err
	}
	if oid == "" {
		return !st.exists, nil
	}
	return st.exists && !st.dir && st.oid == oid && st.mode == mode, nil
}

type disk struct {
	exists bool
	dir    bool
	oid    string
	mode   string
	perm   uint32
}

// notExist includes ENOTDIR: a path whose parent is a file (file to
// directory change) does not exist either.
func notExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func diskState(p string) (disk, error) {
	fi, err := os.Lstat(p)
	if notExist(err) {
		return disk{}, nil
	}
	if err != nil {
		return disk{}, err
	}
	switch {
	case fi.IsDir():
		return disk{exists: true, dir: true}, nil
	case !fi.Mode().IsRegular():
		return disk{}, fmt.Errorf("%w: %s is not a regular file", ErrUnsafePath, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return disk{}, err
	}
	mode := scan.ModeFile
	if fi.Mode().Perm()&0o111 != 0 {
		mode = scan.ModeExec
	}
	return disk{exists: true, oid: scan.BlobOID(b), mode: mode, perm: uint32(fi.Mode().Perm())}, nil
}
