// Package snapshot implements step 2 of the cycle (design.md §6): scan the
// live disk and, if it differs from the box branch, commit the difference on
// top of box. It only changes managed paths; everything else in the tree is
// copied from the parent commit.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/scan"
)

// minDeletesForPercent: the mass-delete guard's percentage threshold only
// applies from this number of deletions on, so deleting 1 of 4 files in a
// small config does not stop the sync.
const minDeletesForPercent = 5

// Guard errors. Compare them with errors.Is.
var (
	ErrSuspicious = errors.New("suspicious snapshot")
	ErrMassDelete = errors.New("mass deletion")
)

// ChangeKind is the kind of change of a path.
type ChangeKind string

const (
	Added    ChangeKind = "A"
	Modified ChangeKind = "M"
	Deleted  ChangeKind = "D"
)

// Change is a change on the box relative to the box branch.
type Change struct {
	Path   string     `json:"path"`
	Kind   ChangeKind `json:"kind"`
	Mode   string     `json:"mode"`
	OldOID string     `json:"old_oid,omitempty"`
	NewOID string     `json:"new_oid,omitempty"`
}

// Options configures a snapshot.
type Options struct {
	Matcher *paths.Matcher
	// Ref is the full box branch, e.g. "refs/heads/box".
	Ref                string
	MaxFileSize        int64
	DeleteGuardFiles   int
	DeleteGuardPercent int
	// AllowMassDelete skips the mass-delete guard (explicit confirmation).
	AllowMassDelete bool
	// Author is "Name <email>".
	Author string
	Now    func() time.Time
	Cache  scan.Cache
	// BeforeCommit runs after the changes are computed and before any object
	// is written to the repo. It receives the content of what was added or
	// modified. If it returns an error nothing is written or committed (the
	// secret scanner hooks in here in M5).
	BeforeCommit func(ctx context.Context, changes []Change, contents map[string][]byte) error
	// DryRun builds the snapshot commit (so a plan can merge the uncommitted
	// box changes exactly) but does not move the branch. Objects are written
	// only after BeforeCommit passes, so a blocked secret never reaches the
	// object store; the unreferenced commit is garbage-collected by git.
	DryRun bool
}

// Result is the result of a snapshot.
type Result struct {
	// Parent is the tip of box before the snapshot ("" if it did not exist).
	Parent string
	// Commit is the new commit, or "" if there were no changes.
	Commit  string
	Changes []Change
	Skipped []scan.Skipped
	// Cache is the stat cache for the next snapshot. It is valid even if
	// there is an error, unless the error comes from the scan itself.
	Cache scan.Cache
}

