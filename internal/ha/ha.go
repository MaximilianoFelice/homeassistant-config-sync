// Package ha abstrae lo que el motor necesita de Home Assistant (design.md
// §10). En M4 solo está el chequeo de config; la implementación contra el
// Supervisor (SUPERVISOR_TOKEN) llega en M7.
package ha

import (
	"context"
	"regexp"
)

// CheckResult es el resultado de check_config.
type CheckResult struct {
	Valid bool
	// Errors es el texto de errores que devuelve HA (vacío si es válida).
	Errors string
	// Warnings son los avisos de HA, uno por elemento. Ojo: una integración
	// con config inválida es un warning, no un error (HA arranca igual y la
	// desactiva), así que "válida" no alcanza para no romper nada.
	Warnings []string
}

var lineRef = regexp.MustCompile(`,? line \d+`)

// NewWarnings devuelve los warnings de after que no estaban en before,
// comparando sin los números de línea (que se corren con cualquier edición).
func NewWarnings(before, after []string) []string {
	seen := map[string]bool{}
	for _, w := range before {
		seen[lineRef.ReplaceAllString(w, "")] = true
	}
	var out []string
	for _, w := range after {
		if !seen[lineRef.ReplaceAllString(w, "")] {
			out = append(out, w)
		}
	}
	return out
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

// Reloader recarga la config YAML de Home Assistant sin reiniciar.
type Reloader interface {
	ReloadAll(ctx context.Context) error
}

// Tipos de evento (design.md §10). Coinciden con los estados del motor más
// sync_finished.
const (
	EventSyncFinished   = "sync_finished"
	EventConflict       = "conflict"
	EventRejected       = "rejected"
	EventBlockedSecrets = "blocked_secrets"
	EventBootstrap      = "bootstrap_review"
	EventSuspicious     = "suspicious_snapshot"
	EventError          = "error"
)

// Event es lo que el motor le cuenta a Home Assistant al terminar un ciclo.
type Event struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
	// Persistent indica que además del evento conviene una notificación
	// persistente (los estados que piden que el usuario haga algo).
	Persistent bool `json:"persistent"`
}

// Notifier publica eventos y notificaciones en Home Assistant.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// ReloadAll implementa Reloader sin hacer nada.
func (Noop) ReloadAll(context.Context) error { return nil }

// Notify implementa Notifier sin hacer nada.
func (Noop) Notify(context.Context, Event) error { return nil }

// CheckerFunc adapta una función a Checker (tests y checker por comando).
type CheckerFunc func(ctx context.Context) (CheckResult, error)

// CheckConfig implementa Checker.
func (f CheckerFunc) CheckConfig(ctx context.Context) (CheckResult, error) { return f(ctx) }
