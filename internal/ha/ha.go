// Package ha abstracts what the engine needs from Home Assistant (design.md
// §10). In M4 there is only the config check; the implementation against the
// Supervisor (SUPERVISOR_TOKEN) arrives in M7.
package ha

import (
	"context"
	"regexp"
)

// CheckResult is the result of check_config.
type CheckResult struct {
	Valid bool
	// Errors is the error text HA returns (empty if valid).
	Errors string
	// Warnings are HA's warnings, one per element. Note: an integration with
	// an invalid config is a warning, not an error (HA starts anyway and
	// disables it), so "valid" is not enough to guarantee nothing breaks.
	Warnings []string
}

var lineRef = regexp.MustCompile(`,? line \d+`)

// NewWarnings returns the warnings in after that were not in before,
// comparing without line numbers (which shift with any edit).
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

// Checker validates the configuration on the live disk.
type Checker interface {
	CheckConfig(ctx context.Context) (CheckResult, error)
}

// Noop is a Checker that always reports valid. It is the v0 default when
// there is no Supervisor.
type Noop struct{}

// CheckConfig implements Checker.
func (Noop) CheckConfig(context.Context) (CheckResult, error) {
	return CheckResult{Valid: true}, nil
}

// Reloader reloads Home Assistant's YAML config without restarting.
type Reloader interface {
	ReloadAll(ctx context.Context) error
}

// Event types (design.md §10). They match the engine states plus
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

// Event is what the engine tells Home Assistant when a cycle finishes.
type Event struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
	// Persistent means a persistent notification is warranted on top of the
	// event (the states that need the user to do something).
	Persistent bool `json:"persistent"`
}

// Notifier publishes events and notifications in Home Assistant.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Restarter restarts Home Assistant Core.
type Restarter interface {
	Restart(ctx context.Context) error
}

// AutomationStates reports the automations that are unavailable, which is
// how HA exposes an automation whose config is invalid: check_config does not
// catch those, they only show up after a reload.
type AutomationStates interface {
	UnavailableAutomations(ctx context.Context) ([]string, error)
}

// NewItems returns the elements of after that are not in before.
func NewItems(before, after []string) []string {
	seen := map[string]bool{}
	for _, s := range before {
		seen[s] = true
	}
	var out []string
	for _, s := range after {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
}

// ReloadAll implements Reloader by doing nothing.
func (Noop) ReloadAll(context.Context) error { return nil }

// Restart implements Restarter by doing nothing.
func (Noop) Restart(context.Context) error { return nil }

// UnavailableAutomations implements AutomationStates: without Home Assistant
// there is nothing to inspect.
func (Noop) UnavailableAutomations(context.Context) ([]string, error) { return nil, nil }

// Notify implements Notifier by doing nothing.
func (Noop) Notify(context.Context, Event) error { return nil }

// CheckerFunc adapts a function to Checker (tests and command checker).
type CheckerFunc func(ctx context.Context) (CheckResult, error)

// CheckConfig implements Checker.
func (f CheckerFunc) CheckConfig(ctx context.Context) (CheckResult, error) { return f(ctx) }
