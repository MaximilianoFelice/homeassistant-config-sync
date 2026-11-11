// Package ha abstrae lo que el motor necesita de Home Assistant (design.md
// §10). En M4 solo está el chequeo de config; la implementación contra el
// Supervisor (SUPERVISOR_TOKEN) llega en M7.
package ha

import "context"

// CheckResult es el resultado de check_config.
type CheckResult struct {
	Valid bool
	// Errors es el texto de errores que devuelve HA (vacío si es válida).
	Errors string
}

// Checker valida la configuración que está en el disco vivo.
type Checker interface {
	CheckConfig(ctx context.Context) (CheckResult, error)
}

// Noop es un Checker que siempre da válido. Es el default del v0 cuando no
// hay Supervisor.
type Noop struct{}

// CheckConfig implementa Checker.
func (Noop) CheckConfig(context.Context) (CheckResult, error) {
	return CheckResult{Valid: true}, nil
}

// CheckerFunc adapta una función a Checker (tests y checker por comando).
type CheckerFunc func(ctx context.Context) (CheckResult, error)

// CheckConfig implementa Checker.
func (f CheckerFunc) CheckConfig(ctx context.Context) (CheckResult, error) { return f(ctx) }
