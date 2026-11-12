// Package apply implements step 4 of the cycle (design.md §7): take the
// live disk from the tree of B (box) to that of R (the merge result), with
// a syntax pre-check, per-path compare-and-swap, a write-ahead journal,
// atomic writes, a config check and rollback.
package apply

import (
	"context"
	"sort"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
)

// Op is an operation on a box path. An empty OID means "does not exist":
// an empty OldOID is a new file and an empty NewOID is a deletion.
type Op struct {
	RepoPath string `json:"repo_path"`
	BoxPath  string `json:"box_path"`
	Mapping  int    `json:"mapping"`
	OldOID   string `json:"old_oid,omitempty"`
	OldMode  string `json:"old_mode,omitempty"`
	NewOID   string `json:"new_oid,omitempty"`
	NewMode  string `json:"new_mode,omitempty"`
	// OldPerm is the file's on-disk permissions before the apply, to restore
	// them on rollback (filled in during the CAS).
	OldPerm uint32 `json:"old_perm,omitempty"`
}

// IsDelete reports whether the operation deletes the path.
func (o Op) IsDelete() bool { return o.NewOID == "" }

// Reasons why a change in R is not applied to the box. Besides these,
// Ignored.Reason can be any paths.Reason.
const ReasonPushOnly = "push_only"

// Ignored is a path that changed between B and R but is not applied to the box.
type Ignored struct {
	RepoPath string `json:"repo_path"`
	Reason   string `json:"reason"`
}

// Plan is what an apply would do, computed from git alone.
type Plan struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Ops     []Op      `json:"ops"`
	Ignored []Ignored `json:"ignored,omitempty"`
}

// MakePlan compares the trees of from (B) and to (R) and builds the
// operations on paths managed in both directions. Deletions come first
// (deepest to shallowest) and then writes, so that a path that turns from
// file into directory, or the other way, does not collide.
func MakePlan(ctx context.Context, repo *gitx.Repo, m *paths.Matcher, from, to string) (*Plan, error) {
	fromEntries, err := repo.LsTree(ctx, from)
	if err != nil {
		return nil, err
	}
	toEntries, err := repo.LsTree(ctx, to)
	if err != nil {
		return nil, err
	}
	old := map[string]gitx.TreeEntry{}
	for _, e := range fromEntries {
		old[e.Path] = e
	}
	cur := map[string]gitx.TreeEntry{}
	for _, e := range toEntries {
		cur[e.Path] = e
	}

	plan := &Plan{From: from, To: to}
	consider := func(p string) {
		o, inOld := old[p]
		n, inNew := cur[p]
		if inOld && inNew && o.OID == n.OID && o.Mode == n.Mode {
			return
		}
		cls := m.ClassifyRepoPath(p)
		switch {
		case !cls.Managed:
			plan.Ignored = append(plan.Ignored, Ignored{RepoPath: p, Reason: string(cls.Reason)})
			return
		case cls.Direction == paths.PushOnly:
			plan.Ignored = append(plan.Ignored, Ignored{RepoPath: p, Reason: ReasonPushOnly})
			return
		}
		op := Op{RepoPath: p, BoxPath: cls.BoxPath, Mapping: cls.Mapping}
		if inOld {
			op.OldOID, op.OldMode = o.OID, o.Mode
		}
		if inNew {
			op.NewOID, op.NewMode = n.OID, n.Mode
		}
		plan.Ops = append(plan.Ops, op)
	}
	for p := range old {
		consider(p)
	}
	for p := range cur {
		if _, ok := old[p]; !ok {
			consider(p)
		}
	}

	sort.Slice(plan.Ops, func(a, b int) bool {
		oa, ob := plan.Ops[a], plan.Ops[b]
		if oa.IsDelete() != ob.IsDelete() {
			return oa.IsDelete()
		}
		if oa.IsDelete() {
			return oa.RepoPath > ob.RepoPath
		}
		return oa.RepoPath < ob.RepoPath
	})
	sort.Slice(plan.Ignored, func(a, b int) bool { return plan.Ignored[a].RepoPath < plan.Ignored[b].RepoPath })
	return plan, nil
}
