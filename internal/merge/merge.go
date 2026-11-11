// Package merge implementa el paso 3 del ciclo (design.md §6): combinar la
// rama box con main y decidir qué commit R hay que aplicar al box y
// publicar. No toca el disco ni mueve refs: solo crea objetos en el repo.
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

// Kind es el caso de merge que se dio.
type Kind string

const (
	// UpToDate: box y main son el mismo commit. R = box.
	UpToDate Kind = "up_to_date"
	// FastForwardMain: main no existe o es ancestro de box (solo cambió el
	// box). R = box; main avanza por fast-forward.
	FastForwardMain Kind = "ff_main"
	// FastForwardBox: box es ancestro de main (solo cambió main). R = main,
	// o un commit sobre main si hubo que conservar paths push_only.
	FastForwardBox Kind = "ff_box"
	// Merged: las dos ramas cambiaron y el merge salió limpio. R es un
	// commit de merge con padres [main, box].
	Merged Kind = "merged"
	// Conflict: el merge tiene conflictos que no se resuelven solos. No hay R.
	Conflict Kind = "conflict"
)

// autoResolvable son los tipos de conflicto que se pueden resolver solos
// cuando todos sus paths son del box (push_only). Los conflictos de
// estructura (archivo/directorio, renombres) siempre frenan, porque git deja
// paths auxiliares en el árbol.
var autoResolvable = []string{
	"CONFLICT (contents)",
	"CONFLICT (add/add)",
	"CONFLICT (modify/delete)",
}

// ConflictInfo describe un conflicto.
type ConflictInfo struct {
	Paths   []string
	Type    string
	Message string
}

// Options configura un merge.
type Options struct {
	// BoxWins dice si un path es autoritativo del box (push_only): en R
	// siempre queda la versión de box, sea cual sea el caso de merge.
	BoxWins func(repoPath string) bool
	Now     func() time.Time
}

// Result es el resultado de un merge.
type Result struct {
	Kind Kind
	Box  string
	Main string
	// R es el commit a aplicar y publicar; vacío si hubo conflicto.
	R string
	// ConflictPaths son los paths en conflicto, ordenados y sin repetir.
	ConflictPaths []string
	Conflicts     []ConflictInfo
	// Overridden son los paths push_only en los que se descartó lo que traía
	// main para quedarse con la versión del box.
	Overridden []string
}

// Merge combina box con main. box tiene que existir; main puede ser ""
// (remoto vacío).
func Merge(ctx context.Context, repo *gitx.Repo, box, main string, opts Options) (*Result, error) {
	if box == "" {
		return nil, errors.New("merge: la rama box no existe")
	}
	res := &Result{Box: box, Main: main}

	switch {
	case main == "":
		res.Kind, res.R = FastForwardMain, box
		return res, nil
	case main == box:
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
		// ours = main, theirs = box: el primer padre del merge es main.
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
			// Todos los conflictos son de paths push_only: los resuelve el
			// override de abajo con la versión del box.
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
		res.R, err = repo.CommitTree(ctx, tree, []string{main}, message("box: conserva paths push_only sobre main", res.Overridden), id, id)
	case Merged:
		res.R, err = repo.CommitTree(ctx, tree, []string{main, box}, message("Merge box en main", res.Overridden), id, id)
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

// overrideBoxWins deja en tree la versión de box de cada path push_only.
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
	for p, b := range inBox { // están en box y main los borró
		updates = append(updates, gitx.IndexUpdate{Path: p, Mode: b.Mode, OID: b.OID})
		overridden = append(overridden, p)
	}
	if len(updates) == 0 {
		return tree, nil, nil
	}
	sort.Strings(overridden)
	newTree, err := repo.BuildTree(ctx, tree, updates)
	if err != nil {
		return "", nil, fmt.Errorf("conservando paths push_only: %w", err)
	}
	return newTree, overridden, nil
}

func message(title string, overridden []string) string {
	var b strings.Builder
	b.WriteString(title + "\n")
	if len(overridden) > 0 {
		b.WriteString("\nPaths push_only que conservan la versión del box:\n")
		for _, p := range overridden {
			b.WriteString("  " + p + "\n")
		}
	}
	return b.String()
}
