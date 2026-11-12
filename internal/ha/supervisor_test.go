package ha

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "token-de-prueba-que-nunca-se-loguea"

type call struct {
	Path string
	Auth string
	Body map[string]any
}

// fakeCore imita el proxy del Supervisor hacia Core.
type fakeCore struct {
	mu      sync.Mutex
	calls   []call
	handler func(w http.ResponseWriter, r *http.Request) bool // true = ya respondió
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{Path: r.Method + " " + r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
	f.mu.Unlock()
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "content-type", http.StatusBadRequest)
		return
	}
	if f.handler != nil && f.handler(w, r) {
		return
	}
	_, _ = w.Write([]byte("[]"))
}

func (f *fakeCore) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Path)
	}
	return out
}

func newClient(t *testing.T, f *fakeCore) *Supervisor {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Supervisor{BaseURL: srv.URL + "/core/api", Token: token, HTTP: srv.Client()}
}

func TestCheckConfig(t *testing.T) {
	tests := []struct {
		name   string
		resp   string
		status int
		want   CheckResult
		errHas string
	}{
		{"válida", `{"result": "valid", "errors": null, "warnings": null}`, 200, CheckResult{Valid: true}, ""},
		{"inválida", `{"result": "invalid", "errors": "Invalid config for 'automation' at automations.yaml, line 3", "warnings": null}`, 200,
			CheckResult{Valid: false, Errors: "Invalid config for 'automation' at automations.yaml, line 3"}, ""},
		{"inválida sin detalle", `{"result": "invalid", "errors": null}`, 200, CheckResult{Valid: false, Errors: "check_config devolvió invalid sin detalle"}, ""},
		{"resultado raro", `{"result": "maybe"}`, 200, CheckResult{}, "desconocido"},
		{"no autorizado", `401: Unauthorized`, 401, CheckResult{}, "401"},
		{"basura", `<html>`, 200, CheckResult{}, "respuesta inesperada"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCore{handler: func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.resp))
				return true
			}}
			got, err := newClient(t, f).CheckConfig(context.Background())
			if tt.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("quería un error con %q, dio %v", tt.errHas, err)
				}
				if strings.Contains(err.Error(), token) {
					t.Error("el error incluye el token")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("= %+v, %v; quería %+v", got, err, tt.want)
			}
			c := f.calls[0]
			if c.Path != "POST /core/api/config/core/check_config" || c.Auth != "Bearer "+token {
				t.Errorf("request = %+v", c)
			}
		})
	}
}

func TestCheckConfigTimeout(t *testing.T) {
	release := make(chan struct{})
	f := &fakeCore{handler: func(http.ResponseWriter, *http.Request) bool {
		<-release
		return false
	}}
	s := newClient(t, f)
	defer close(release)
	s.CheckTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := s.CheckConfig(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("quería timeout, dio %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("el timeout no se respetó")
	}
}

func TestReloadAll(t *testing.T) {
	f := &fakeCore{}
	if err := newClient(t, f).ReloadAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := f.paths(); len(p) != 1 || p[0] != "POST /core/api/services/homeassistant/reload_all" {
		t.Errorf("llamadas = %v", p)
	}
}

func TestNotifyProblemCreatesPersistentNotification(t *testing.T) {
	f := &fakeCore{}
	s := newClient(t, f)
	s.NotifyService = "notify.mobile_app_pixel"
	ev := Event{Type: EventConflict, Title: "Config Sync: conflicto", Message: "Conflicto en automations.yaml",
		Data: map[string]any{"paths": []string{"config/automations.yaml"}, "type": "no-pisa"}, Persistent: true}
	if err := s.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /core/api/events/homeassistant_config_sync",
		"POST /core/api/services/persistent_notification/create",
		"POST /core/api/services/notify/mobile_app_pixel",
	}
	if got := f.paths(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("llamadas = %v", got)
	}
	evBody := f.calls[0].Body
	if evBody["type"] != EventConflict || evBody["paths"] == nil {
		t.Errorf("evento = %v (Data no puede pisar type)", evBody)
	}
	if f.calls[1].Body["notification_id"] != NotificationID {
		t.Errorf("notificación = %v", f.calls[1].Body)
	}
}

