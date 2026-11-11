// Package secrets es el escáner de secretos que corre antes de commitear un
// snapshot (design.md §8). Combina las reglas por defecto de gitleaks con
// reglas propias de Home Assistant.
//
// Regla de oro: un Finding nunca lleva el valor del secreto. Ni en sus
// campos, ni en Error(), ni en logs.
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

// Severidades.
const (
	Block = "block"
	Warn  = "warn"
)

// Reglas propias.
const (
	RuleHALiteral    = "ha-literal-secret"
	RuleHAWebhookID  = "ha-webhook-id"
	RuleURLWithCreds = "url-with-credentials"
)

// ErrSecretsFound indica que hay hallazgos bloqueantes. El error concreto es
// *BlockedError.
var ErrSecretsFound = errors.New("posibles secretos en los cambios")

// Finding es un hallazgo, sin el valor.
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

// BlockedError lista los hallazgos bloqueantes.
type BlockedError struct {
	Findings []Finding
}

func (e *BlockedError) Error() string {
	var b strings.Builder
	b.WriteString(ErrSecretsFound.Error() + ":")
	for _, f := range e.Findings {
		b.WriteString("\n  " + f.String())
	}
	b.WriteString("\nMové el valor a secrets.yaml (y usá !secret), o agregá el fingerprint a secret_scan.allow si es un falso positivo.")
	return b.String()
}

// Is hace que errors.Is(err, ErrSecretsFound) funcione.
func (e *BlockedError) Is(target error) bool { return target == ErrSecretsFound }

// Scanner es el escáner configurado.
type Scanner struct {
	gl        *detect.Detector
	webhookID string
	allow     map[string]bool
}

// New crea un Scanner.
func New(cfg config.SecretScan) (*Scanner, error) {
	// gitleaks loguea a stderr, y en debug podría incluir contenido: se apaga.
	logging.Logger = zerolog.Nop()
	gl, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, fmt.Errorf("cargando las reglas de gitleaks: %w", err)
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

// Scan devuelve los hallazgos de un archivo (incluidos los permitidos por
// fingerprint, marcados como tales por Filter).
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

	// DetectBytes en vez de Detect(Fragment): Fragment está deprecado hasta
	// v9. Se pierden solo las allowlists por path de gitleaks (lockfiles y
	// similares), que acá no aplican.
	for _, gf := range s.gl.DetectBytes(content) {
		// gitleaks numera las líneas desde 0 en fragmentos sueltos.
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

// Check escanea un conjunto de archivos (path del repo → contenido). Devuelve
// las advertencias y, si hay hallazgos bloqueantes no permitidos, un
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

// urlWithCreds detecta esquema://usuario:clave@host.
var urlWithCreds = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s:/@"']+:[^\s/@"']+@[^\s/"']+`)

// sensitiveKeys son las claves YAML cuyo valor literal es un secreto. Una
// clave matchea si es igual a una de estas o termina en "_" + una de estas
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

// scanYAML aplica las reglas de HA. Si el YAML no parsea, no hace nada (el
// pre-check de sintaxis es otro paso; gitleaks igual corrió).
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

// walk recorre todos los pares clave/valor de los mappings.
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

// literal dice si un valor es un escalar escrito a mano: no un !secret,
// !env_var, !input ni otro tag de HA, no una plantilla, no vacío ni booleano.
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

// fingerprint identifica un hallazgo para permitirlo: cambia si cambia el
// path, la regla o el contenido de la línea.
func fingerprint(repoPath, rule, line string) string {
	h := sha256.Sum256([]byte(repoPath + "\x00" + rule + "\x00" + line))
	return hex.EncodeToString(h[:])
}
