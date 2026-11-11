package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrPushRejected indica que el remoto rechazó un push por no ser
// fast-forward (alguien pusheó en el medio). Hay que volver a fetchear.
var ErrPushRejected = errors.New("el remoto rechazó el push (no es fast-forward)")

// LsRemote devuelve las ramas del remoto: nombre corto → OID.
func (r *Repo) LsRemote(ctx context.Context, url string) (map[string]string, error) {
	out, err := r.run(ctx, nil, "ls-remote", "--heads", "--end-of-options", url)
	if err != nil {
		return nil, err
	}
	heads := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("ls-remote: línea inesperada %q", line)
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			heads[name] = oid
		}
	}
	return heads, nil
}

// DeleteRef borra un ref si existe.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	cur, err := r.ResolveRef(ctx, ref)
	if err != nil || cur == "" {
		return err
	}
	_, err = r.run(ctx, nil, "update-ref", "-d", ref, cur)
	return err
}

// Push empuja refspecs ("<oid>:refs/heads/x") de forma atómica: o se
// actualizan todos los refs o ninguno. Nunca usa --force.
func (r *Repo) Push(ctx context.Context, url string, refspecs ...string) error {
	if len(refspecs) == 0 {
		return nil
	}
	for _, rs := range refspecs {
		if strings.HasPrefix(rs, "+") {
			return fmt.Errorf("push forzado no permitido: %q", rs)
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
	return err
}
