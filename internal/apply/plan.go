// Package apply implementa el paso 4 del ciclo (design.md §7): llevar el
// disco vivo del árbol de B (box) al de R (resultado del merge), con
// pre-check de sintaxis, compare-and-swap por path, write-ahead journal,
// escritura atómica, check de config y rollback.
package apply

import (
	"context"
	"sort"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
)

// Op es una operación sobre un path del box. Un OID vacío significa "no
// existe": OldOID vacío es un archivo nuevo y NewOID vacío es un borrado.
type Op struct {
	RepoPath string `json:"repo_path"`
	BoxPath  string `json:"box_path"`
	Mapping  int    `json:"mapping"`
	OldOID   string `json:"old_oid,omitempty"`
	OldMode  string `json:"old_mode,omitempty"`
	NewOID   string `json:"new_oid,omitempty"`
	NewMode  string `json:"new_mode,omitempty"`
	// OldPerm son los permisos que tenía el archivo en disco antes del apply,
	// para restaurarlos en un rollback (se completa durante el CAS).
	OldPerm uint32 `json:"old_perm,omitempty"`
}

// IsDelete dice si la operación borra el path.
func (o Op) IsDelete() bool { return o.NewOID == "" }

// Motivos por los que un cambio de R no se aplica al box. Además de estos,
// Ignored.Reason puede ser cualquier paths.Reason.
const ReasonPushOnly = "push_only"

// Ignored es un path que cambió entre B y R pero no se aplica al box.
type Ignored struct {
	RepoPath string `json:"repo_path"`
	Reason   string `json:"reason"`
}

// Plan es lo que haría un apply, calculado solo desde git.
type Plan struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Ops     []Op      `json:"ops"`
	Ignored []Ignored `json:"ignored,omitempty"`
}

// MakePlan compara los árboles de from (B) y to (R) y arma las operaciones
// sobre los paths gestionados en las dos direcciones. Primero van los
// borrados (de lo más profundo a lo más alto) y después las escrituras, para
// que un path que pasa de archivo a directorio, o al revés, no choque.
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
