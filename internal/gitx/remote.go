package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrPushRejected means the remote rejected a push because it was not a
// fast-forward (someone pushed in between). Fetch again.
var ErrPushRejected = errors.New("the remote rejected the push (not a fast-forward)")

// ErrAuth means the remote rejected the credentials: the token is wrong,
// expired, or lacks access to the repo.
var ErrAuth = errors.New("the remote rejected the token (wrong, expired, or without access to this repo)")

// authError turns an authentication failure into ErrAuth.
func authError(err error) error {
	var ge *Error
	if errors.As(err, &ge) {
		s := ge.Stderr
		if strings.Contains(s, "Authentication failed") || strings.Contains(s, "could not read Username") ||
			strings.Contains(s, "could not read Password") || strings.Contains(s, "returned error: 401") ||
			strings.Contains(s, "returned error: 403") || strings.Contains(s, "Permission to") {
			return fmt.Errorf("%w: %s", ErrAuth, strings.TrimSpace(s))
		}
	}
	return err
}

// LsRemote returns the remote's branches: short name → OID.
func (r *Repo) LsRemote(ctx context.Context, url string) (map[string]string, error) {
	out, err := r.run(ctx, nil, "ls-remote", "--heads", "--end-of-options", url)
	if err != nil {
		return nil, authError(err)
	}
	heads := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("ls-remote: unexpected line %q", line)
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			heads[name] = oid
		}
	}
	return heads, nil
}

// DeleteRef deletes a ref if it exists.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	cur, err := r.ResolveRef(ctx, ref)
	if err != nil || cur == "" {
		return err
	}
	_, err = r.run(ctx, nil, "update-ref", "-d", ref, cur)
	return err
}

// Push pushes refspecs ("<oid>:refs/heads/x") atomically: either all refs
// are updated or none. It never uses --force.
func (r *Repo) Push(ctx context.Context, url string, refspecs ...string) error {
	if len(refspecs) == 0 {
		return nil
	}
	for _, rs := range refspecs {
		if strings.HasPrefix(rs, "+") {
			return fmt.Errorf("force push not allowed: %q", rs)
		}
	}
	args := append([]string{"push", "--atomic", "--porcelain", "--no-verify", "--end-of-options", url}, refspecs...)
	_, err := r.run(ctx, nil, args...)
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		out := append(append([]byte{}, ge.Stdout...), ge.Stderr...)
		if bytes.Contains(out, []byte("non-fast-forward")) || bytes.Contains(out, []byte("fetch first")) ||
			bytes.Contains(out, []byte("stale info")) {
			return fmt.Errorf("%w: %s", ErrPushRejected, strings.TrimSpace(ge.Stderr))
		}
	}
	return authError(err)
}
