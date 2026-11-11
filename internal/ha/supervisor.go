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

// Valores por defecto del cliente del Supervisor (design.md §10).
const (
	DefaultCoreURL        = "http://supervisor/core/api"
	DefaultCheckTimeout   = 120 * time.Second
	DefaultRequestTimeout = 30 * time.Second
	// EventType es el tipo de evento que se dispara en el bus de HA.
	EventType = "homeassistant_config_sync"
	// NotificationID hace que la notificación persistente se actualice en
	// vez de apilarse.
	NotificationID = "homeassistant_config_sync"
)

// maxBody acota lo que se lee de una respuesta de HA.
const maxBody = 1 << 20

// ErrNoToken indica que no hay SUPERVISOR_TOKEN (no corre como app de HA).
var ErrNoToken = errors.New("no hay SUPERVISOR_TOKEN: el cliente del Supervisor solo funciona dentro de una app de Home Assistant")

// Supervisor habla con Home Assistant Core a través del proxy del
// Supervisor, autenticado con el SUPERVISOR_TOKEN que recibe la app con
// homeassistant_api: true. No usa tokens de larga duración.
type Supervisor struct {
	BaseURL        string
	Token          string
	HTTP           *http.Client
	CheckTimeout   time.Duration
	RequestTimeout time.Duration
	// NotifyService es opcional ("notify.mobile_app_x"): además de la
	// notificación persistente, se manda ahí lo que pida intervención.
	NotifyService string
}

// NewSupervisorFromEnv arma el cliente con SUPERVISOR_TOKEN.
func NewSupervisorFromEnv(notifyService string) (*Supervisor, error) {
	tok := os.Getenv("SUPERVISOR_TOKEN")
	if tok == "" {
		return nil, ErrNoToken
	}
	return &Supervisor{Token: tok, NotifyService: notifyService}, nil
}

// APIError es una respuesta no exitosa de HA. Nunca incluye el token.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("home assistant respondió %d a %s %s: %s", e.Status, e.Method, e.Path, e.Body)
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

// post manda un JSON y decodifica la respuesta en out (si no es nil).
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
		// El error de net/http incluye la URL, nunca los headers.
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
			return fmt.Errorf("POST %s: respuesta inesperada: %w", path, err)
		}
	}
	return nil
}

// CheckConfig implementa Checker con POST /api/config/core/check_config.
func (s *Supervisor) CheckConfig(ctx context.Context) (CheckResult, error) {
	var resp struct {
		Result   string  `json:"result"`
		Errors   *string `json:"errors"`
		Warnings *string `json:"warnings"`
	}
	if err := s.post(ctx, "/config/core/check_config", orDefault(s.CheckTimeout, DefaultCheckTimeout), struct{}{}, &resp); err != nil {
		return CheckResult{}, err
	}
	switch resp.Result {
	case "valid":
		return CheckResult{Valid: true}, nil
	case "invalid":
		msg := "check_config devolvió invalid sin detalle"
		if resp.Errors != nil && *resp.Errors != "" {
			msg = *resp.Errors
		}
		return CheckResult{Valid: false, Errors: msg}, nil
	default:
		return CheckResult{}, fmt.Errorf("check_config devolvió un resultado desconocido: %q", resp.Result)
	}
}

// ReloadAll implementa Reloader con homeassistant.reload_all.
func (s *Supervisor) ReloadAll(ctx context.Context) error {
	return s.post(ctx, "/services/homeassistant/reload_all", orDefault(s.RequestTimeout, DefaultRequestTimeout), struct{}{}, nil)
}

// Notify implementa Notifier: siempre dispara el evento; si el evento pide
// intervención, crea (o actualiza) la notificación persistente y avisa por
// NotifyService; si el sync volvió a la normalidad, la descarta.
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
