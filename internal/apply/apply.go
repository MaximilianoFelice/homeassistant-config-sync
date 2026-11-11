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

// Errores del apply. Se comparan con errors.Is.
var (
	// ErrPrecheck: un YAML/JSON de R no parsea. No se tocó el disco.
	ErrPrecheck = errors.New("el pre-check de sintaxis falló")
	// ErrDiskChanged: el disco ya no está como en B (alguien editó desde el
	// snapshot). No se tocó el disco; hay que volver a snapshotear.
	ErrDiskChanged = errors.New("el disco cambió desde el snapshot")
	// ErrUnsafePath: un path a escribir pasa por un symlink. No se tocó el disco.
	ErrUnsafePath = errors.New("path inseguro en el box")
	// ErrRejected: Home Assistant dijo que la config es inválida. Se hizo
	// rollback a B.
	ErrRejected = errors.New("home assistant rechazó la configuración")
	// ErrCheckUnavailable: no se pudo correr el check. Se hizo rollback a B.
	ErrCheckUnavailable = errors.New("no se pudo validar la configuración")
)

// errCrash simula en tests que el proceso muere a mitad del apply.
var errCrash = errors.New("crash simulado")

// Options configura un apply.
type Options struct {
	Matcher     *paths.Matcher
	Checker     ha.Checker
	JournalPath string

	// afterOp es un hook de tests: si devuelve true después de la operación
	// i, Apply sale como si el proceso hubiera muerto (sin rollback).
	afterOp func(i int) bool
}

// Result es el resultado de un apply.
type Result struct {
	Plan *Plan
	// Applied indica que el disco quedó en R (con el check aprobado si hacía
	// falta). El journal queda en fase "checked" hasta que el motor mueva box
	// y llame a Finish.
	Applied bool
	// Checked indica que se corrió el check de config.
	Checked bool
	Check   ha.CheckResult
	// RolledBack indica que se escribió algo y se volvió a B.
	RolledBack bool
}

// Apply lleva el disco de B a R según el plan.
func Apply(ctx context.Context, repo *gitx.Repo, plan *Plan, opts Options) (*Result, error) {
	res := &Result{Plan: plan}
	if len(plan.Ops) == 0 {
		return res, nil
	}
	mappings := opts.Matcher.Mappings()

	// 1. Pre-check de sintaxis, antes de tocar nada.
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

	// 2. Paths seguros y compare-and-swap contra B.
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

	// 3. Write-ahead journal.
	j := &Journal{From: plan.From, To: plan.To, Phase: PhaseWriting, Ops: ops}
	if err := writeJournal(opts.JournalPath, j); err != nil {
		return res, err
	}

	// 4. Escribir.
	for i, op := range ops {
		if err := doOp(op, contents[op.RepoPath], mappings[op.Mapping].Box); err != nil {
			return res, fail(ctx, repo, res, ops[:i+1], opts, fmt.Errorf("%s: %w", op.RepoPath, err))
		}
		if opts.afterOp != nil && opts.afterOp(i) {
			return res, errCrash
		}
	}

	// 5. Check de config, si se tocó algún mapeo que lo pide.
	needsCheck := false
	for _, op := range ops {
		needsCheck = needsCheck || mappings[op.Mapping].Check
	}
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

// fail hace rollback de las operaciones hechas y borra el journal. Si el
// rollback falla, el journal queda para que Recover lo reintente.
func fail(ctx context.Context, repo *gitx.Repo, res *Result, done []Op, opts Options, cause error) error {
	if err := rollback(ctx, repo, done, opts.Matcher.Mappings()); err != nil {
		return fmt.Errorf("%w; además falló el rollback (queda el journal para recuperar): %w", cause, err)
	}
	res.RolledBack = true
	if err := removeJournal(opts.JournalPath); err != nil {
		return fmt.Errorf("%w; además: %w", cause, err)
	}
	return cause
}

// rollback deshace operaciones en orden inverso, dejando cada path como en B.
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
	// OldOID no vacío hace que permFor respete OldPerm.
	old := Op{BoxPath: op.BoxPath, NewOID: op.OldOID, NewMode: op.OldMode, OldOID: op.OldOID, OldPerm: op.OldPerm}
	return doOp(old, content, root)
}

// doOp hace una operación sobre el disco.
func doOp(op Op, content []byte, root string) error {
	if op.IsDelete() {
		if err := os.Remove(op.BoxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		pruneEmptyDirs(filepath.Dir(op.BoxPath), root)
		return nil
	}
	// Si en el lugar hay un directorio (cambio de directorio a archivo), los
	// borrados del plan ya lo vaciaron; si no está vacío, os.Remove falla y
	// se hace rollback.
	if fi, err := os.Lstat(op.BoxPath); err == nil && fi.IsDir() {
		if err := os.Remove(op.BoxPath); err != nil {
			return fmt.Errorf("hay un directorio con contenido no gestionado: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(op.BoxPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(op.BoxPath, content, permFor(op))
}

// permFor conserva los permisos que tenía el archivo y ajusta solo el bit de
// ejecución según el modo de git.
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

// pruneEmptyDirs borra directorios vacíos desde dir hacia arriba, sin tocar
// root.
func pruneEmptyDirs(dir, root string) {
	root = filepath.Clean(root)
	for d := filepath.Clean(dir); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			return
		}
	}
}

// safeParents verifica que ningún componente existente entre root y el
// directorio padre de p sea un symlink: escribir a través de uno podría
// salir del mapeo.
func safeParents(root, p string) error {
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: la raíz %s no es un directorio", ErrUnsafePath, root)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: %s está fuera de %s", ErrUnsafePath, p, root)
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
			return fmt.Errorf("%w: %s es un symlink", ErrUnsafePath, cur)
		}
	}
	return nil
}

// matchesOld dice si el disco está como B para esta operación y devuelve los
// permisos actuales del archivo.
func matchesOld(op Op) (bool, uint32, error) {
	st, err := diskState(op.BoxPath)
	if err != nil {
		return false, 0, err
	}
	if op.OldOID == "" {
		// Archivo nuevo: no tiene que existir (un directorio sí puede existir,
		// porque los borrados del mismo plan lo pueden vaciar).
		return !st.exists || st.dir, 0, nil
	}
	return st.exists && !st.dir && st.oid == op.OldOID && st.mode == op.OldMode, st.perm, nil
}

// atState dice si el disco está en el estado (oid, mode) dado.
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

// notExist incluye ENOTDIR: un path cuyo padre es un archivo (cambio de
// archivo a directorio) tampoco existe.
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
		return disk{}, fmt.Errorf("%w: %s no es un archivo regular", ErrUnsafePath, p)
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
