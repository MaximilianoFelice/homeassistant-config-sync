package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Supervisor client defaults (design.md §10).
const (
	DefaultCoreURL        = "http://supervisor/core/api"
	DefaultCheckTimeout   = 120 * time.Second
	DefaultRequestTimeout = 30 * time.Second
	// EventType is the event type fired on the HA bus.
	EventType = "homeassistant_config_sync"
	// NotificationID makes the persistent notification update in place
	// instead of stacking up.
	NotificationID = "homeassistant_config_sync"
)

// maxBody bounds how much of an HA response is read.
const maxBody = 1 << 20

// ErrNoToken means there is no SUPERVISOR_TOKEN (not running as an HA app).
var ErrNoToken = errors.New("no SUPERVISOR_TOKEN: the Supervisor client only works inside a Home Assistant app")

// Supervisor talks to Home Assistant Core through the Supervisor proxy,
// authenticated with the SUPERVISOR_TOKEN the app receives with
// homeassistant_api: true. It does not use long-lived tokens.
type Supervisor struct {
	BaseURL        string
	Token          string
	HTTP           *http.Client
	CheckTimeout   time.Duration
	RequestTimeout time.Duration
	// NotifyService is optional ("notify.mobile_app_x"): on top of the
	// persistent notification, anything that needs intervention is sent there.
	NotifyService string
}

// NewSupervisorFromEnv builds the client with SUPERVISOR_TOKEN.
func NewSupervisorFromEnv(notifyService string) (*Supervisor, error) {
	tok := os.Getenv("SUPERVISOR_TOKEN")
	if tok == "" {
		return nil, ErrNoToken
	}
	return &Supervisor{Token: tok, NotifyService: notifyService}, nil
}

// APIError is a non-successful response from HA. It never includes the token.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("home assistant responded %d to %s %s: %s", e.Status, e.Method, e.Path, e.Body)
}

func (s *Supervisor) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return DefaultCoreURL
}

func (s *Supervisor) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// post sends a JSON body and decodes the response into out (if not nil).
func (s *Supervisor) post(ctx context.Context, path string, timeout time.Duration, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		// The net/http error includes the URL, never the headers.
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: http.MethodPost, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("POST %s: unexpected response: %w", path, err)
		}
	}
	return nil
}

// CheckConfig implements Checker with POST /api/config/core/check_config.
func (s *Supervisor) CheckConfig(ctx context.Context) (CheckResult, error) {
	var resp struct {
		Result   string  `json:"result"`
		Errors   *string `json:"errors"`
		Warnings *string `json:"warnings"`
	}
	if err := s.post(ctx, "/config/core/check_config", orDefault(s.CheckTimeout, DefaultCheckTimeout), struct{}{}, &resp); err != nil {
		return CheckResult{}, err
	}
	var warnings []string
	if resp.Warnings != nil {
		for w := range strings.SplitSeq(*resp.Warnings, "\n") {
			if w = strings.TrimSpace(w); w != "" {
				warnings = append(warnings, w)
			}
		}
	}
	switch resp.Result {
	case "valid":
		return CheckResult{Valid: true, Warnings: warnings}, nil
	case "invalid":
		msg := "check_config returned invalid without details"
		if resp.Errors != nil && *resp.Errors != "" {
			msg = *resp.Errors
		}
		return CheckResult{Valid: false, Errors: msg, Warnings: warnings}, nil
	default:
		return CheckResult{}, fmt.Errorf("check_config returned an unknown result: %q", resp.Result)
	}
}

// ReloadAll implements Reloader with homeassistant.reload_all.
func (s *Supervisor) ReloadAll(ctx context.Context) error {
	return s.post(ctx, "/services/homeassistant/reload_all", orDefault(s.RequestTimeout, DefaultRequestTimeout), struct{}{}, nil)
}

// Notify implements Notifier: it always fires the event; if the event needs
// intervention, it creates (or updates) the persistent notification and
// notifies through NotifyService; if the sync is back to normal, it dismisses
// the notification.
func (s *Supervisor) Notify(ctx context.Context, ev Event) error {
	timeout := orDefault(s.RequestTimeout, DefaultRequestTimeout)
	payload := map[string]any{"type": ev.Type, "title": ev.Title, "message": ev.Message}
	for k, v := range ev.Data {
		if _, taken := payload[k]; !taken {
			payload[k] = v
		}
	}
	var errs []error
	if err := s.post(ctx, "/events/"+EventType, timeout, payload, nil); err != nil {
		errs = append(errs, err)
	}

	switch {
	case ev.Persistent:
		note := map[string]string{"notification_id": NotificationID, "title": ev.Title, "message": ev.Message}
		if err := s.post(ctx, "/services/persistent_notification/create", timeout, note, nil); err != nil {
			errs = append(errs, err)
		}
		if domain, service, ok := strings.Cut(s.NotifyService, "."); ok {
			msg := map[string]string{"title": ev.Title, "message": ev.Message}
			if err := s.post(ctx, "/services/"+domain+"/"+service, timeout, msg, nil); err != nil {
				errs = append(errs, err)
			}
		}
	case ev.Type == EventSyncFinished:
		dismiss := map[string]string{"notification_id": NotificationID}
		if err := s.post(ctx, "/services/persistent_notification/dismiss", timeout, dismiss, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
