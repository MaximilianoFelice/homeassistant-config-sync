package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrWorkTreeChanged means git refused to update the work tree because files
// it had to touch are not what the index says (someone edited them since the
// snapshot) or an untracked file is in the way.
var ErrWorkTreeChanged = errors.New("files on the box changed or are in the way")

// WorkTree runs git against a work tree and an index that live outside the
// bare repo.
type WorkTree struct {
	r     *Repo
	dir   string
	index string
}

// WorkTree returns a handle on dir as work tree, with index as the index file.
func (r *Repo) WorkTree(dir, index string) *WorkTree {
	return &WorkTree{r: r, dir: dir, index: index}
}

// Dir returns the work tree directory.
func (w *WorkTree) Dir() string { return w.dir }

func (w *WorkTree) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, w.r.git, append(append(w.r.configArgs(), "-c", "core.bare=false"), args...)...)
	cmd.Env = append(w.r.Env(),
		"GIT_WORK_TREE="+w.dir,
		"GIT_INDEX_FILE="+w.index,
		// Paths are always literal: a file named "*.yaml" is just that file.
		"GIT_LITERAL_PATHSPECS=1",
	)
	cmd.Dir = w.dir
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, &Error{Args: args, Stderr: stderr.String(), Stdout: stdout.Bytes(), Err: err}
	}
	return stdout.Bytes(), nil
}

// ReadTree loads a tree into the index without touching the work tree. ""
// empties the index.
func (w *WorkTree) ReadTree(ctx context.Context, treeish string) error {
	if treeish == "" {
		_, err := w.run(ctx, nil, "read-tree", "--empty")
		return err
	}
	_, err := w.run(ctx, nil, "read-tree", "--end-of-options", treeish)
	return err
}

// AddAll stages the whole work tree, like `git add -A`: new, modified and
// deleted files, honoring .gitignore. Symlinks are stored as links.
func (w *WorkTree) AddAll(ctx context.Context) error {
	_, err := w.run(ctx, nil, "add", "-A")
	return err
}

// TrackedIgnored lists tracked paths that the .gitignore files now ignore.
// Only .gitignore files count: the repo's internal exclude file is about
// what git looks at, not about what the user ignores.
func (w *WorkTree) TrackedIgnored(ctx context.Context) ([]string, error) {
	out, err := w.run(ctx, nil, "ls-files", "-z", "--cached", "--ignored", "--exclude-per-directory=.gitignore")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// RemoveCached removes paths from the index, leaving the files on disk.
func (w *WorkTree) RemoveCached(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := w.run(ctx, nulList(paths), "rm", "--cached", "--quiet", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// WriteTree writes the index as a tree.
func (w *WorkTree) WriteTree(ctx context.Context) (string, error) {
	out, err := w.run(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Update moves the index and the work tree from one tree to another (`git
// read-tree -m -u`). Git refuses, and nothing is written, if a file it has to
// touch was edited since the index was refreshed or an untracked file is in
// the way: that comes back as ErrWorkTreeChanged.
func (w *WorkTree) Update(ctx context.Context, from, to string) error {
	if from == "" {
		from = EmptyTree
	}
	_, err := w.run(ctx, nil, "read-tree", "-m", "-u", "--end-of-options", from, to)
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) && (strings.Contains(ge.Stderr, "not uptodate") ||
		strings.Contains(ge.Stderr, "would be overwritten") || strings.Contains(ge.Stderr, "would be lost")) {
		return fmt.Errorf("%w: %s", ErrWorkTreeChanged, strings.TrimSpace(ge.Stderr))
	}
	return err
}

// Missing lists tracked paths that are not on disk.
func (w *WorkTree) Missing(ctx context.Context) ([]string, error) {
	out, err := w.run(ctx, nil, "ls-files", "-z", "--deleted")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// Restore writes paths from the index to the work tree.
func (w *WorkTree) Restore(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := w.run(ctx, nulList(paths), "checkout-index", "--force", "-z", "--stdin")
	return err
}

func splitNul(out []byte) []string {
	var paths []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths
}

func nulList(paths []string) io.Reader {
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return &b
}
