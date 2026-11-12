// Package gitx wraps the git CLI for the engine: a bare repo in the app's
// state dir, always with an isolated environment (no global or system
// config, no hooks, no credential helpers) and with protocols restricted to
// HTTPS. Tests also enable file://.
//
// The repo is bare; WorkTree runs git against a work tree and an index that
// live elsewhere (the mounted folders and the state dir), so no .git is ever
// written into the synced folders.
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

// ZeroOID is the OID git uses for "does not exist" (SHA-1).
const ZeroOID = "0000000000000000000000000000000000000000"

// EngineIdent is the engine's identity: committer of all its commits and
// author of merges.
const EngineIdent = "homeassistant-config-sync <noreply@homeassistant-config-sync.invalid>"

// DefaultUsername is sent with the token when no username is configured.
// GitHub and most hosts only look at the token.
const DefaultUsername = "x-access-token"

// Options configures how git is invoked.
type Options struct {
	// AllowFile enables file:// remotes and local paths (tests).
	AllowFile bool
	// AllowHTTP enables plain http:// remotes (tests only).
	AllowHTTP bool
	// Token is the HTTPS access token. It reaches git only through the
	// askpass program's environment: never the URL, the config or argv.
	Token string
	// Username goes with the token. Empty = DefaultUsername.
	Username string
	// AskPass is the askpass program (see WriteAskPass). Required for a token.
	AskPass string
	// GitBinary selects the git executable. Empty = "git" from PATH.
	GitBinary string
}

// askPassScript answers git's prompts from the environment, so the token is
// never written to disk.
const askPassScript = `#!/bin/sh
case "$1" in
  Username*) printf '%s\n' "$HCS_GIT_USERNAME" ;;
  *) printf '%s\n' "$HCS_GIT_TOKEN" ;;
esac
`

// WriteAskPass writes the askpass program to path (it holds no secrets).
func WriteAskPass(path string) error {
	return os.WriteFile(path, []byte(askPassScript), 0o700)
}

// Repo is a bare repo managed by the engine.
type Repo struct {
	dir  string
	opts Options
	git  string
}

// Error is a failed git command, with its stderr.
type Error struct {
	Args   []string
	Stderr string
	// Stdout is kept because some commands (merge-tree) exit non-zero
	// with valid output.
	Stdout []byte
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *Error) Unwrap() error { return e.Err }

// InitBare creates (or reopens) a SHA-1 bare repo in dir.
func InitBare(ctx context.Context, dir string, opts Options) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := Open(dir, opts)
	// GIT_DIR already points to dir (see Env), so init takes no path.
	if _, err := r.run(ctx, nil, "init", "--quiet", "--bare", "--object-format=sha1", "--initial-branch=main"); err != nil {
		return nil, err
	}
	return r, nil
}

// Open opens an existing bare repo without checking anything.
func Open(dir string, opts Options) *Repo {
	g := opts.GitBinary
	if g == "" {
		g = "git"
	}
	return &Repo{dir: dir, opts: opts, git: g}
}

// Dir returns the repo directory.
func (r *Repo) Dir() string { return r.dir }

// Env returns the environment git runs with: built from scratch, not
// inherited. In particular GIT_*, credential helpers and the user's config
// do not pass through.
func (r *Repo) Env() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir, // no user ~/.gitconfig or ~/.git-credentials
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_DIR=" + r.dir,
	}
	if r.opts.Token != "" && r.opts.AskPass != "" {
		user := r.opts.Username
		if user == "" {
			user = DefaultUsername
		}
		env = append(env, "GIT_ASKPASS="+r.opts.AskPass, "HCS_GIT_USERNAME="+user, "HCS_GIT_TOKEN="+r.opts.Token)
	} else {
		env = append(env, "GIT_ASKPASS=/bin/false")
	}
	return env
}

// configArgs are the -c flags that go before every subcommand.
func (r *Repo) configArgs() []string {
	args := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "credential.helper=",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.autocrlf=false",
		"-c", "core.quotePath=false",
		"-c", "gc.auto=0",
	}
	if r.opts.AllowFile {
		args = append(args, "-c", "protocol.file.allow=always")
	}
	if r.opts.AllowHTTP {
		args = append(args, "-c", "protocol.http.allow=always")
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

// Version returns the git version as (major, minor).
func (r *Repo) Version(ctx context.Context) (major, minor int, err error) {
	out, err := r.run(ctx, nil, "version")
	if err != nil {
		return 0, 0, err
	}
	// "git version 2.53.0" (may carry suffixes like ".windows.1")
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return 0, 0, fmt.Errorf("unexpected git version output: %q", out)
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected git version: %q", f[2])
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, err
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// RequireVersion fails if git is older than 2.42 (merge-tree --write-tree
// needs 2.38, --attr-source 2.42).
func (r *Repo) RequireVersion(ctx context.Context) error {
	major, minor, err := r.Version(ctx)
	if err != nil {
		return err
	}
	if major < 2 || (major == 2 && minor < 42) {
		return fmt.Errorf("git >= 2.42 is required, found %d.%d", major, minor)
	}
	return nil
}

// ResolveRef returns a ref's OID, or "" if it does not exist.
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

// TreeOf returns a commit's tree.
func (r *Repo) TreeOf(ctx context.Context, commit string) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeEntry is a (blob) entry of a tree, walked recursively.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000"…
	OID  string
	Path string
}

// LsTree recursively lists the blobs of a tree-ish. An empty treeish
// returns nothing (empty tree).
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
			return nil, fmt.Errorf("ls-tree: unexpected record %q", rec)
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: unexpected record %q", rec)
		}
		if f[1] != "blob" {
			continue // submodules (commit) are not managed
		}
		entries = append(entries, TreeEntry{Mode: f[0], OID: f[2], Path: string(p)})
	}
	return entries, nil
}

