package secrets

import (
	"strings"
	"testing"
)

// A GitHub token-shaped value that gitleaks' default rules catch. It is not
// a real token.
const fakeToken = "ghp_" + "0123456789abcdefghijABCDEFGHIJ012345"

func TestDefaultRulesFindTokensWithoutTheValue(t *testing.T) {
	s, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Scan("homeassistant/configuration.yaml", []byte("a: 1\ntoken: "+fakeToken+"\n"))
	if len(got) != 1 || got[0].Line != 2 || got[0].Path != "homeassistant/configuration.yaml" {
		t.Fatalf("findings = %+v", got)
	}
	if got[0].Fingerprint != "homeassistant/configuration.yaml:"+got[0].Rule+":2" {
		t.Errorf("fingerprint = %q, want gitleaks' path:rule:line", got[0].Fingerprint)
	}
	if strings.Contains(got[0].String(), fakeToken) {
		t.Error("a finding carries the value")
	}
}

func TestInlineAllowAndIgnoreFile(t *testing.T) {
	content := []byte("token: " + fakeToken + " # gitleaks:allow\nother: " + fakeToken + "\n")
	s, _ := New(nil, nil)
	got := s.Scan("a.yaml", content)
	if len(got) != 1 || got[0].Line != 2 {
		t.Fatalf("gitleaks:allow not honored: %+v", got)
	}
	// .gitleaksignore, in both the file format and the git (commit:) format.
	for _, line := range []string{got[0].Fingerprint, strings.Repeat("a", 40) + ":" + got[0].Fingerprint} {
		s, err := New(nil, []byte("# comment\n"+line+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if f := s.Scan("a.yaml", content); len(f) != 0 {
			t.Errorf("ignore line %q not honored: %+v", line, f)
		}
	}
}

func TestRepoConfigExtendsDefaultsAndAddsRules(t *testing.T) {
	cfg := []byte(`
[extend]
useDefault = true

[[rules]]
id = "ha-literal-password"
description = "Literal password instead of !secret"
regex = '''(?i)^\s*password:\s*['"]?([^!\s'"][^\s'"]{3,})'''
secretGroup = 1
keywords = ["password"]
`)
	for range 3 { // gitleaks' own loader stops extending after two loads
		s, err := New(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Scan("x.yaml", []byte("password: hunter22\npassword: !secret pw\ntoken: "+fakeToken+"\n"))
		rules := map[string]bool{}
		for _, f := range got {
			rules[f.Rule] = true
		}
		if !rules["ha-literal-password"] || len(got) != 2 {
			t.Fatalf("findings = %+v (want the custom rule on line 1 and a default rule on line 3)", got)
		}
	}
}

func TestRepoConfigWithoutDefaults(t *testing.T) {
	s, err := New([]byte("[[rules]]\nid = \"only\"\nregex = 'NEVERMATCHES'\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Scan("a.yaml", []byte("token: "+fakeToken+"\n")); len(got) != 0 {
		t.Errorf("without useDefault the default rules should not apply: %+v", got)
	}
}

func TestInvalidRepoConfig(t *testing.T) {
	if _, err := New([]byte("[extend]\npath = '/etc/x.toml'\n"), nil); err == nil {
		t.Error("extend.path should be rejected")
	}
	if _, err := New([]byte("not toml ["), nil); err == nil {
		t.Error("invalid TOML should be an error")
	}
}