// Take takes the snapshot.
func Take(ctx context.Context, repo *gitx.Repo, opts Options) (*Result, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	parent, err := repo.ResolveRef(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	base, err := repo.LsTree(ctx, parent)
	if err != nil {
		return nil, err
	}
	sr, err := scan.Scan(opts.Matcher, scan.Options{MaxFileSize: opts.MaxFileSize, Cache: opts.Cache, Now: now})
	if err != nil {
		return nil, err
	}
	res := &Result{Parent: parent, Skipped: sr.Skipped, Cache: sr.Cache}

	changes, baseManaged, err := diff(opts.Matcher, base, sr)
	if err != nil {
		return res, err
	}
	if err := massDeleteGuard(changes, baseManaged, opts); err != nil {
		return res, err
	}
	res.Changes = changes
	if len(changes) == 0 {
		return res, nil
	}

	contents := map[string][]byte{}
	for _, c := range changes {
		if c.Kind == Deleted {
			continue
		}
		f := sr.Files[c.Path]
		content := f.Content
		if content == nil {
			if content, err = scan.ReadVerified(f.BoxPath, f.OID, opts.MaxFileSize); err != nil {
				return res, fmt.Errorf("re-reading %s: %w", c.Path, err)
			}
		}
		contents[c.Path] = content
	}

	if opts.BeforeCommit != nil {
		if err := opts.BeforeCommit(ctx, changes, contents); err != nil {
			return res, err
		}
	}
	updates := make([]gitx.IndexUpdate, 0, len(changes))
	for _, c := range changes {
		if c.Kind == Deleted {
			updates = append(updates, gitx.IndexUpdate{Path: c.Path})
			continue
		}
		oid, err := repo.WriteBlob(ctx, contents[c.Path])
		if err != nil {
			return res, err
		}
		if oid != c.NewOID {
			return res, fmt.Errorf("%s: git computed blob %s but the scan computed %s", c.Path, oid, c.NewOID)
		}
		updates = append(updates, gitx.IndexUpdate{Path: c.Path, Mode: c.Mode, OID: oid})
	}

	baseTree := ""
	if parent != "" {
		if baseTree, err = repo.TreeOf(ctx, parent); err != nil {
			return res, err
		}
	}
	tree, err := repo.BuildTree(ctx, baseTree, updates)
	if err != nil {
		return res, err
	}

	when := now()
	author, err := gitx.ParseIdent(opts.Author, when)
	if err != nil {
		return res, err
	}
	// The author comes from the config (commit_author); the committer is the engine.
	committer, _ := gitx.ParseIdent(gitx.EngineIdent, when)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commit, err := repo.CommitTree(ctx, tree, parents, Message(changes), author, committer)
	if err != nil {
		return res, err
	}
	if !opts.DryRun {
		if err := repo.UpdateRef(ctx, opts.Ref, commit, parent); err != nil {
			return res, err
		}
	}
	res.Commit = commit
	return res, nil
}

// diff compares the box tree with the scan. It returns the changes and how
// many managed paths the base had.
func diff(m *paths.Matcher, base []gitx.TreeEntry, sr *scan.Result) ([]Change, int, error) {
	roots := map[int]scan.Root{}
	for _, r := range sr.Roots {
		roots[r.Mapping] = r
	}

	var changes []Change
	baseManaged := 0
	baseManagedByMapping := map[int]int{}
	inBase := map[string]bool{}

	for _, e := range base {
		inBase[e.Path] = true
		cls := m.ClassifyRepoPath(e.Path)
		if !cls.Managed {
			continue // owned by the repo: kept as is
		}
		baseManaged++
		baseManagedByMapping[cls.Mapping]++

		if f, ok := sr.Files[e.Path]; ok {
			if f.OID != e.OID || f.Mode != e.Mode {
				changes = append(changes, Change{Path: e.Path, Kind: Modified, Mode: f.Mode, OldOID: e.OID, NewOID: f.OID})
			}
			continue
		}
		if skipped(sr.Skipped, e.Path) {
			continue // skipping is never deleting
		}
		changes = append(changes, Change{Path: e.Path, Kind: Deleted, Mode: e.Mode, OldOID: e.OID})
	}
	for p, f := range sr.Files {
		if !inBase[p] {
			changes = append(changes, Change{Path: p, Kind: Added, Mode: f.Mode, NewOID: f.OID})
		}
	}

	// Root guards: a broken mount would look like "everything was deleted".
	for i, mp := range m.Mappings() {
		r := roots[i]
		if !r.Active {
			continue
		}
		if !r.Exists {
			return nil, 0, fmt.Errorf("%w: root %s of mapping %q does not exist or is not a directory", ErrSuspicious, mp.Box, mp.Repo)
		}
		if r.Managed == 0 && baseManagedByMapping[i] > 0 {
			return nil, 0, fmt.Errorf("%w: %s has no managed files but box had %d", ErrSuspicious, mp.Box, baseManagedByMapping[i])
		}
	}

	sort.Slice(changes, func(a, b int) bool { return changes[a].Path < changes[b].Path })
	return changes, baseManaged, nil
}

func skipped(list []scan.Skipped, p string) bool {
	for _, s := range list {
		if s.Covers(p) {
			return true
		}
	}
	return false
}

func massDeleteGuard(changes []Change, baseManaged int, opts Options) error {
	if opts.AllowMassDelete {
		return nil
	}
	deleted := 0
	for _, c := range changes {
		if c.Kind == Deleted {
			deleted++
		}
	}
	overFiles := opts.DeleteGuardFiles > 0 && deleted > opts.DeleteGuardFiles
	overPercent := opts.DeleteGuardPercent > 0 && deleted >= minDeletesForPercent && deleted*100 > opts.DeleteGuardPercent*baseManaged
	if overFiles || overPercent {
		return fmt.Errorf("%w: the snapshot would delete %d of %d managed files", ErrMassDelete, deleted, baseManaged)
	}
	return nil
}

// maxMessagePaths is how many paths are listed in the commit message.
const maxMessagePaths = 100

// Message builds the snapshot commit message.
func Message(changes []Change) string {
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
		fmt.Fprintf(&b, "%s %s\n", c.Kind, c.Path)
	}
	return b.String()
}
