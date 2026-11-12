package config

import (
	"strings"
	"testing"
)

func valid() *Config {
	c := Default()
	c.Repository = "https://github.com/owner/repo.git"
	c.Token = "github_pat_x"
	return c
}

func TestDefaultsAreValid(t *testing.T) {
	if err := valid().Validate(ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestParseKeepsDefaultsAndRejectsUnknownKeys(t *testing.T) {
	c, err := Parse([]byte(`{"repository": "https://example.com/r.git", "token": "t", "root": "config", "merge_options": ["theirs"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MainBranch != "main" || c.MaxFileSizeMB != 10 || !c.CheckConfig || !c.Reload || c.SecretScan.Mode != SecretsWarn {
		t.Errorf("defaults lost: %+v", c)
	}
	if c.RootPath() != "/homeassistant" {
		t.Errorf("root alias = %q", c.RootPath())
	}
	if _, err := Parse([]byte(`{"mode": "push_only"}`)); err == nil {
		t.Error("an unknown key was accepted")
	}
}

func TestValidateReportsEveryError(t *testing.T) {
	c := valid()
	c.Repository = "git@github.com:owner/repo.git"
	c.Token = ""
	c.Root = "homeassistant"
	c.MergeOptions = []string{"theirs", "-s ours"}
	c.SecretScan.Mode = "maybe"
	err := c.Validate(ValidateOptions{})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, field := range []string{"repository:", "token:", "root:", "merge_options[1]:", "secret_scan.mode:"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s in:\n%v", field, err)
		}
	}
	if strings.Contains(err.Error(), "merge_options[0]") {
		t.Errorf("a valid merge option was rejected:\n%v", err)
	}
}

func TestRepoURL(t *testing.T) {
	for _, tc := range []struct {
		url       string
		allowFile bool
		ok        bool
	}{
		{"https://github.com/o/r.git", false, true},
		{"https://gitlab.example.com/group/sub/r", false, true},
		{"https://user:pass@github.com/o/r.git", false, false},
		{"http://github.com/o/r.git", false, false},
		{"ssh://git@github.com/o/r.git", false, false},
		{"file:///tmp/r.git", false, false},
		{"file:///tmp/r.git", true, true},
		{"/tmp/r.git", true, true},
	} {
		if err := ValidateRepoURL(tc.url, tc.allowFile); (err == nil) != tc.ok {
			t.Errorf("%s (allowFile=%v): err = %v", tc.url, tc.allowFile, err)
		}
	}
}

func TestLocalRemoteNeedsNoToken(t *testing.T) {
	c := valid()
	c.Repository, c.Token = "/tmp/remote.git", ""
	if err := c.Validate(ValidateOptions{AllowFileRemote: true}); err != nil {
		t.Fatal(err)
	}
}
