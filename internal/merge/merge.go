// Package merge implements step 3 of the cycle (design.md §6): combine the
// box branch with main and decide which commit R to apply to the box and
// publish. It does not touch the disk or move refs: it only creates objects
// in the repo.
package merge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
)

// Kind is the merge case that occurred.
type Kind string

const (
	// UpToDate: box and main are the same commit. R = box.
	UpToDate Kind = "up_to_date"
	// FastForwardMain: main does not exist or is an ancestor of box (only the
	// box changed). R = box; main advances by fast-forward.
	FastForwardMain Kind = "ff_main"
	// FastForwardBox: box is an ancestor of main (only main changed). R = main,
	// or a commit on top of main if push_only paths had to be kept.
	FastForwardBox Kind = "ff_box"
	// Merged: both branches changed and the merge was clean. R is a
	// merge commit with parents [main, box].
	Merged Kind = "merged"
	// Conflict: the merge has conflicts that do not resolve on their own. No R.
	Conflict Kind = "conflict"
)

// autoResolvable are the conflict types that can resolve on their own
// when all their paths belong to the box (push_only). Structural conflicts
// (file/directory, renames) always stop, because git leaves auxiliary
// paths in the tree.
var autoResolvable = []string{
	"CONFLICT (contents)",
	"CONFLICT (add/add)",
	"CONFLICT (modify/delete)",
}

// ConflictInfo describes a conflict.
type ConflictInfo struct {
	Paths   []string
	Type    string
	Message string
}

// Options configures a merge.
type Options struct {
	// BoxWins reports whether a path is box-authoritative (push_only): R
	// always keeps the box version, whatever the merge case.
	BoxWins func(repoPath string) bool
	Now     func() time.Time
}

// Result is the result of a merge.
type Result struct {
	Kind Kind
	Box  string
	Main string
	// R is the commit to apply and publish; empty on conflict.
	R string
	// ConflictPaths are the conflicted paths, sorted and deduplicated.
	ConflictPaths []string
	Conflicts     []ConflictInfo
	// Overridden are the push_only paths where main's version was discarded
	// to keep the box version.
	Overridden []string
}

// Merge combines box with main. box must exist; main may be ""
// (empty remote).
func Merge(ctx context.Context, repo *gitx.Repo, box, main string, opts Options) (*Result, error) {
	if box == "" {
		return nil, errors.New("merge: the box branch does not exist")
	}
	res := &Result{Box: box, Main: main}

	switch main {
	case "":
		res.Kind, res.R = FastForwardMain, box
		return res, nil
	case box:
		res.Kind, res.R = UpToDate, box
		return res, nil
	}
	if ok, err := repo.IsAncestor(ctx, main, box); err != nil {
		return nil, err
	} else if ok {
		res.Kind, res.R = FastForwardMain, box
		return res, nil
	}

	var tree string
	boxIsAncestor, err := repo.IsAncestor(ctx, box, main)
	if err != nil {
		return nil, err
	}
	if boxIsAncestor {
		res.Kind = FastForwardBox
		if tree, err = repo.TreeOf(ctx, main); err != nil {
			return nil, err
		}
	} else {
		res.Kind = Merged
		// ours = main, theirs = box: the merge's first parent is main.
		mt, err := repo.MergeTree(ctx, main, box)
		if err != nil {
			return nil, err
		}
		if !mt.Clean {
			collectConflicts(res, mt)
			if !resolvable(res, opts.BoxWins) {
				res.Kind = Conflict
				return res, nil
			}
			// All conflicts are on push_only paths: the override below
			// resolves them with the box version.
			res.Conflicts, res.ConflictPaths = nil, nil
		}
		tree = mt.Tree
	}

	tree, res.Overridden, err = overrideBoxWins(ctx, repo, box, tree, opts.BoxWins)
	if err != nil {
		return nil, err
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	id, _ := gitx.ParseIdent(gitx.EngineIdent, now())

	switch res.Kind {
	case FastForwardBox:
		if len(res.Overridden) == 0 {
			res.R = main
			return res, nil
		}
		res.R, err = repo.CommitTree(ctx, tree, []string{main}, message("box: keep push_only paths over main", res.Overridden), id, id)
	case Merged:
		res.R, err = repo.CommitTree(ctx, tree, []string{main, box}, message("Merge box into main", res.Overridden), id, id)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func collectConflicts(res *Result, mt *gitx.MergeTreeResult) {
	set := map[string]bool{}
	for _, p := range mt.Conflicted {
		set[p] = true
	}
	for _, m := range mt.Messages {
		if !strings.HasPrefix(m.Type, "CONFLICT") {
			continue
		}
		res.Conflicts = append(res.Conflicts, ConflictInfo{Paths: m.Paths, Type: m.Type, Message: m.Message})
		for _, p := range m.Paths {
			set[p] = true
		}
	}
	for p := range set {
		res.ConflictPaths = append(res.ConflictPaths, p)
	}
	sort.Strings(res.ConflictPaths)
}

func resolvable(res *Result, boxWins func(string) bool) bool {
	if boxWins == nil || len(res.ConflictPaths) == 0 {
		return false
	}
	for _, c := range res.Conflicts {
		if !slices.Contains(autoResolvable, c.Type) {
			return false
		}
	}
	for _, p := range res.ConflictPaths {
		if !boxWins(p) {
			return false
		}
	}
	return true
}

// overrideBoxWins puts the box version of every push_only path into tree.
func overrideBoxWins(ctx context.Context, repo *gitx.Repo, box, tree string, boxWins func(string) bool) (string, []string, error) {
	if boxWins == nil {
		return tree, nil, nil
	}
	boxEntries, err := repo.LsTree(ctx, box)
	if err != nil {
		return "", nil, err
	}
	treeEntries, err := repo.LsTree(ctx, tree)
	if err != nil {
		return "", nil, err
	}
	inBox := map[string]gitx.TreeEntry{}
	for _, e := range boxEntries {
		if boxWins(e.Path) {
			inBox[e.Path] = e
		}
	}
	var updates []gitx.IndexUpdate
	var overridden []string
	for _, e := range treeEntries {
		if !boxWins(e.Path) {
			continue
		}
		b, ok := inBox[e.Path]
		switch {
		case !ok:
			updates = append(updates, gitx.IndexUpdate{Path: e.Path})
			overridden = append(overridden, e.Path)
		case b.OID != e.OID || b.Mode != e.Mode:
			updates = append(updates, gitx.IndexUpdate{Path: e.Path, Mode: b.Mode, OID: b.OID})
			overridden = append(overridden, e.Path)
		}
		delete(inBox, e.Path)
	}
	for p, b := range inBox { // in box, deleted by main
		updates = append(updates, gitx.IndexUpdate{Path: p, Mode: b.Mode, OID: b.OID})
		overridden = append(overridden, p)
	}
	if len(updates) == 0 {
		return tree, nil, nil
	}
	sort.Strings(overridden)
	newTree, err := repo.BuildTree(ctx, tree, updates)
	if err != nil {
		return "", nil, fmt.Errorf("keeping push_only paths: %w", err)
	}
	return newTree, overridden, nil
}

func message(title string, overridden []string) string {
	var b strings.Builder
	b.WriteString(title + "\n")
	if len(overridden) > 0 {
		b.WriteString("\npush_only paths that keep the box version:\n")
		for _, p := range overridden {
			b.WriteString("  " + p + "\n")
		}
	}
	return b.String()
}
