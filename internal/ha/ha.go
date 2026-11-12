// Package ha abstracts what the engine needs from Home Assistant: the config
// check (informational), the reload, and events and notifications. The
// implementation goes through the Supervisor (SUPERVISOR_TOKEN).
package ha

import (
	"context"
)

// CheckResult is the result of check_config.
type CheckResult struct {
	Valid bool
	// Errors is the error text HA returns (empty if valid).
	Errors string
	// Warnings are HA's warnings, one per element (an integration with an
	// invalid config is a warning: HA starts anyway and disables it).
	Warnings []string
}

// Checker checks the configuration on the live disk.
type Checker interface {
	CheckConfig(ctx context.Context) (CheckResult, error)
}

// Noop is a Checker, Reloader and Notifier that does nothing. It is the
// default outside the app.
type Noop struct{}

// CheckConfig implements Checker.
func (Noop) CheckConfig(context.Context) (CheckResult, error) {
	return CheckResult{Valid: true}, nil
}

// Reloader reloads Home Assistant's YAML config without restarting.
type Reloader interface {
	ReloadAll(ctx context.Context) error
}

// Event types. They match the engine states that need a person, plus
// sync_finished and secrets_found.
const (
	EventSyncFinished   = "sync_finished"
	EventConflict       = "conflict"
	EventReview         = "review"
	EventMassDelete     = "mass_delete"
	EventTooLarge       = "too_large"
	EventPathCollision  = "path_collision"
	EventBlockedSecrets = "blocked_secrets"
	EventSecretsFound   = "secrets_found"
	EventCheckFailed    = "check_failed"
	EventError          = "error"
)

// Event is what the engine tells Home Assistant when a cycle finishes.
type Event struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
	// Persistent means a persistent notification is warranted on top of the
	// event (things that need the user to do something).
	Persistent bool `json:"persistent"`
}

// Notifier publishes events and notifications in Home Assistant.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// ReloadAll implements Reloader by doing nothing.
func (Noop) ReloadAll(context.Context) error { return nil }

// Notify implements Notifier by doing nothing.
func (Noop) Notify(context.Context, Event) error { return nil }

// CheckerFunc adapts a function to Checker (tests and command checker).
type CheckerFunc func(ctx context.Context) (CheckResult, error)

// CheckConfig implements Checker.
func (f CheckerFunc) CheckConfig(ctx context.Context) (CheckResult, error) { return f(ctx) }
