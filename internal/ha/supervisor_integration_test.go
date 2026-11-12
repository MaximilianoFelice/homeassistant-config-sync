//go:build integration

// Tests against a real Home Assistant Core. They are run by
// scripts/ha-core-test.sh, which starts the container and passes the URL, the
// token and the container name through environment variables.
package ha

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func integrationClient(t *testing.T) (*Supervisor, string) {
	t.Helper()
	url, tok, container := os.Getenv("HCS_HA_URL"), os.Getenv("HCS_HA_TOKEN"), os.Getenv("HCS_HA_CONTAINER")
	if url == "" || tok == "" || container == "" {
		t.Skip("HCS_HA_URL / HCS_HA_TOKEN / HCS_HA_CONTAINER not set (run scripts/ha-core-test.sh)")
	}
	return &Supervisor{BaseURL: url, Token: tok}, container
}

// inContainer runs a shell command inside the HA container.
func inContainer(t *testing.T, container, script string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec: %v: %s", err, out)
	}
}

// TestIntegrationInvalidAutomationIsUnavailable checks the assumption the
// engine relies on: check_config does not catch an automation with an invalid
// trigger, but after a reload HA exposes it as an unavailable entity.
func TestIntegrationInvalidAutomationIsUnavailable(t *testing.T) {
	s, c := integrationClient(t)
	ctx := context.Background()
	inContainer(t, c, "cp /config/automations.yaml /config/automations.bak")
	t.Cleanup(func() {
		inContainer(t, c, "cp /config/automations.bak /config/automations.yaml")
		_ = s.ReloadAll(ctx)
	})

	before, err := s.UnavailableAutomations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inContainer(t, c, `cat >> /config/automations.yaml <<'EOF'
- id: hcs_bad
  alias: hcs bad
  triggers:
    - trigger: no_such_trigger_platform
  actions: []
EOF`)
	cr, err := s.CheckConfig(ctx)
	if err != nil || !cr.Valid {
		t.Fatalf("check_config should still pass (that is the gap): %+v, %v", cr, err)
	}
	if err := s.ReloadAll(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.UnavailableAutomations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := NewItems(before, after); len(got) != 1 || got[0] != "automation.hcs_bad" {
		t.Fatalf("new unavailable automations = %v (before %v, after %v)", got, before, after)
	}
}

func TestIntegrationCheckConfig(t *testing.T) {
	s, c := integrationClient(t)
	ctx := context.Background()
	inContainer(t, c, "cp /config/configuration.yaml /config/configuration.bak")
	t.Cleanup(func() { inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml") })

	got, err := s.CheckConfig(ctx)
	if err != nil || !got.Valid || len(got.Warnings) != 0 {
		t.Fatalf("base config: %+v, %v", got, err)
	}

	// Invalid integration: HA treats it as a warning, not an error.
	inContainer(t, c, `printf '\ntimer:\n  hcs_t:\n    duration: not_a_duration\n' >> /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || !got.Valid || len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, "\n"), "timer") {
		t.Fatalf("invalid integration: want valid with a timer warning, got %+v, %v", got, err)
	}
	if len(NewWarnings(nil, got.Warnings)) == 0 {
		t.Error("NewWarnings should see it as new")
	}

	// Invalid http: the CLI script (check_config --json) counts it as an
	// error, but the REST API (the one the app uses in prod) returns it as a
	// warning with result "valid". Verified against HA 2026.11.1. That is why
	// check_warnings: new is the default.
	inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml")
	inContainer(t, c, `sed -i 's/^http:$/http:\n  server_port: not_a_number/' /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || !got.Valid || !strings.Contains(strings.Join(got.Warnings, "\n"), "'http'") {
		t.Fatalf("invalid http over REST: want valid with an http warning, got %+v, %v", got, err)
	}

	// Broken YAML: this one is an error for the REST API.
	inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml")
	inContainer(t, c, `printf '\nbroken: [\n' >> /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || got.Valid || got.Errors == "" {
		t.Fatalf("broken YAML: want invalid, got %+v, %v", got, err)
	}
}

func TestIntegrationReloadAll(t *testing.T) {
	s, _ := integrationClient(t)
	if err := s.ReloadAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationNotify(t *testing.T) {
	s, _ := integrationClient(t)
	ctx := context.Background()
	problem := Event{Type: EventConflict, Title: "hcs: conflict", Message: "integration test",
		Data: map[string]any{"paths": []string{"config/a.yaml"}}, Persistent: true}
	if err := s.Notify(ctx, problem); err != nil {
		t.Fatalf("persistent notification: %v", err)
	}
	if err := s.Notify(ctx, Event{Type: EventSyncFinished, Title: "hcs", Message: "ok"}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	// A notify_service that does not exist must fail with an HA error,
	// without stopping the event or the notification.
	s.NotifyService = "notify.does_not_exist"
	err := s.Notify(ctx, problem)
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.Path, "notify/does_not_exist") {
		t.Errorf("nonexistent notify_service: %v", err)
	}
}

func TestIntegrationBadToken(t *testing.T) {
	s, _ := integrationClient(t)
	s.Token = "invalid-token"
	_, err := s.CheckConfig(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("want 401, got %v", err)
	}
	if strings.Contains(err.Error(), "invalid-token") {
		t.Error("the error includes the token")
	}
}

func TestIntegrationCheckTimeoutIsGenerous(t *testing.T) {
	s, _ := integrationClient(t)
	start := time.Now()
	if _, err := s.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("check_config took %v (default timeout %v)", time.Since(start).Round(time.Millisecond), DefaultCheckTimeout)
}
