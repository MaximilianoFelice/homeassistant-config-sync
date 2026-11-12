// Package secrets is the secret scanner (docs/redesign.md §9): gitleaks with
// its standard configuration. The rules come from the repo's .gitleaks.toml
// (gitleaks' defaults when there is none), false positives are silenced with
// the repo's .gitleaksignore or a gitleaks:allow comment, exactly as with the
// gitleaks CLI.
//
// Golden rule: a Finding never carries the secret's value. Not in its
// fields, not in Error(), not in logs.
package secrets

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/spf13/viper"
	glconfig "github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
)

// File names gitleaks looks for at the root of what it scans.
const (
	ConfigFile = ".gitleaks.toml"
	IgnoreFile = ".gitleaksignore"
)

// Finding is a finding, without the value. Fingerprint has gitleaks' format
// for files ("path:rule:line"), so it can be pasted into .gitleaksignore.
type Finding struct {
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Rule        string `json:"rule"`
	Fingerprint string `json:"fingerprint"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s [%s]", f.Path, f.Line, f.Rule, f.Fingerprint)
}

// Scanner is a gitleaks detector plus the repo's ignore list.
type Scanner struct {
	det    *detect.Detector
	ignore map[string]bool
}

// gitleaks' config loader keeps package-level state; loading is serialized.
var loadMu sync.Mutex

var commitPrefix = regexp.MustCompile(`^[0-9a-f]{40}:`)

// New builds a scanner from the repo's .gitleaks.toml (nil: gitleaks'
// defaults) and .gitleaksignore (nil: none).
func New(configTOML, ignore []byte) (*Scanner, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	// gitleaks logs to stderr, and in debug it could include content.
	logging.Logger = zerolog.Nop()

	def, err := load([]byte(glconfig.DefaultConfig))
	if err != nil {
		return nil, fmt.Errorf("loading gitleaks' default rules: %w", err)
	}
	cfg := def
	if len(bytes.TrimSpace(configTOML)) > 0 {
		if cfg, err = loadRepoConfig(configTOML, def); err != nil {
			return nil, fmt.Errorf("%s: %w", ConfigFile, err)
		}
	}
	s := &Scanner{det: detect.NewDetector(cfg), ignore: map[string]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(ignore))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Fingerprints from a git scan carry the commit first; what is
		// scanned here is the tree, so only path:rule:line matters.
		s.ignore[commitPrefix.ReplaceAllString(line, "")] = true
	}
	return s, nil
}

func load(toml []byte) (glconfig.Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(toml)); err != nil {
		return glconfig.Config{}, err
	}
	var vc glconfig.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return glconfig.Config{}, err
	}
	return vc.Translate()
}

// loadRepoConfig loads the repo's config. [extend] useDefault is applied
// here instead of by gitleaks: its loader counts extensions in a
// package-level variable that never resets, so a long-running process that
// reloads the config would silently stop extending after the second load.
func loadRepoConfig(toml []byte, def glconfig.Config) (glconfig.Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(bytes.NewReader(toml)); err != nil {
		return glconfig.Config{}, err
	}
	var vc glconfig.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return glconfig.Config{}, err
	}
	if vc.Extend.Path != "" || vc.Extend.URL != "" {
		return glconfig.Config{}, errors.New("[extend] path and url are not supported; use useDefault = true")
	}
	useDefault, disabled := vc.Extend.UseDefault, vc.Extend.DisabledRules
	vc.Extend = glconfig.Extend{}
	cfg, err := vc.Translate()
	if err != nil {
		return glconfig.Config{}, err
	}
	if !useDefault {
		return cfg, nil
	}
	off := map[string]bool{}
	for _, id := range disabled {
		off[id] = true
	}
	if cfg.Rules == nil {
		cfg.Rules = map[string]glconfig.Rule{}
	}
	if cfg.Keywords == nil {
		cfg.Keywords = map[string]struct{}{}
	}
	// A rule defined in the repo replaces the default rule with its ID.
	for id, r := range def.Rules {
		if _, mine := cfg.Rules[id]; mine || off[id] {
			continue
		}
		cfg.Rules[id] = r
		cfg.OrderedRules = append(cfg.OrderedRules, id)
		for _, k := range r.Keywords {
			cfg.Keywords[k] = struct{}{}
		}
	}
	cfg.Allowlists = append(cfg.Allowlists, def.Allowlists...)
	return cfg, nil
}

// Scan returns a file's findings that are not ignored.
func (s *Scanner) Scan(path string, content []byte) []Finding {
	seen := map[string]bool{}
	var out []Finding
	//nolint:staticcheck // Fragment is the only way to pass the file path, which per-path allowlists need.
	for _, gf := range s.det.Detect(detect.Fragment{Raw: string(content), FilePath: path}) {
		// gitleaks numbers lines from 0 in standalone fragments; its CLI
		// reports them from 1.
		line := gf.StartLine + 1
		fp := fmt.Sprintf("%s:%s:%d", path, gf.RuleID, line)
		if seen[fp] || s.ignore[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, Finding{Path: path, Line: line, Rule: gf.RuleID, Fingerprint: fp})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Line != out[b].Line {
			return out[a].Line < out[b].Line
		}
		return out[a].Rule < out[b].Rule
	})
	return out
}
