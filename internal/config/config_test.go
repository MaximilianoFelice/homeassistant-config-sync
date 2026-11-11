package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() *Config {
	c := Default()
	c.Repository = "git@github.com:owner/repo.git"
	return c
}

func TestDefaultIsValidOnceRepositoryIsSet(t *testing.T) {
	if err := validConfig().Validate(ValidateOptions{}); err != nil {
		t.Fatalf("la config por defecto debería ser válida: %v", err)
	}
	if err := Default().Validate(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("sin repository debería fallar por repository, dio %v", err)
	}
}

func TestParseKeepsDefaultsAndRejectsUnknownKeys(t *testing.T) {
	c, err := Parse([]byte(`
repository: git@github.com:owner/repo.git
sync_interval_minutes: 0
secret_scan:
  allow: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.SyncIntervalMinutes != 0 {
		t.Errorf("sync_interval_minutes = %d, quería 0", c.SyncIntervalMinutes)
	}
	if c.MainBranch != "main" || c.Mode != ModeBidirectional || len(c.Mappings) != 2 {
		t.Errorf("se perdieron defaults: %+v", c)
	}
	if c.SecretScan.WebhookID != SeverityWarn {
		t.Errorf("un objeto parcial pisó el default de webhook_id: %q", c.SecretScan.WebhookID)
	}

	if _, err := Parse([]byte("repositry: x\n")); err == nil {
		t.Error("una clave desconocida debería ser un error")
	}
	if c, err := Parse(nil); err != nil || c.MainBranch != "main" {
		t.Errorf("un archivo vacío debería dar los defaults: %v %+v", err, c)
	}
}

func TestParseReadsSupervisorJSON(t *testing.T) {
	c, err := Parse([]byte(`{"repository": "git@github.com:o/r.git", "mode": "push_only",
		"mappings": [{"repo": "config", "box": "/homeassistant", "include": ["*.yaml"], "push_only": [], "exclude": []}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModePushOnly || len(c.Mappings) != 1 || c.Mappings[0].Include[0] != "*.yaml" {
		t.Errorf("JSON mal leído: %+v", c)
	}
	if err := c.Validate(ValidateOptions{}); err != nil {
		t.Errorf("debería ser válida: %v", err)
	}
}

func TestLoadResolvesRelativeBoxPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte(`
repository: /tmp/origin.git
mappings:
  - repo: config
    box: box/homeassistant
    include: ["*.yaml"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "box", "homeassistant"); c.Mappings[0].Box != want {
		t.Errorf("box = %q, quería %q", c.Mappings[0].Box, want)
	}
	if err := c.Validate(ValidateOptions{AllowFileRemote: true}); err != nil {
		t.Errorf("debería ser válida con AllowFileRemote: %v", err)
	}
}

func TestValidateReportsEveryError(t *testing.T) {
	c := validConfig()
	c.MainBranch = "box"
	c.Mode = "both"
	c.MaxFileSizeKB = 0
	c.DeleteGuardPercent = 0
	c.CommitAuthor = "sin email"
	c.SecretScan.Allow = []string{"nope"}
	c.NotifyService = "notify/../../evil"
	err := c.Validate(ValidateOptions{})
	if err == nil {
		t.Fatal("quería errores")
	}
	for _, field := range []string{"box_branch", "mode", "max_file_size_kb", "delete_guard_percent", "commit_author", "secret_scan.allow[0]", "notify_service"} {
		if !strings.Contains(err.Error(), field+":") {
			t.Errorf("falta el error de %s en:\n%v", field, err)
		}
	}
}

func TestValidateMappings(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // vacío = válida
	}{
		{"defaults", func(*Config) {}, ""},
		{"sin mapeos", func(c *Config) { c.Mappings = nil }, "al menos un mapeo"},
		{"repo absoluto", func(c *Config) { c.Mappings[0].Repo = "/config" }, "mappings[0].repo"},
		{"repo con ..", func(c *Config) { c.Mappings[0].Repo = "../config" }, "mappings[0].repo"},
		{"repo sucio", func(c *Config) { c.Mappings[0].Repo = "config/" }, "mappings[0].repo"},
		{"repo .git", func(c *Config) { c.Mappings[0].Repo = ".git/x" }, "mappings[0].repo"},
		{"repo anidado", func(c *Config) { c.Mappings[1].Repo = "config/apps" }, "se solapa"},
		{"box relativo", func(c *Config) { c.Mappings[0].Box = "homeassistant" }, "mappings[0].box"},
		{"box raíz", func(c *Config) { c.Mappings[0].Box = "/" }, "mappings[0].box"},
		{"box anidado", func(c *Config) { c.Mappings[1].Box = "/homeassistant/apps" }, "se solapa"},
		{"box hermano con prefijo común", func(c *Config) { c.Mappings[1].Box = "/homeassistant2" }, ""},
		{"glob inválido", func(c *Config) { c.Mappings[0].Include = []string{"[a"} }, "include[0]"},
		{"glob absoluto", func(c *Config) { c.Mappings[0].Exclude = []string{"/x"} }, "exclude[0]"},
		{"glob vacío", func(c *Config) { c.Mappings[0].PushOnly = []string{""} }, "push_only[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.Mappings = DefaultMappings()
			tt.mutate(c)
			err := c.Validate(ValidateOptions{})
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("no quería error, dio %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("quería un error con %q, dio %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"main", "box", "box/rpi4", "release-1.0"} {
		if err := validateBranch(ok); err != nil {
			t.Errorf("%q debería ser válida: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-main", "a..b", "a b", "a:b", "x.lock", "a/", "/a", "a//b", "a@{1}", "a~1", "a^", "a*", "a\x01"} {
		if err := validateBranch(bad); err == nil {
			t.Errorf("%q debería ser inválida", bad)
		}
	}
}

func TestValidateRepoURL(t *testing.T) {
	tests := []struct {
		url       string
		allowFile bool
		ok        bool
	}{
		{"git@github.com:owner/repo.git", false, true},
		{"github.com:owner/repo", false, true},
		{"ssh://git@github.com/owner/repo.git", false, true},
		{"ssh://git@gitea.local:2222/owner/repo.git", false, true},

		{"", false, false},
		{"https://github.com/owner/repo.git", false, false},
		{"https://ghp_token@github.com/owner/repo.git", false, false},
		{"http://github.com/owner/repo.git", false, false},
		{"git://github.com/owner/repo.git", false, false},
		{"ssh://user:pass@github.com/owner/repo.git", false, false},
		{"ssh://-oProxyCommand=sh/repo", false, false},
		{"-oProxyCommand=sh", false, false},
		{"-x@host:repo", false, false},
		{"ext::sh -c touch% /tmp/pwned", false, false},
		{"ssh://github.com", false, false},
		{" git@github.com:owner/repo.git", false, false},
		{"git@github.com:owner/repo.git\n", false, false},
		{"owner/repo", false, false},

		{"file:///tmp/origin.git", false, false},
		{"/tmp/origin.git", false, false},
		{"file:///tmp/origin.git", true, true},
		{"/tmp/origin.git", true, true},
	}
	for _, tt := range tests {
		err := ValidateRepoURL(tt.url, tt.allowFile)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateRepoURL(%q, allowFile=%v) = %v, quería ok=%v", tt.url, tt.allowFile, err, tt.ok)
		}
	}
}
