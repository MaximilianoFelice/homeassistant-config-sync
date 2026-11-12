//go:build integration

// Tests contra un Home Assistant Core real. Los corre scripts/ha-core-test.sh,
// que levanta el contenedor y pasa la URL, el token y el nombre del
// contenedor por variables de entorno.
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
		t.Skip("faltan HCS_HA_URL / HCS_HA_TOKEN / HCS_HA_CONTAINER (corré scripts/ha-core-test.sh)")
	}
	return &Supervisor{BaseURL: url, Token: tok}, container
}

// inContainer corre un comando de shell dentro del contenedor de HA.
func inContainer(t *testing.T, container, script string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec: %v: %s", err, out)
	}
}

func TestIntegrationCheckConfig(t *testing.T) {
	s, c := integrationClient(t)
	ctx := context.Background()
	inContainer(t, c, "cp /config/configuration.yaml /config/configuration.bak")
	t.Cleanup(func() { inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml") })

	got, err := s.CheckConfig(ctx)
	if err != nil || !got.Valid || len(got.Warnings) != 0 {
		t.Fatalf("config base: %+v, %v", got, err)
	}

	// Integración inválida: HA la considera warning, no error.
	inContainer(t, c, `printf '\ntimer:\n  hcs_t:\n    duration: no_es_duracion\n' >> /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || !got.Valid || len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, "\n"), "timer") {
		t.Fatalf("integración inválida: quería válida con warning de timer, dio %+v, %v", got, err)
	}
	if len(NewWarnings(nil, got.Warnings)) == 0 {
		t.Error("NewWarnings tendría que verlo como nuevo")
	}

	// http inválido: el script de la CLI (check_config --json) lo cuenta como
	// error, pero la API REST — la que usa la app en prod — lo devuelve como
	// warning con result "valid". Comprobado contra HA 2026.11.1. Por eso
	// check_warnings: new es el default.
	inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml")
	inContainer(t, c, `sed -i 's/^http:$/http:\n  server_port: no_es_numero/' /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || !got.Valid || !strings.Contains(strings.Join(got.Warnings, "\n"), "'http'") {
		t.Fatalf("http inválido por REST: quería válida con warning de http, dio %+v, %v", got, err)
	}

	// YAML roto: este sí es un error para la API REST.
	inContainer(t, c, "cp /config/configuration.bak /config/configuration.yaml")
	inContainer(t, c, `printf '\nroto: [\n' >> /config/configuration.yaml`)
	got, err = s.CheckConfig(ctx)
	if err != nil || got.Valid || got.Errors == "" {
		t.Fatalf("YAML roto: quería inválida, dio %+v, %v", got, err)
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
	problem := Event{Type: EventConflict, Title: "hcs: conflicto", Message: "test de integración",
		Data: map[string]any{"paths": []string{"config/a.yaml"}}, Persistent: true}
	if err := s.Notify(ctx, problem); err != nil {
		t.Fatalf("notificación persistente: %v", err)
	}
	if err := s.Notify(ctx, Event{Type: EventSyncFinished, Title: "hcs", Message: "ok"}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	// Un notify_service que no existe tiene que fallar con un error de HA,
	// sin cortar el evento ni la notificación.
	s.NotifyService = "notify.no_existe"
	err := s.Notify(ctx, problem)
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.Path, "notify/no_existe") {
		t.Errorf("notify_service inexistente: %v", err)
	}
}

func TestIntegrationBadToken(t *testing.T) {
	s, _ := integrationClient(t)
	s.Token = "token-invalido"
	_, err := s.CheckConfig(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("quería 401, dio %v", err)
	}
	if strings.Contains(err.Error(), "token-invalido") {
		t.Error("el error incluye el token")
	}
}

func TestIntegrationCheckTimeoutIsGenerous(t *testing.T) {
	s, _ := integrationClient(t)
	start := time.Now()
	if _, err := s.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("check_config tardó %v (timeout por defecto %v)", time.Since(start).Round(time.Millisecond), DefaultCheckTimeout)
}
