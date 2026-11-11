// Package gitx envuelve el git CLI para el motor: siempre sobre un repo bare,
// siempre con un entorno aislado (sin la config global ni del sistema, sin
// hooks, sin credential helpers, sin agente SSH) y con los protocolos
// restringidos a SSH (design.md §9). En tests se habilita además file://.
//
// Todo se hace con plumbing: el motor nunca tiene un working tree.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ZeroOID es el OID que git usa para "no existe" (SHA-1).
const ZeroOID = "0000000000000000000000000000000000000000"

// EngineIdent es la identidad del motor: committer de todos sus commits y
// autor de los merges.
const EngineIdent = "homeassistant-config-sync <noreply@homeassistant-config-sync.invalid>"

// Options configura cómo se invoca git.
type Options struct {
	// AllowFile habilita remotos file:// y paths locales (v0 y tests).
	AllowFile bool
	// SSHKey es la clave privada de la deploy key. Vacío = sin SSH.
	SSHKey string
	// KnownHosts es el archivo known_hosts fijo.
	KnownHosts string
	// GitBinary permite elegir el ejecutable de git. Vacío = "git" del PATH.
	GitBinary string
}

// Repo es un repo bare manejado por el motor.
type Repo struct {
	dir  string
	opts Options
	git  string
}

// Error es un fallo de un comando git, con su stderr.
type Error struct {
	Args   []string
	Stderr string
	// Stdout se conserva porque algunos comandos (merge-tree) usan un exit
	// distinto de 0 con salida válida.
	Stdout []byte
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *Error) Unwrap() error { return e.Err }

// InitBare crea (o reabre) un repo bare SHA-1 en dir.
func InitBare(ctx context.Context, dir string, opts Options) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := Open(dir, opts)
	// GIT_DIR ya apunta a dir (ver Env), así que init no lleva path.
	if _, err := r.run(ctx, nil, "init", "--quiet", "--bare", "--object-format=sha1", "--initial-branch=main"); err != nil {
		return nil, err
	}
	return r, nil
}

// Open abre un repo bare existente sin chequear nada.
func Open(dir string, opts Options) *Repo {
	g := opts.GitBinary
	if g == "" {
		g = "git"
	}
	return &Repo{dir: dir, opts: opts, git: g}
}

// Dir devuelve el directorio del repo.
func (r *Repo) Dir() string { return r.dir }

// Env devuelve el entorno con el que corre git: se arma de cero, no se
// hereda. En particular no pasan SSH_AUTH_SOCK, GIT_* ni la config del
// usuario.
func (r *Repo) Env() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir, // nada de ~/.gitconfig ni ~/.ssh del usuario
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_DIR=" + r.dir,
	}
	ssh := []string{
		"ssh", "-F", os.DevNull,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=yes",
	}
	if r.opts.KnownHosts != "" {
		ssh = append(ssh, "-o", "UserKnownHostsFile="+shellQuote(r.opts.KnownHosts))
	} else {
		ssh = append(ssh, "-o", "UserKnownHostsFile="+os.DevNull)
	}
	if r.opts.SSHKey != "" {
		ssh = append(ssh, "-i", shellQuote(r.opts.SSHKey))
	}
	env = append(env, "GIT_SSH_COMMAND="+strings.Join(ssh, " "))
	return env
}

// configArgs son los -c que van antes de cada subcomando.
func (r *Repo) configArgs() []string {
	args := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.ssh.allow=always",
		"-c", "credential.helper=",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.autocrlf=false",
		"-c", "gc.auto=0",
	}
	if r.opts.AllowFile {
		args = append(args, "-c", "protocol.file.allow=always")
	}
	return args
}

func (r *Repo) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return r.runEnv(ctx, nil, stdin, args...)
}