func TestNotifySuccessDismissesNotification(t *testing.T) {
	f := &fakeCore{}
	s := newClient(t, f)
	s.NotifyService = "notify.mobile_app_pixel"
	if err := s.Notify(context.Background(), Event{Type: EventSyncFinished, Message: "ok"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /core/api/events/homeassistant_config_sync", "POST /core/api/services/persistent_notification/dismiss"}
	if got := f.paths(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("llamadas = %v (no se avisa al celular por un sync normal)", got)
	}
}

func TestNotifyKeepsGoingAfterAFailure(t *testing.T) {
	f := &fakeCore{handler: func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/events/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}}
	err := newClient(t, f).Notify(context.Background(), Event{Type: EventRejected, Persistent: true})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("quería el error del evento, dio %v", err)
	}
	if len(f.paths()) != 2 {
		t.Errorf("la notificación persistente igual tiene que salir: %v", f.paths())
	}
}

func TestNewSupervisorFromEnv(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "")
	if _, err := NewSupervisorFromEnv(""); !errors.Is(err, ErrNoToken) {
		t.Errorf("sin token: %v", err)
	}
	t.Setenv("SUPERVISOR_TOKEN", "x")
	s, err := NewSupervisorFromEnv("notify.x")
	if err != nil || s.Token != "x" || s.base() != DefaultCoreURL {
		t.Errorf("= %+v, %v", s, err)
	}
}

func TestCheckConfigWarnings(t *testing.T) {
	f := &fakeCore{handler: func(w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"result": "valid", "errors": null, "warnings": "Invalid config for 'input_boolean' at configuration.yaml, line 3: x\nOtro aviso\n"}`))
		return true
	}}
	got, err := newClient(t, f).CheckConfig(context.Background())
	if err != nil || !got.Valid || len(got.Warnings) != 2 || got.Warnings[1] != "Otro aviso" {
		t.Errorf("= %+v, %v", got, err)
	}
}

func TestNewWarningsIgnoresLineNumbers(t *testing.T) {
	before := []string{"Invalid config for 'timer' at configuration.yaml, line 40: bad"}
	after := []string{"Invalid config for 'timer' at configuration.yaml, line 52: bad", "Invalid config for 'http' at x.yaml, line 1: nuevo"}
	if got := NewWarnings(before, after); len(got) != 1 || !strings.Contains(got[0], "http") {
		t.Errorf("= %v", got)
	}
}

// Salida real de `check_config --json` (HA 2026.11.1) con una integración
// inválida (warning) y una config core inválida (error).
const checkJSON = `Testing configuration at /config
{"check_type": "full", "total_errors": 1, "total_warnings": 1,
 "errors": {"http": ["Invalid config for 'http' at configuration.yaml, line 120: expected a port number", {"server_port": "no_es_numero"}]},
 "warnings": {"input_boolean": ["Invalid config for 'input_boolean' at configuration.yaml, line 117: invalid boolean value", {"e2e_x": {"initial": "no"}}]}}`

func TestCommandCheckerReadsHAJSON(t *testing.T) {
	cmd := "cat <<'EOF'\n" + checkJSON + "\nEOF\nexit 1"
	got, err := CommandChecker{Command: cmd}.CheckConfig(context.Background())
	if err != nil || got.Valid || !strings.Contains(got.Errors, "'http'") || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "input_boolean") {
		t.Errorf("= %+v, %v", got, err)
	}
	ok := `{"total_errors": 0, "total_warnings": 1, "errors": {}, "warnings": {"x": ["aviso"]}}`
	got, err = CommandChecker{Command: "echo '" + ok + "'"}.CheckConfig(context.Background())
	if err != nil || !got.Valid || len(got.Warnings) != 1 {
		t.Errorf("válida con warning = %+v, %v", got, err)
	}
}

func TestCommandChecker(t *testing.T) {
	ok, err := CommandChecker{Command: "true"}.CheckConfig(context.Background())
	if err != nil || !ok.Valid {
		t.Errorf("true: %+v %v", ok, err)
	}
	bad, err := CommandChecker{Command: "echo 'Invalid config' >&2; exit 1"}.CheckConfig(context.Background())
	if err != nil || bad.Valid || bad.Errors != "Invalid config" {
		t.Errorf("exit 1: %+v %v", bad, err)
	}
}
