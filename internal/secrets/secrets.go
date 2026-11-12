// Package secrets is the secret scanner that runs before committing a
// snapshot (design.md §8). It combines gitleaks' default rules with
// Home Assistant-specific rules.
//
// Golden rule: a Finding never carries the secret's value. Not in its
// fields, not in Error(), not in logs.
package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/rs/zerolog"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	"go.yaml.in/yaml/v3"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
)

// Severities.
const (
	Block = "block"
	Warn  = "warn"
)

// Custom rules.
const (
	RuleHALiteral    = "ha-literal-secret"
	RuleHAWebhookID  = "ha-webhook-id"
	RuleURLWithCreds = "url-with-credentials"
)

// ErrSecretsFound means there are blocking findings. The concrete error is
// *BlockedError.
var ErrSecretsFound = errors.New("possible secrets in the changes")

// Finding is a finding, without the value.
type Finding struct {
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Rule        string `json:"rule"`
	Severity    string `json:"severity"`
	Fingerprint string `json:"fingerprint"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s (%s) [%s]", f.Path, f.Line, f.Rule, f.Severity, f.Fingerprint)
}

// BlockedError lists the blocking findings.
type BlockedError struct {
	Findings []Finding
}

func (e *BlockedError) Error() string {
	var b strings.Builder
	b.WriteString(ErrSecretsFound.Error() + ":")
	for _, f := range e.Findings {
		b.WriteString("\n  " + f.String())
	}
	b.WriteString("\nMove the value to secrets.yaml (and use !secret), or add the fingerprint to secret_scan.allow if it is a false positive.")
	return b.String()
}

// Is makes errors.Is(err, ErrSecretsFound) work.
func (e *BlockedError) Is(target error) bool { return target == ErrSecretsFound }

// Scanner is the configured scanner.
type Scanner struct {
	gl        *detect.Detector
	webhookID string
	allow     map[string]bool
}

// New creates a Scanner.
func New(cfg config.SecretScan) (*Scanner, error) {
	// gitleaks logs to stderr, and in debug it could include content: turn it off.
	logging.Logger = zerolog.Nop()
	gl, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the gitleaks rules: %w", err)
	}
	s := &Scanner{gl: gl, webhookID: cfg.WebhookID, allow: map[string]bool{}}
	if s.webhookID == "" {
		s.webhookID = Warn
	}
	for _, fp := range cfg.Allow {
		s.allow[fp] = true
	}
	return s, nil
}

// Scan returns a file's findings (including those allowed by fingerprint,
// which Check filters out).
func (s *Scanner) Scan(repoPath string, content []byte) []Finding {
	lines := bytes.Split(content, []byte{'\n'})
	seen := map[string]bool{}
	var out []Finding
	add := func(line int, rule, severity string) {
		f := Finding{Path: repoPath, Line: line, Rule: rule, Severity: severity, Fingerprint: fingerprint(repoPath, rule, lineText(lines, line))}
		key := fmt.Sprintf("%d\x00%s", f.Line, f.Rule)
		if !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
	}

	// DetectBytes instead of Detect(Fragment): Fragment is deprecated until
	// v9. Only gitleaks' per-path allowlists (lockfiles and the like) are
	// lost, and they do not apply here.
	for _, gf := range s.gl.DetectBytes(content) {
		// gitleaks numbers lines from 0 in standalone fragments.
		add(gf.StartLine+1, gf.RuleID, Block)
	}
	for i, l := range lines {
		if urlWithCreds.Match(l) {
			add(i+1, RuleURLWithCreds, Block)
		}
	}
	switch strings.ToLower(path.Ext(repoPath)) {
	case ".yaml", ".yml":
		s.scanYAML(content, add)
	}

	sort.Slice(out, func(a, b int) bool {
		if out[a].Line != out[b].Line {
			return out[a].Line < out[b].Line
		}
		return out[a].Rule < out[b].Rule
	})
	return out
}

// Check scans a set of files (repo path → content). It returns the warnings
// and, if there are blocking findings that are not allowed, a
// *BlockedError.
func (s *Scanner) Check(contents map[string][]byte) (warnings []Finding, err error) {
	paths := make([]string, 0, len(contents))
	for p := range contents {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var blocked []Finding
	for _, p := range paths {
		for _, f := range s.Scan(p, contents[p]) {
			switch {
			case s.allow[f.Fingerprint]:
				continue
			case f.Severity == Block:
				blocked = append(blocked, f)
			default:
				warnings = append(warnings, f)
			}
		}
	}
	if len(blocked) > 0 {
		return warnings, &BlockedError{Findings: blocked}
	}
	return warnings, nil
}

// urlWithCreds detects scheme://user:password@host.
var urlWithCreds = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s:/@"']+:[^\s/@"']+@[^\s/"']+`)

// sensitiveKeys are the YAML keys whose literal value is a secret. A key
// matches if it equals one of these or ends in "_" + one of these
// (api_password, refresh_token, client_secret…).
var sensitiveKeys = []string{
	"password", "passwd", "pass", "token", "api_key", "apikey", "secret",
	"private_key", "privkey", "bearer", "auth_key", "access_key", "secret_key",
}

func sensitive(key string) bool {
	k := strings.ToLower(key)
	for _, w := range sensitiveKeys {
		if k == w || strings.HasSuffix(k, "_"+w) {
			return true
		}
	}
	return false
}

// scanYAML applies the HA rules. If the YAML does not parse, it does nothing
// (the syntax pre-check is a separate step; gitleaks ran anyway).
func (s *Scanner) scanYAML(content []byte, add func(line int, rule, severity string)) {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			return
		}
		walk(&doc, func(k, v *yaml.Node) {
			if !literal(v) {
				return
			}
			switch {
			case strings.EqualFold(k.Value, "webhook_id"):
				add(v.Line, RuleHAWebhookID, s.webhookID)
			case sensitive(k.Value):
				add(v.Line, RuleHALiteral, Block)
			}
		})
	}
}

// walk visits every key/value pair of the mappings.
func walk(n *yaml.Node, fn func(k, v *yaml.Node)) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walk(c, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			fn(n.Content[i], n.Content[i+1])
			walk(n.Content[i+1], fn)
		}
	}
}

// literal reports whether a value is a hand-written scalar: not a !secret,
// !env_var, !input or other HA tag, not a template, not empty or boolean.
func literal(v *yaml.Node) bool {
	if v.Kind != yaml.ScalarNode {
		return false
	}
	switch v.Tag {
	case "!!str", "!!int", "!!float", "":
	default:
		return false // !secret, !env_var, !input, !!bool, !!null…
	}
	val := strings.TrimSpace(v.Value)
	if val == "" || strings.Contains(val, "{{") || strings.Contains(val, "{%") {
		return false
	}
	return true
}

func lineText(lines [][]byte, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimSpace(string(lines[line-1]))
}

// fingerprint identifies a finding so it can be allowed: it changes if the
// path, the rule or the line content changes.
func fingerprint(repoPath, rule, line string) string {
	h := sha256.Sum256([]byte(repoPath + "\x00" + rule + "\x00" + line))
	return hex.EncodeToString(h[:])
}
