package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// ErrUnrelated indica que dos commits no tienen historia en común.
var ErrUnrelated = errors.New("las historias no tienen ningún commit en común")

// IsAncestor dice si a es ancestro de b (o igual).
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

// MergeMessage es un mensaje informativo de merge-tree sobre uno o más
// paths. Type es estable entre versiones de git: "Auto-merging",
// "CONFLICT (contents)", "CONFLICT (modify/delete)", etc.
type MergeMessage struct {
	Paths   []string
	Type    string
	Message string
}

// MergeTreeResult es el resultado de git merge-tree --write-tree.
type MergeTreeResult struct {
	// Tree es el árbol resultante. Con conflictos, tiene los marcadores de
	// conflicto adentro de los archivos.
	Tree string
	// Clean indica que no hubo conflictos.
	Clean bool
	// Conflicted son los paths con conflicto, como los lista git.
	Conflicted []string
	Messages   []MergeMessage
}

// MergeTree hace un merge de 3 vías entre ours y theirs sin tocar ningún
// working tree ni ref (git >= 2.38).
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
		// Exit 1 = hubo conflictos; la salida igual es válida. Hay que
		// recuperarla del error, porque run solo la devuelve en éxito.
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

// parseMergeTree parsea la salida -z --name-only:
//
//	<tree> NUL
//	[<path> NUL]...           (paths en conflicto)
//	NUL
//	[<n> NUL <path>{n} NUL <tipo> NUL <mensaje> NUL]...
//
// En un merge limpio la salida es solo "<tree> NUL" (las secciones
// siguientes pueden faltar).
func parseMergeTree(out []byte) (*MergeTreeResult, error) {
	fields := bytes.Split(out, []byte{0})
	// El último campo es lo que viene después del último NUL: vacío.
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 || len(fields[0]) == 0 {
		return nil, fmt.Errorf("merge-tree: salida vacía")
	}
	res := &MergeTreeResult{Tree: string(fields[0])}
	i := 1
	for ; i < len(fields) && len(fields[i]) > 0; i++ {
		res.Conflicted = append(res.Conflicted, string(fields[i]))
	}
	i++ // separador vacío
	for i < len(fields) {
		n, err := strconv.Atoi(string(fields[i]))
		if err != nil || n < 0 || i+1+n+2 > len(fields) {
			return nil, fmt.Errorf("merge-tree: mensaje mal formado en el campo %d", i)
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
