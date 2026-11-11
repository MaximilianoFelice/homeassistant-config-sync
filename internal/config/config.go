// Package config define el esquema de configuración del motor. Es el mismo
// esquema que /data/options.json de la app, así el CLI del v0 y la app del v1
// leen exactamente lo mismo. Ver docs/design.md §13.
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

// Modos de sync (design.md §6).
const (
	ModeBidirectional = "bidirectional"
	ModePushOnly      = "push_only"
	ModePullOnly      = "pull_only"
)

// Políticas de reload y restart (design.md §7).
const (
	ReloadAuto = "auto"
	ReloadNone = "none"

	RestartNever = "never"
	RestartAuto  = "auto"
)

// Severidad de la regla de webhook_id del escáner de secretos (design.md §8).
const (
	SeverityWarn  = "warn"
	SeverityBlock = "block"
)

// Config es la configuración completa del motor.
type Config struct {
	Repository          string     `yaml:"repository"`
	MainBranch          string     `yaml:"main_branch"`
	BoxBranch           string     `yaml:"box_branch"`
	Mode                string     `yaml:"mode"`
	SyncIntervalMinutes int        `yaml:"sync_interval_minutes"`
	Reload              string     `yaml:"reload"`
	Restart             string     `yaml:"restart"`
	CommitAuthor        string     `yaml:"commit_author"`
	MaxFileSizeKB       int        `yaml:"max_file_size_kb"`
	DeleteGuardPercent  int        `yaml:"delete_guard_percent"`
	DeleteGuardFiles    int        `yaml:"delete_guard_files"`
	NotifyService       string     `yaml:"notify_service"`
	SSHKnownHosts       []string   `yaml:"ssh_known_hosts"`
	Mappings            []Mapping  `yaml:"mappings"`
	SecretScan          SecretScan `yaml:"secret_scan"`
}

// Mapping vincula un prefijo del repo con un directorio del box
// (design.md §4) y define qué paths de ese directorio se gestionan (§5).
//
// Las tres listas son de strings (y no de objetos con dirección) para que el
// esquema de opciones del Supervisor pueda validarlas.
type Mapping struct {
	// Repo es el prefijo dentro del repo, sin barras al principio ni al final.
	Repo string `yaml:"repo"`
	// Box es el directorio absoluto en el box.
	Box string `yaml:"box"`
	// Include son los globs que se sincronizan en las dos direcciones.
	Include []string `yaml:"include"`
	// PushOnly son los globs que solo viajan del box al repo.
	PushOnly []string `yaml:"push_only"`
	// Exclude saca paths de Include y de PushOnly.
	Exclude []string `yaml:"exclude"`
}

// SecretScan configura el escáner de secretos (design.md §8).
type SecretScan struct {
	WebhookID string   `yaml:"webhook_id"`
	Allow     []string `yaml:"allow"`
}

// Default devuelve la configuración por defecto. Repository queda vacío
// porque no tiene un default razonable.
func Default() *Config {
	return &Config{
		MainBranch:          "main",
		BoxBranch:           "box",
		Mode:                ModeBidirectional,
		SyncIntervalMinutes: 5,
		Reload:              ReloadAuto,
		Restart:             RestartNever,
		CommitAuthor:        "Home Assistant <ha@box.local>",
		MaxFileSizeKB:       1024,
		DeleteGuardPercent:  20,
		DeleteGuardFiles:    25,
		Mappings:            DefaultMappings(),
		SecretScan:          SecretScan{WebhookID: SeverityWarn},
	}
}

// DefaultMappings devuelve los mapeos por defecto: config/ → /homeassistant
// con la whitelist segura, y app_configs/ → /addon_configs vacío (hay que
// elegir apps explícitamente).
func DefaultMappings() []Mapping {
	return []Mapping{
		{
			Repo: "config",
			Box:  "/homeassistant",
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
			Box:      "/addon_configs",
			Include:  []string{},
			PushOnly: []string{},
			Exclude:  []string{},
		},
	}
}

// Load lee la configuración desde un archivo YAML o JSON (JSON es YAML
// válido, así que options.json del Supervisor se lee igual). Los campos
// ausentes conservan su default. Las claves desconocidas son un error, para
// que un typo no pase desapercibido. Los paths Box relativos se resuelven
// contra el directorio del archivo.
//
// Load no valida: eso lo hace Validate.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("leyendo la config: %w", err)
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

// Parse decodifica la configuración sobre los defaults.
func Parse(b []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return cfg, nil
}