func (r *Repo) runEnv(ctx context.Context, extraEnv []string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.git, append(r.configArgs(), args...)...)
	cmd.Env = append(r.Env(), extraEnv...)
	cmd.Dir = r.dir
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, &Error{Args: args, Stderr: stderr.String(), Stdout: stdout.Bytes(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Version devuelve la versión de git como (major, minor).
func (r *Repo) Version(ctx context.Context) (major, minor int, err error) {
	out, err := r.run(ctx, nil, "version")
	if err != nil {
		return 0, 0, err
	}
	// "git version 2.53.0" (puede traer sufijos como ".windows.1")
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return 0, 0, fmt.Errorf("salida inesperada de git version: %q", out)
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("versión de git inesperada: %q", f[2])
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, err
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// RequireVersion falla si git es más viejo que 2.38 (merge-tree --write-tree).
func (r *Repo) RequireVersion(ctx context.Context) error {
	major, minor, err := r.Version(ctx)
	if err != nil {
		return err
	}
	if major < 2 || (major == 2 && minor < 38) {
		return fmt.Errorf("hace falta git >= 2.38, hay %d.%d", major, minor)
	}
	return nil
}

// ResolveRef devuelve el OID de un ref, o "" si no existe.
func (r *Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		var ge *Error
		var ee *exec.ExitError
		if errors.As(err, &ge) && errors.As(ge.Err, &ee) && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeOf devuelve el árbol de un commit.
func (r *Repo) TreeOf(ctx context.Context, commit string) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeEntry es una entrada (blob) de un árbol, recorrido recursivamente.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000"…
	OID  string
	Path string
}

// LsTree lista recursivamente los blobs de un tree-ish. Con treeish vacío
// devuelve nada (árbol vacío).
func (r *Repo) LsTree(ctx context.Context, treeish string) ([]TreeEntry, error) {
	if treeish == "" {
		return nil, nil
	}
	out, err := r.run(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", treeish)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> SP <type> SP <oid> TAB <path>"
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("ls-tree: registro inesperado %q", rec)
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: registro inesperado %q", rec)
		}
		if f[1] != "blob" {
			continue // submódulos (commit) no se gestionan
		}
		entries = append(entries, TreeEntry{Mode: f[0], OID: f[2], Path: string(p)})
	}
	return entries, nil
}

// WriteBlob guarda contenido como blob, sin filtros, y devuelve su OID.
func (r *Repo) WriteBlob(ctx context.Context, content []byte) (string, error) {
	out, err := r.run(ctx, bytes.NewReader(content), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadBlob devuelve el contenido de un blob.
func (r *Repo) ReadBlob(ctx context.Context, oid string) ([]byte, error) {
	return r.run(ctx, nil, "cat-file", "blob", oid)
}

// HasObject dice si el objeto existe en el repo.
func (r *Repo) HasObject(ctx context.Context, oid string) (bool, error) {
	_, err := r.run(ctx, nil, "cat-file", "-e", oid)
	if err == nil {
		return true, nil
	}
	var ge *Error
	var ee *exec.ExitError
	if errors.As(err, &ge) && errors.As(ge.Err, &ee) {
		return false, nil
	}
	return false, err
}

// IndexUpdate es un cambio a aplicar sobre un árbol. OID vacío = borrar.
type IndexUpdate struct {
	Path string
	Mode string // "100644" o "100755"; se ignora si se borra
	OID  string
}

// BuildTree parte del árbol base (o del árbol vacío si base es "") y aplica
// los cambios con un índice temporal. Devuelve el OID del árbol nuevo.
func (r *Repo) BuildTree(ctx context.Context, base string, updates []IndexUpdate) (string, error) {
	tmp, err := os.MkdirTemp(r.dir, "index-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	if base != "" {
		if _, err := r.runEnv(ctx, env, nil, "read-tree", "--end-of-options", base); err != nil {
			return "", err
		}
	}

	// Primero los borrados y después los altas, para que un path que pasa de
	// archivo a directorio (o al revés) no choque consigo mismo.
	var buf bytes.Buffer
	for _, u := range updates {
		if u.OID == "" {
			fmt.Fprintf(&buf, "0 %s\t%s\x00", ZeroOID, u.Path)
		}
	}
	for _, u := range updates {
		if u.OID != "" {
			fmt.Fprintf(&buf, "%s %s\t%s\x00", u.Mode, u.OID, u.Path)
		}
	}
	if buf.Len() > 0 {
		if _, err := r.runEnv(ctx, env, &buf, "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	out, err := r.runEnv(ctx, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Ident es un autor o committer de git.
type Ident struct {
	Name  string
	Email string
	When  time.Time
}

// ParseIdent parsea "Nombre <email>".
func ParseIdent(s string, when time.Time) (Ident, error) {
	name, rest, ok := strings.Cut(s, " <")
	if !ok || !strings.HasSuffix(rest, ">") {
		return Ident{}, fmt.Errorf("identidad inválida %q: esperaba 'Nombre <email>'", s)
	}
	return Ident{Name: name, Email: strings.TrimSuffix(rest, ">"), When: when}, nil
}

func (id Ident) date() string {
	return fmt.Sprintf("%d %s", id.When.Unix(), id.When.Format("-0700"))
}

// CommitTree crea un commit con el árbol y los padres dados.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, msg string, author, committer Ident) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name,
		"GIT_AUTHOR_EMAIL=" + author.Email,
		"GIT_AUTHOR_DATE=" + author.date(),
		"GIT_COMMITTER_NAME=" + committer.Name,
		"GIT_COMMITTER_EMAIL=" + committer.Email,
		"GIT_COMMITTER_DATE=" + committer.date(),
	}
	out, err := r.runEnv(ctx, env, strings.NewReader(msg), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ErrRefMoved indica que un ref no tenía el valor esperado (compare-and-swap).
var ErrRefMoved = errors.New("el ref cambió mientras tanto")

// UpdateRef mueve ref a newOID solo si hoy vale oldOID ("" = no tiene que
// existir).
func (r *Repo) UpdateRef(ctx context.Context, ref, newOID, oldOID string) error {
	if oldOID == "" {
		oldOID = ZeroOID
	}
	_, err := r.run(ctx, nil, "update-ref", "-m", "homeassistant-config-sync", "--no-deref", ref, newOID, oldOID)
	if err == nil {
		return nil
	}
	// No se parsea el stderr (cambia entre versiones): si el ref ya no vale
	// lo esperado, fue un compare-and-swap perdido.
	cur, rerr := r.ResolveRef(ctx, ref)
	if rerr == nil {
		if cur == "" {
			cur = ZeroOID
		}
		if cur != oldOID {
			return fmt.Errorf("%s: %w", ref, ErrRefMoved)
		}
	}
	return err
}

// Fetch trae refspecs desde un remoto (URL directa, sin remotes configurados).
func (r *Repo) Fetch(ctx context.Context, url string, refspecs ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "--prune", "--end-of-options", url}, refspecs...)
	_, err := r.run(ctx, nil, args...)
	return err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
