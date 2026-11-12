// Package config defines the engine's configuration schema. It is the same
// schema as the app's /data/options.json, so the CLI and the app read exactly
// the same thing. See docs/redesign.md §11.
//
// Only how to reach the repo and how the box behaves live here. What is
// synced (.gitignore), how merges resolve per path (.gitattributes) and the
// secret scanner's rules (.gitleaks.toml) live in the repo itself.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Secret scanner modes (docs/redesign.md §9).
const (
	SecretsWarn  = "warn"
	SecretsBlock = "block"
	SecretsOff   = "off"
)

// HomeAssistantDir is where the Supervisor mounts Home Assistant's
// configuration inside an app. "config" is accepted as an alias for it in
// the root option, like Terminal & SSH's ~/config link.
const HomeAssistantDir = "/homeassistant"

// Config is the engine's full configuration.
type Config struct {
	// Repository is the repo's HTTPS URL.
	Repository string `yaml:"repository"`
	// Token is the access token git authenticates with.
	Token string `yaml:"token"`
	// Username is sent with the token for hosts that need one. Empty: a
	// placeholder is sent (GitHub and most hosts ignore it).
	Username   string `yaml:"username"`
	MainBranch string `yaml:"main_branch"`
	BoxBranch  string `yaml:"box_branch"`
	// Root is the folder synced, as an absolute path in the app's
	// filesystem. "/" syncs every mount.
	Root                string   `yaml:"root"`
	SyncIntervalMinutes int      `yaml:"sync_interval_minutes"`
	MergeOptions        []string `yaml:"merge_options"`
	// CheckConfig asks Home Assistant to check its config after applying a
	// change to it, only to inform.
	CheckConfig bool `yaml:"check_config"`
	// Reload asks Home Assistant to reload after applying a change to it.
	Reload             bool       `yaml:"reload"`
	MaxFileSizeMB      int        `yaml:"max_file_size_mb"`
	DeleteGuardFiles   int        `yaml:"delete_guard_files"`
	DeleteGuardPercent int        `yaml:"delete_guard_percent"`
	CommitAuthor       string     `yaml:"commit_author"`
	NotifyService      string     `yaml:"notify_service"`
	SecretScan         SecretScan `yaml:"secret_scan"`
}

// SecretScan configures the secret scanner.
type SecretScan struct {
	Mode string `yaml:"mode"`
}

// Default returns the default configuration. Repository and Token are left
// empty because they have no sensible default.
func Default() *Config {
	return &Config{
		MainBranch:          "main",
		BoxBranch:           "box",
		Root:                "/",
		SyncIntervalMinutes: 5,
		MergeOptions:        []string{},
		CheckConfig:         true,
		Reload:              true,
		MaxFileSizeMB:       10,
		DeleteGuardFiles:    25,
		DeleteGuardPercent:  20,
		CommitAuthor:        "Home Assistant <ha@box.local>",
		SecretScan:          SecretScan{Mode: SecretsWarn},
	}
}

// Load reads the configuration from a YAML or JSON file (JSON is valid YAML,
// so the Supervisor's options.json reads the same way). Missing fields keep
// their default. Unknown keys are an error, so a typo does not go unnoticed.
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

// RootPath returns Root with the "config" alias resolved and cleaned.
func (c *Config) RootPath() string {
	return CleanRoot(c.Root)
}
