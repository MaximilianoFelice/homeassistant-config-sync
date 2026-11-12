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
		t.Fatalf("the default config should be valid: %v", err)
	}
	if err := Default().Validate(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("without repository it should fail on repository, got %v", err)
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
		t.Errorf("sync_interval_minutes = %d, want 0", c.SyncIntervalMinutes)
	}
	if c.MainBranch != "main" || c.Mode != ModeBidirectional || len(c.Mappings) != 2 {
		t.Errorf("defaults were lost: %+v", c)
	}
	if c.SecretScan.WebhookID != SeverityWarn {
		t.Errorf("a partial object overwrote the webhook_id default: %q", c.SecretScan.WebhookID)
	}

	if _, err := Parse([]byte("repositry: x\n")); err == nil {
		t.Error("an unknown key should be an error")
	}
	if c, err := Parse(nil); err != nil || c.MainBranch != "main" {
		t.Errorf("an empty file should give the defaults: %v %+v", err, c)
	}
}

func TestParseReadsSupervisorJSON(t *testing.T) {
	c, err := Parse([]byte(`{"repository": "git@github.com:o/r.git", "mode": "push_only",
		"mappings": [{"repo": "config", "box": "/homeassistant", "include": ["*.yaml"], "push_only": [], "exclude": []}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModePushOnly || len(c.Mappings) != 1 || c.Mappings[0].Include[0] != "*.yaml" {
		t.Errorf("JSON read incorrectly: %+v", c)
	}
	if err := c.Validate(ValidateOptions{}); err != nil {
		t.Errorf("should be valid: %v", err)
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
		t.Errorf("box = %q, want %q", c.Mappings[0].Box, want)
	}
	if err := c.Validate(ValidateOptions{AllowFileRemote: true}); err != nil {
		t.Errorf("should be valid with AllowFileRemote: %v", err)
	}
}

func TestValidateReportsEveryError(t *testing.T) {
	c := validConfig()
	c.MainBranch = "box"
	c.Mode = "both"
	c.MaxFileSizeKB = 0
	c.DeleteGuardPercent = 0
	c.CommitAuthor = "no email"
	c.SecretScan.Allow = []string{"nope"}
	c.NotifyService = "notify/../../evil"
	err := c.Validate(ValidateOptions{})
	if err == nil {
		t.Fatal("want errors")
	}
	for _, field := range []string{"box_branch", "mode", "max_file_size_kb", "delete_guard_percent", "commit_author", "secret_scan.allow[0]", "notify_service"} {
		if !strings.Contains(err.Error(), field+":") {
			t.Errorf("missing the %s error in:\n%v", field, err)
		}
	}
}

func TestValidateMappings(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty = valid
	}{
		{"defaults", func(*Config) {}, ""},
		{"no mappings", func(c *Config) { c.Mappings = nil }, "at least one mapping"},
		{"absolute repo", func(c *Config) { c.Mappings[0].Repo = "/config" }, "mappings[0].repo"},
		{"repo with ..", func(c *Config) { c.Mappings[0].Repo = "../config" }, "mappings[0].repo"},
		{"unclean repo", func(c *Config) { c.Mappings[0].Repo = "config/" }, "mappings[0].repo"},
		{"repo .git", func(c *Config) { c.Mappings[0].Repo = ".git/x" }, "mappings[0].repo"},
		{"nested repo", func(c *Config) { c.Mappings[1].Repo = "config/apps" }, "overlaps"},
		{"relative box", func(c *Config) { c.Mappings[0].Box = "homeassistant" }, "mappings[0].box"},
		{"root box", func(c *Config) { c.Mappings[0].Box = "/" }, "mappings[0].box"},
		{"nested box", func(c *Config) { c.Mappings[1].Box = "/homeassistant/apps" }, "overlaps"},
		{"sibling box with common prefix", func(c *Config) { c.Mappings[1].Box = "/homeassistant2" }, ""},
		{"invalid glob", func(c *Config) { c.Mappings[0].Include = []string{"[a"} }, "include[0]"},
		{"absolute glob", func(c *Config) { c.Mappings[0].Exclude = []string{"/x"} }, "exclude[0]"},
		{"empty glob", func(c *Config) { c.Mappings[0].PushOnly = []string{""} }, "push_only[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.Mappings = DefaultMappings()
			tt.mutate(c)
			err := c.Validate(ValidateOptions{})
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("want no error, got %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("want an error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"main", "box", "box/rpi4", "release-1.0"} {
		if err := validateBranch(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-main", "a..b", "a b", "a:b", "x.lock", "a/", "/a", "a//b", "a@{1}", "a~1", "a^", "a*", "a\x01"} {
		if err := validateBranch(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
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
			t.Errorf("ValidateRepoURL(%q, allowFile=%v) = %v, want ok=%v", tt.url, tt.allowFile, err, tt.ok)
		}
	}
}
