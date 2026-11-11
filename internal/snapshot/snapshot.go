// Package snapshot implementa el paso 2 del ciclo (design.md §6): escanear
// el disco vivo y, si difiere de la rama box, commitear la diferencia sobre
// box. Solo cambia paths gestionados; todo lo demás del árbol se copia del
// commit padre.
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

// Committer es la identidad con la que el motor firma sus commits como
// committer; el autor sale de la config (commit_author).
const Committer = "homeassistant-config-sync <noreply@homeassistant-config-sync.invalid>"

// minDeletesForPercent: el umbral porcentual del guard de borrado masivo
// solo aplica desde esta cantidad de borrados, para que borrar 1 de 4
// archivos en una config chica no frene el sync.
const minDeletesForPercent = 5

// Errores de los guards. Se comparan con errors.Is.
var (
	ErrSuspicious = errors.New("snapshot sospechoso")
	ErrMassDelete = errors.New("borrado masivo")
)

// ChangeKind es el tipo de cambio de un path.
type ChangeKind string

const (
	Added    ChangeKind = "A"
	Modified ChangeKind = "M"
	Deleted  ChangeKind = "D"
)

// Change es un cambio del box respecto de la rama box.
type Change struct {
	Path   string
	Kind   ChangeKind
	Mode   string
	OldOID string
	NewOID string
}

// Options configura un snapshot.
type Options struct {
	Matcher *paths.Matcher
	// Ref es la rama box completa, p. ej. "refs/heads/box".
	Ref                string
	MaxFileSize        int64
	DeleteGuardFiles   int
	DeleteGuardPercent int
	// AllowMassDelete saltea el guard de borrado masivo (confirmación explícita).
	AllowMassDelete bool
	// Author es "Nombre <email>".
	Author string
	Now    func() time.Time
	Cache  scan.Cache
	// BeforeCommit corre después de calcular los cambios y antes de escribir
	// cualquier objeto en el repo. Recibe el contenido de lo agregado o
	// modificado. Si devuelve error no se escribe ni se commitea nada (acá
	// se engancha el escáner de secretos en M5).
	BeforeCommit func(ctx context.Context, changes []Change, contents map[string][]byte) error
}

// Result es el resultado de un snapshot.
type Result struct {
	// Parent es la punta de box antes del snapshot ("" si no existía).
	Parent string
	// Commit es el commit nuevo, o "" si no hubo cambios.
	Commit  string
	Changes []Change
	Skipped []scan.Skipped
	// Cache es la cache de stat para el próximo snapshot. Vale aunque haya
	// error, salvo que el error sea del escaneo mismo.
	Cache scan.Cache
}

// Take hace el snapshot.
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
				return res, fmt.Errorf("releyendo %s: %w", c.Path, err)
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
			return res, fmt.Errorf("%s: git calculó el blob %s y el escaneo %s", c.Path, oid, c.NewOID)
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
	committer, _ := gitx.ParseIdent(Committer, when)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commit, err := repo.CommitTree(ctx, tree, parents, Message(changes), author, committer)
	if err != nil {
		return res, err
	}
	if err := repo.UpdateRef(ctx, opts.Ref, commit, parent); err != nil {
		return res, err
	}
	res.Commit = commit
	return res, nil
}

// diff compara el árbol de box con el escaneo. Devuelve los cambios y
// cuántos paths gestionados tenía la base.
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
			continue // es del repo: se conserva tal cual
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
			continue // saltear nunca es borrar
		}
		changes = append(changes, Change{Path: e.Path, Kind: Deleted, Mode: e.Mode, OldOID: e.OID})
	}
	for p, f := range sr.Files {
		if !inBase[p] {
			changes = append(changes, Change{Path: p, Kind: Added, Mode: f.Mode, NewOID: f.OID})
		}
	}

	// Guards de raíz: un mount roto se vería como "borraron todo".
	for i, mp := range m.Mappings() {
		r := roots[i]
		if !r.Active {
			continue
		}
		if !r.Exists {
			return nil, 0, fmt.Errorf("%w: la raíz %s del mapeo %q no existe o no es un directorio", ErrSuspicious, mp.Box, mp.Repo)
		}
		if r.Managed == 0 && baseManagedByMapping[i] > 0 {
			return nil, 0, fmt.Errorf("%w: %s no tiene ningún archivo gestionado pero box tenía %d", ErrSuspicious, mp.Box, baseManagedByMapping[i])
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
		return fmt.Errorf("%w: el snapshot borraría %d de %d archivos gestionados", ErrMassDelete, deleted, baseManaged)
	}
	return nil
}

// maxMessagePaths es cuántos paths se listan en el mensaje del commit.
const maxMessagePaths = 100

// Message arma el mensaje del commit de snapshot.
func Message(changes []Change) string {
	var b strings.Builder
	if len(changes) == 1 {
		fmt.Fprintf(&b, "box: %s\n\n", changes[0].Path)
	} else {
		fmt.Fprintf(&b, "box: %d cambios\n\n", len(changes))
	}
	for i, c := range changes {
		if i == maxMessagePaths {
			fmt.Fprintf(&b, "… y %d más\n", len(changes)-maxMessagePaths)
			break
		}
		fmt.Fprintf(&b, "%s %s\n", c.Kind, c.Path)
	}
	return b.String()
}
