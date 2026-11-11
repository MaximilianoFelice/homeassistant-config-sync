package ha

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// CommandChecker valida la config corriendo un comando de shell: exit 0 es
// válida; cualquier otro exit es inválida y la salida son los errores. Sirve
// en el v0 para correr el check_config de una imagen de HA con Docker.
type CommandChecker struct {
	Command string
}

// CheckConfig implementa Checker.
func (c CommandChecker) CheckConfig(ctx context.Context) (CheckResult, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c.Command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return CheckResult{Valid: true}, nil
	case errors.As(err, &ee):
		return CheckResult{Valid: false, Errors: strings.TrimSpace(out.String())}, nil
	default:
		return CheckResult{}, err
	}
}
