package secrets

import (
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
)

// Los secretos de prueba se arman en runtime (pseudoaleatorios con semilla
// fija, para que tengan la entropía de uno real) así este archivo no dispara
// ni la push protection de GitHub ni un escaneo del propio repo.
var (
	fakeGitHubPAT = "gh" + "p_" + randChars(1, 36, alnum)
	fakePEM       = "-----BEGIN " + "RSA PRIVATE KEY-----\n" +
		randChars(2, 64, b64) + "\n" + randChars(3, 64, b64) + "\n" + randChars(4, 40, b64) + "\n" +
		"-----END " + "RSA PRIVATE KEY-----\n"
	fakePEMBody  = fakePEM[len("-----BEGIN RSA PRIVATE KEY-----\n"):][:30]
	fakePassword = "hunter2-" + "Sup3rS3cret"
)

const (
	alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64   = alnum + "+/"
)

func randChars(seed int64, n int, alphabet string) string {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // datos de prueba
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func scanner(t *testing.T, cfg config.SecretScan) *Scanner {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type hit struct {
	line     int
	rule     string
	severity string
}

func TestScanFindsSecrets(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		secret  string // lo que nunca puede aparecer en la salida
		want    []hit
	}{
		{
			"password literal en YAML", "config/configuration.yaml",
			"http:\n  api_password: " + fakePassword + "\n",
			fakePassword, []hit{{2, RuleHALiteral, Block}},
		},
		{
			"token anidado en una lista", "config/sensors.yaml",
			"- platform: rest\n  resource: http://x/api\n  headers:\n    x: y\n  token: " + fakePassword + "\n",
			fakePassword, []hit{{5, RuleHALiteral, Block}},
		},
		{
			"password numérico", "config/alarm.yaml",
			"alarm:\n  password: 482913\n",
			"482913", []hit{{2, RuleHALiteral, Block}},
		},
		{
			"client_secret", "config/packages/spotify.yaml",
			"spotify:\n  client_id: abc\n  client_secret: " + fakePassword + "\n",
			fakePassword, []hit{{3, RuleHALiteral, Block}},
		},
		{
			"URL con credenciales", "config/cameras.yaml",
			"camera:\n  - platform: generic\n    still_image_url: http://admin:" + fakePassword + "@10.0.0.5/snap.jpg\n",
			fakePassword, []hit{{3, RuleURLWithCreds, Block}},
		},
		{
			"token de GitHub (gitleaks)", "config/scripts/deploy.sh",
			"#!/bin/sh\nexport GH=" + fakeGitHubPAT + "\n",
			fakeGitHubPAT, []hit{{2, "github-pat", Block}},
		},
		{
			"clave privada (gitleaks)", "app_configs/x/key.conf",
			"ssl on\n" + fakePEM,
			fakePEMBody, []hit{{2, "private-key", Block}},
		},
		{
			"webhook_id es advertencia", "config/automations.yaml",
			"- id: '1'\n  trigger:\n    - platform: webhook\n      webhook_id: abre-el-porton-xyz\n",
			"abre-el-porton-xyz", []hit{{4, RuleHAWebhookID, Warn}},
		},
	}
	s := scanner(t, config.SecretScan{WebhookID: Warn})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Scan(tt.path, []byte(tt.content))
			for _, w := range tt.want {
				found := false
				for _, f := range got {
					if f.Line == w.line && f.Rule == w.rule && f.Severity == w.severity {
						found = true
					}
				}
				if !found {
					t.Errorf("faltó %+v; hallazgos: %+v", w, got)
				}
			}
			// El valor nunca aparece en la salida.
			_, err := s.Check(map[string][]byte{tt.path: []byte(tt.content)})
			for _, f := range got {
				if strings.Contains(f.String(), tt.secret) {
					t.Errorf("el hallazgo expone el valor: %s", f)
				}
			}
			if err != nil && strings.Contains(err.Error(), tt.secret) {
				t.Errorf("el error expone el valor: %v", err)
			}
		})
	}
}

