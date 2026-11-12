package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// ErrUnrelated means two commits share no history.
var ErrUnrelated = errors.New("the histories have no commit in common")

// IsAncestor reports whether a is an ancestor of b (or equal).
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// MergeMessage is an informational merge-tree message about one or more
// paths. Type is stable across git versions: "Auto-merging",
// "CONFLICT (contents)", "CONFLICT (modify/delete)", etc.
type MergeMessage struct {
	Paths   []string
	Type    string
	Message string
}

// MergeTreeResult is the result of git merge-tree --write-tree.
type MergeTreeResult struct {
	// Tree is the resulting tree. With conflicts, it has the conflict
	// markers inside the files.
	Tree string
	// Clean means there were no conflicts.
	Clean bool
	// Conflicted are the conflicted paths, as git lists them.
	Conflicted []string
	Messages   []MergeMessage
}

// MergeTree does a 3-way merge between ours and theirs without touching any
// working tree or ref (git >= 2.38).
func (r *Repo) MergeTree(ctx context.Context, ours, theirs string) (*MergeTreeResult, error) {
	out, err := r.run(ctx, nil, "merge-tree", "--write-tree", "-z", "--name-only", "--messages", ours, theirs)
	clean := err == nil
	if err != nil {
		if exitCode(err) != 1 {
			var ge *Error
			if errors.As(err, &ge) && bytes.Contains([]byte(ge.Stderr), []byte("unrelated histories")) {
				return nil, ErrUnrelated
			}
			return nil, err
		}
		// Exit 1 = there were conflicts; the output is still valid. It has
		// to be recovered from the error, because run only returns it on success.
		var ge *Error
		if !errors.As(err, &ge) {
			return nil, err
		}
		out = ge.Stdout
	}
	res, perr := parseMergeTree(out)
	if perr != nil {
		return nil, perr
	}
	res.Clean = clean
	return res, nil
}

// parseMergeTree parses the -z --name-only output:
//
//	<tree> NUL
//	[<path> NUL]...           (conflicted paths)
//	NUL
//	[<n> NUL <path>{n} NUL <type> NUL <message> NUL]...
//
// In a clean merge the output is only "<tree> NUL" (the following
// sections may be missing).
func parseMergeTree(out []byte) (*MergeTreeResult, error) {
	fields := bytes.Split(out, []byte{0})
	// The last field is what comes after the last NUL: empty.
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 || len(fields[0]) == 0 {
		return nil, fmt.Errorf("merge-tree: empty output")
	}
	res := &MergeTreeResult{Tree: string(fields[0])}
	i := 1
	for ; i < len(fields) && len(fields[i]) > 0; i++ {
		res.Conflicted = append(res.Conflicted, string(fields[i]))
	}
	i++ // empty separator
	for i < len(fields) {
		n, err := strconv.Atoi(string(fields[i]))
		if err != nil || n < 0 || i+1+n+2 > len(fields) {
			return nil, fmt.Errorf("merge-tree: malformed message at field %d", i)
		}
		i++
		var m MergeMessage
		for j := 0; j < n; j++ {
			m.Paths = append(m.Paths, string(fields[i+j]))
		}
		i += n
		m.Type = string(fields[i])
		m.Message = string(bytes.TrimRight(fields[i+1], "\n"))
		i += 2
		res.Messages = append(res.Messages, m)
	}
	return res, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
