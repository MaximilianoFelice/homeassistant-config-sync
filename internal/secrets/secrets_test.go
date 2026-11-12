package secrets

import (
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
)

// Test secrets are built at runtime (pseudorandom with a fixed seed, so they
// have the entropy of a real one) so this file triggers neither GitHub push
// protection nor a scan of this repo.
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
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // test data
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
		secret  string // what must never appear in the output
		want    []hit
	}{
		{
			"literal password in YAML", "config/configuration.yaml",
			"http:\n  api_password: " + fakePassword + "\n",
			fakePassword, []hit{{2, RuleHALiteral, Block}},
		},
		{
			"token nested in a list", "config/sensors.yaml",
			"- platform: rest\n  resource: http://x/api\n  headers:\n    x: y\n  token: " + fakePassword + "\n",
			fakePassword, []hit{{5, RuleHALiteral, Block}},
		},
		{
			"numeric password", "config/alarm.yaml",
			"alarm:\n  password: 482913\n",
			"482913", []hit{{2, RuleHALiteral, Block}},
		},
		{
			"client_secret", "config/packages/spotify.yaml",
			"spotify:\n  client_id: abc\n  client_secret: " + fakePassword + "\n",
			fakePassword, []hit{{3, RuleHALiteral, Block}},
		},
		{
			"URL with credentials", "config/cameras.yaml",
			"camera:\n  - platform: generic\n    still_image_url: http://admin:" + fakePassword + "@10.0.0.5/snap.jpg\n",
			fakePassword, []hit{{3, RuleURLWithCreds, Block}},
		},
		{
			"GitHub token (gitleaks)", "config/scripts/deploy.sh",
			"#!/bin/sh\nexport GH=" + fakeGitHubPAT + "\n",
			fakeGitHubPAT, []hit{{2, "github-pat", Block}},
		},
		{
			"private key (gitleaks)", "app_configs/x/key.conf",
			"ssl on\n" + fakePEM,
			fakePEMBody, []hit{{2, "private-key", Block}},
		},
		{
			"webhook_id is a warning", "config/automations.yaml",
			"- id: '1'\n  trigger:\n    - platform: webhook\n      webhook_id: open-the-gate-xyz\n",
			"open-the-gate-xyz", []hit{{4, RuleHAWebhookID, Warn}},
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
					t.Errorf("missing %+v; findings: %+v", w, got)
				}
			}
			// The value never appears in the output.
			_, err := s.Check(map[string][]byte{tt.path: []byte(tt.content)})
			for _, f := range got {
				if strings.Contains(f.String(), tt.secret) {
					t.Errorf("the finding exposes the value: %s", f)
				}
			}
			if err != nil && strings.Contains(err.Error(), tt.secret) {
				t.Errorf("the error exposes the value: %v", err)
			}
		})
	}
}

func TestScanIgnoresSafeConfig(t *testing.T) {
	files := map[string]string{
		"config/configuration.yaml": `homeassistant:
  name: Home
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
      - name: current token
        state: "{{ states('input_text.token') }}"
notify:
  - platform: rest
    password: ''
    token_type: bearer
    verify_ssl: true
`,
		"config/automations.yaml": `- id: '1699999999999'
  alias: Hallway light
  trigger:
    - platform: device
      device_id: 4f9a8c1e2b3d4e5f6a7b8c9d0e1f2a3b
      domain: binary_sensor
      entity_id: binary_sensor.hallway_motion
      type: motion
  action:
    - service: light.turn_on
      target:
        entity_id: light.hallway
      data:
        brightness_pct: 40
`,
		"config/blueprints/automation/x.yaml": `blueprint:
  name: X
  input:
    password:
      name: Password
  domain: automation
trigger:
  - platform: state
    entity_id: !input sensor
`,
		"config/.storage/lovelace": `{"version": 1, "data": {"config": {"views": [{"title": "Home", "cards": [{"type": "entities", "entities": ["light.hallway"]}]}]}}}`,
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
		t.Errorf("blocking false positives: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", warnings)
	}
}

func TestWebhookIDCanBlock(t *testing.T) {
	s := scanner(t, config.SecretScan{WebhookID: Block})
	_, err := s.Check(map[string][]byte{"config/a.yaml": []byte("trigger:\n  webhook_id: abc123\n")})
	if !errors.Is(err, ErrSecretsFound) {
		t.Errorf("with webhook_id: block wanted a block, got %v", err)
	}
}

func TestAllowFingerprint(t *testing.T) {
	content := []byte("rest:\n  password: " + fakePassword + "\n")
	s := scanner(t, config.SecretScan{})
	_, err := s.Check(map[string][]byte{"config/rest.yaml": content})
	var be *BlockedError
	if !errors.As(err, &be) || len(be.Findings) != 1 {
		t.Fatalf("wanted one finding, got %v", err)
	}
	fp := be.Findings[0].Fingerprint
	if len(fp) != 64 {
		t.Fatalf("fingerprint = %q", fp)
	}

	allowed := scanner(t, config.SecretScan{Allow: []string{fp}})
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": content}); err != nil {
		t.Errorf("with the fingerprint allowed it should not block: %v", err)
	}
	// Moving the same file to another path invalidates the exception…
	if _, err := allowed.Check(map[string][]byte{"config/other.yaml": content}); err == nil {
		t.Error("another path should not inherit the exception")
	}
	// …and so does editing the line.
	edited := []byte("rest:\n  password: " + fakePassword + "-v2\n")
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": edited}); err == nil {
		t.Error("editing the line should invalidate the exception")
	}
	// Adding lines above does not invalidate it (it does not depend on the line number).
	moved := append([]byte("# comment\n"), content...)
	if _, err := allowed.Check(map[string][]byte{"config/rest.yaml": moved}); err != nil {
		t.Errorf("moving the line should not invalidate the exception: %v", err)
	}
}

func TestBrokenYAMLStillRunsGitleaks(t *testing.T) {
	s := scanner(t, config.SecretScan{})
	content := []byte("a: [broken\nexport GH=" + fakeGitHubPAT + "\n")
	if _, err := s.Check(map[string][]byte{"config/x.yaml": content}); !errors.Is(err, ErrSecretsFound) {
		t.Errorf("broken YAML must still go through gitleaks: %v", err)
	}
}

func TestSensitiveKeys(t *testing.T) {
	for k, want := range map[string]bool{
		"password": true, "api_password": true, "Password": true, "refresh_token": true,
		"client_secret": true, "api_key": true, "pass": true,
		"token_type": false, "secrets": false, "bypass": false, "passive": false, "keyboard": false,
	} {
		if got := sensitive(k); got != want {
			t.Errorf("sensitive(%q) = %v, want %v", k, got, want)
		}
	}
}