func TestScanIgnoresSafeConfig(t *testing.T) {
	files := map[string]string{
		"config/configuration.yaml": `homeassistant:
  name: Casa
  latitude: !secret latitude
http:
  api_password: !secret http_password
  ssl_certificate: /ssl/fullchain.pem
mqtt:
  password: !env_var MQTT_PASSWORD
recorder:
  db_url: !secret db_url
template:
  - sensor:
      - name: token actual
        state: "{{ states('input_text.token') }}"
notify:
  - platform: rest
    password: ''
    token_type: bearer
    verify_ssl: true
`,
		"config/automations.yaml": `- id: '1699999999999'
  alias: Luz del pasillo
  trigger:
    - platform: device
      device_id: 4f9a8c1e2b3d4e5f6a7b8c9d0e1f2a3b
      domain: binary_sensor
      entity_id: binary_sensor.pasillo_movimiento
      type: motion
  action:
    - service: light.turn_on
      target:
        entity_id: light.pasillo
      data:
        brightness_pct: 40
`,
		"config/blueprints/automation/x.yaml": `blueprint:
  name: X
  input:
    password:
      name: Clave
  domain: automation
trigger:
  - platform: state
    entity_id: !input sensor
`,
		"config/.storage/lovelace": `{"version": 1, "data": {"config": {"views": [{"title": "Casa", "cards": [{"type": "entities", "entities": ["light.pasillo"]}]}]}}}`,
		"config/esphome/living.yaml": `esphome:
  name: living
api:
  encryption:
    key: !secret living_api_key
ota:
  password: !secret ota_password
wifi:
  ssid: !secret wifi_ssid
  password: !secret wifi_password
`,
	}
	contents := map[string][]byte{}
	for p, c := range files {
		contents[p] = []byte(c)
	}
	s := scanner(t, config.SecretScan{WebhookID: Warn})
	warnings, err := s.Check(contents)
	if err != nil {
		t.Errorf("falsos positivos bloqueantes: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("advertencias inesperadas: %+v", warnings)
	}
}

func TestWebhookIDCanBlock(t *testing.T) {
	s := scanner(t, config.SecretScan{WebhookID: Block})
	_, err := s.Check(map[string][]byte{"config/a.yaml": []byte("trigger:\n  webhook_id: abc123\n")})
	if !errors.Is(err, ErrSecretsFound) {
		t.Errorf("con webhook_id: block quería bloqueo, dio %v", err)
	}
}

func TestAllowFingerprint(t *testing.T) {
	content := []byte("rest:\n  password: " + fakePassword + "\n")
	s := scanner(t, config.SecretScan{})
	_, err := s.Check(map[string][]byte{"config/rest.yaml": content})
	var be *BlockedError
	if !errors.As(err, &be) || len(be.Findings) != 1 {
		t.Fatalf("quería un hallazgo, dio %v", err)
	}
	fp := be.Findings[0].Fingerprint
	if len(fp) != 64 {
		t.Fatalf("fingerprint = %q", fp)
	}

	allowed := scanner(t, config.SecretScan{Allow: []string{fp}})
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": content}); err != nil {
		t.Errorf("con el fingerprint permitido no debería bloquear: %v", err)
	}
	// Mover el mismo archivo a otro path invalida la excepción…
	if _, err := allowed.Check(map[string][]byte{"config/otro.yaml": content}); err == nil {
		t.Error("otro path no debería heredar la excepción")
	}
	// …y editar la línea también.
	edited := []byte("rest:\n  password: " + fakePassword + "-v2\n")
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": edited}); err == nil {
		t.Error("editar la línea debería invalidar la excepción")
	}
	// Agregar líneas arriba no la invalida (no depende del número de línea).
	moved := append([]byte("# comentario\n"), content...)
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": moved}); err != nil {
		t.Errorf("mover la línea no debería invalidar la excepción: %v", err)
	}
}

func TestBrokenYAMLStillRunsGitleaks(t *testing.T) {
	s := scanner(t, config.SecretScan{})
	content := []byte("a: [roto\nexport GH=" + fakeGitHubPAT + "\n")
	if _, err := s.Check(map[string][]byte{"config/x.yaml": content}); !errors.Is(err, ErrSecretsFound) {
		t.Errorf("un YAML roto igual tiene que pasar por gitleaks: %v", err)
	}
}

func TestSensitiveKeys(t *testing.T) {
	for k, want := range map[string]bool{
		"password": true, "api_password": true, "Password": true, "refresh_token": true,
		"client_secret": true, "api_key": true, "pass": true,
		"token_type": false, "secrets": false, "bypass": false, "passive": false, "keyboard": false,
	} {
		if got := sensitive(k); got != want {
			t.Errorf("sensitive(%q) = %v, quería %v", k, got, want)
		}
	}
}
