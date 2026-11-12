// Package config defines the engine's configuration schema. It is the same
// schema as the app's /data/options.json, so the v0 CLI and the v1 app read
// exactly the same thing. See docs/design.md §13.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Sync modes (design.md §6).
const (
	ModeBidirectional = "bidirectional"
	ModePushOnly      = "push_only"
	ModePullOnly      = "pull_only"
)

// Reload and restart policies (design.md §7).
const (
	ReloadAuto = "auto"
	ReloadNone = "none"

	RestartNever = "never"
	RestartAuto  = "auto"
)

// What to do with check_config warnings (design.md §7). HA reports an
// integration with an invalid config as a warning: it still starts, but
// without that integration.
const (
	// WarningsNew rejects if the change adds warnings that were not there before.
	WarningsNew = "new"
	// WarningsAny rejects on any warning.
	WarningsAny = "any"
	// WarningsIgnore only looks at errors.
	WarningsIgnore = "ignore"
)

// Severity of the secret scanner's webhook_id rule (design.md §8).
const (
	SeverityWarn  = "warn"
	SeverityBlock = "block"
)

// Config is the engine's full configuration.
type Config struct {
	Repository          string     `yaml:"repository"`
	MainBranch          string     `yaml:"main_branch"`
	BoxBranch           string     `yaml:"box_branch"`
	Mode                string     `yaml:"mode"`
	SyncIntervalMinutes int        `yaml:"sync_interval_minutes"`
	Reload              string     `yaml:"reload"`
	Restart             string     `yaml:"restart"`
	CheckWarnings       string     `yaml:"check_warnings"`
	CommitAuthor        string     `yaml:"commit_author"`
	MaxFileSizeKB       int        `yaml:"max_file_size_kb"`
	DeleteGuardPercent  int        `yaml:"delete_guard_percent"`
	DeleteGuardFiles    int        `yaml:"delete_guard_files"`
	NotifyService       string     `yaml:"notify_service"`
	SSHKnownHosts       []string   `yaml:"ssh_known_hosts"`
	Mappings            []Mapping  `yaml:"mappings"`
	SecretScan          SecretScan `yaml:"secret_scan"`
}

// Mapping links a repo prefix to a box directory (design.md §4) and defines
// which paths of that directory are managed (§5).
//
// The three lists are strings (rather than objects with a direction) so the
// Supervisor's options schema can validate them.
type Mapping struct {
	// Repo is the prefix inside the repo, with no leading or trailing slashes.
	Repo string `yaml:"repo"`
	// Box is the absolute directory on the box.
	Box string `yaml:"box"`
	// Include are the globs synced in both directions.
	Include []string `yaml:"include"`
	// PushOnly are the globs that only travel from the box to the repo.
	PushOnly []string `yaml:"push_only"`
	// Exclude removes paths from Include and PushOnly.
	Exclude []string `yaml:"exclude"`
	// Check says whether a change applied in this mapping must pass Home
	// Assistant's check_config (design.md §7).
	Check bool `yaml:"check"`
}

// SecretScan configures the secret scanner (design.md §8).
type SecretScan struct {
	WebhookID string   `yaml:"webhook_id"`
	Allow     []string `yaml:"allow"`
}

// Default returns the default configuration. Repository is left empty
// because it has no sensible default.
func Default() *Config {
	return &Config{
		MainBranch:          "main",
		BoxBranch:           "box",
		Mode:                ModeBidirectional,
		SyncIntervalMinutes: 5,
		Reload:              ReloadAuto,
		Restart:             RestartNever,
		CheckWarnings:       WarningsNew,
		CommitAuthor:        "Home Assistant <ha@box.local>",
		MaxFileSizeKB:       1024,
		DeleteGuardPercent:  20,
		DeleteGuardFiles:    25,
		Mappings:            DefaultMappings(),
		SecretScan:          SecretScan{WebhookID: SeverityWarn},
	}
}

// DefaultMappings returns the default mappings: config/ → /homeassistant
// with the safe whitelist, and app_configs/ → /app_configs empty (apps have
// to be chosen explicitly).
func DefaultMappings() []Mapping {
	return []Mapping{
		{
			Repo:  "config",
			Box:   "/homeassistant",
			Check: true,
			Include: []string{
				"**/*.yaml",
				"**/*.yml",
				"custom_templates/**/*.jinja",
				"python_scripts/**/*.py",
				"pyscript/**/*.py",
			},
			PushOnly: []string{
				".storage/lovelace*",
			},
			Exclude: []string{
				".cache/**",
				"deps/**",
				"tts/**",
				"backups/**",
				"custom_components/**",
				"esphome/.esphome/**",
				"**/__pycache__/**",
				"ip_bans.yaml",
			},
		},
		{
			Repo:     "app_configs",
			Box:      "/app_configs",
			Include:  []string{},
			PushOnly: []string{},
			Exclude:  []string{},
		},
	}
}

// Load reads the configuration from a YAML or JSON file (JSON is valid YAML,
// so the Supervisor's options.json reads the same way). Missing fields keep
// their default. Unknown keys are an error, so a typo does not go unnoticed.
// Relative Box paths are resolved against the file's directory.
//
// Load does not validate: Validate does that.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the config: %w", err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for i := range cfg.Mappings {
		if b := cfg.Mappings[i].Box; b != "" && !filepath.IsAbs(b) {
			cfg.Mappings[i].Box = filepath.Join(dir, b)
		}
	}
	return cfg, nil
}

// Parse decodes the configuration on top of the defaults.
func Parse(b []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return cfg, nil
}