// WriteBlob stores content as a blob, without filters, and returns its OID.
func (r *Repo) WriteBlob(ctx context.Context, content []byte) (string, error) {
	out, err := r.run(ctx, bytes.NewReader(content), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadBlob returns a blob's content.
func (r *Repo) ReadBlob(ctx context.Context, oid string) ([]byte, error) {
	return r.run(ctx, nil, "cat-file", "blob", oid)
}

// ReadBlobs returns the content of several blobs with a single git process.
func (r *Repo) ReadBlobs(ctx context.Context, oids []string) (map[string][]byte, error) {
	blobs := map[string][]byte{}
	if len(oids) == 0 {
		return blobs, nil
	}
	out, err := r.run(ctx, strings.NewReader(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for len(out) > 0 {
		header, rest, ok := bytes.Cut(out, []byte{'\n'})
		if !ok {
			return nil, fmt.Errorf("cat-file: truncated output")
		}
		f := strings.Fields(string(header))
		if len(f) == 2 && f[1] == "missing" {
			out = rest
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("cat-file: unexpected header %q", header)
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n > len(rest) {
			return nil, fmt.Errorf("cat-file: unexpected header %q", header)
		}
		blobs[f[0]] = rest[:n]
		out = rest[n:]
		if len(out) > 0 && out[0] == '\n' {
			out = out[1:]
		}
	}
	return blobs, nil
}

// HasObject reports whether the object exists in the repo.
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

// IndexUpdate is a change to apply to a tree. Empty OID = delete.
type IndexUpdate struct {
	Path string
	Mode string // "100644" or "100755"; ignored on delete
	OID  string
}

// BuildTree starts from the base tree (or the empty tree if base is "") and
// applies the changes with a temporary index. Returns the new tree's OID.
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

	// Deletions first, then additions, so a path that turns from file into
	// directory (or the other way) does not collide with itself.
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

// Ident is a git author or committer.
type Ident struct {
	Name  string
	Email string
	When  time.Time
}

// ParseIdent parses "Name <email>".
func ParseIdent(s string, when time.Time) (Ident, error) {
	name, rest, ok := strings.Cut(s, " <")
	if !ok || !strings.HasSuffix(rest, ">") {
		return Ident{}, fmt.Errorf("invalid identity %q: expected 'Name <email>'", s)
	}
	return Ident{Name: name, Email: strings.TrimSuffix(rest, ">"), When: when}, nil
}

func (id Ident) date() string {
	return fmt.Sprintf("%d %s", id.When.Unix(), id.When.Format("-0700"))
}

// CommitTree creates a commit with the given tree and parents.
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

// ErrRefMoved means a ref did not have the expected value (compare-and-swap).
var ErrRefMoved = errors.New("the ref changed in the meantime")

// UpdateRef moves ref to newOID only if it currently equals oldOID ("" = it
// must not exist).
func (r *Repo) UpdateRef(ctx context.Context, ref, newOID, oldOID string) error {
	if oldOID == "" {
		oldOID = ZeroOID
	}
	_, err := r.run(ctx, nil, "update-ref", "-m", "homeassistant-config-sync", "--no-deref", ref, newOID, oldOID)
	if err == nil {
		return nil
	}
	// stderr is not parsed (it changes between versions): if the ref no
	// longer has the expected value, a compare-and-swap was lost.
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

// Fetch fetches refspecs from a remote (direct URL, no configured remotes).
func (r *Repo) Fetch(ctx context.Context, url string, refspecs ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "--prune", "--end-of-options", url}, refspecs...)
	_, err := r.run(ctx, nil, args...)
	return authError(err)
}

// EmptyTree is the OID of the empty tree (SHA-1).
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Change is a path that differs between two trees.
type Change struct {
	// Status is git's letter: A (added), M (modified), D (deleted), T (type
	// changed, e.g. file to symlink).
	Status string `json:"status"`
	Path   string `json:"path"`
	// OID is the new blob ("" for deletions).
	OID string `json:"-"`
}

// DiffTrees lists the paths that differ between two trees (or commits).
// "" stands for the empty tree. Renames are reported as delete + add.
func (r *Repo) DiffTrees(ctx context.Context, from, to string) ([]Change, error) {
	if from == "" {
		from = EmptyTree
	}
	if to == "" {
		to = EmptyTree
	}
	out, err := r.run(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--raw", "--end-of-options", from, to)
	if err != nil {
		return nil, err
	}
	return parseRaw(out)
}

// parseRaw parses `--raw -z` output: ":<mode> <mode> <oid> <oid> <status>\0<path>\0"...
func parseRaw(out []byte) ([]Change, error) {
	var changes []Change
	fields := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("diff: unexpected record %q", fields[i])
		}
		c := Change{Status: meta[4][:1], Path: string(fields[i+1])}
		if c.Status != "D" {
			c.OID = meta[3]
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// ObjectSizes returns the size of each object.
func (r *Repo) ObjectSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	sizes := map[string]int64{}
	if len(oids) == 0 {
		return sizes, nil
	}
	in := strings.Join(oids, "\n") + "\n"
	out, err := r.run(ctx, strings.NewReader(in), "cat-file", "--batch-check=%(objectname) %(objectsize)")
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		oid, size, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cat-file: unexpected line %q", line)
		}
		sizes[oid] = n
	}
	return sizes, nil
}
