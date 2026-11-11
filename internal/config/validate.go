package config

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ValidateOptions ajusta la validación según el contexto de ejecución.
type ValidateOptions struct {
	// AllowFileRemote habilita remotos file:// y paths locales (v0 y tests).
	AllowFileRemote bool
}

var (
	authorRe      = regexp.MustCompile(`^[^<>\n]+ <[^<>@\s]+@[^<>\s]+>$`)
	fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate devuelve todos los errores de la configuración juntos, cada uno
// con el nombre del campo, para que se puedan corregir de una vez.
func (c *Config) Validate(opts ValidateOptions) error {
	var errs []error
	add := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if err := ValidateRepoURL(c.Repository, opts.AllowFileRemote); err != nil {
		add("repository", "%v", err)
	}
	if err := validateBranch(c.MainBranch); err != nil {
		add("main_branch", "%v", err)
	}
	if err := validateBranch(c.BoxBranch); err != nil {
		add("box_branch", "%v", err)
	}
	if c.MainBranch == c.BoxBranch {
		add("box_branch", "no puede ser igual a main_branch")
	}
	oneOf(add, "mode", c.Mode, ModeBidirectional, ModePushOnly, ModePullOnly)
	oneOf(add, "reload", c.Reload, ReloadAuto, ReloadNone)
	oneOf(add, "restart", c.Restart, RestartNever, RestartAuto)
	oneOf(add, "secret_scan.webhook_id", c.SecretScan.WebhookID, SeverityWarn, SeverityBlock)

	if c.SyncIntervalMinutes < 0 {
		add("sync_interval_minutes", "no puede ser negativo (0 = solo manual)")
	}
	if c.MaxFileSizeKB <= 0 {
		add("max_file_size_kb", "tiene que ser mayor que 0")
	}
	if c.DeleteGuardPercent < 1 || c.DeleteGuardPercent > 100 {
		add("delete_guard_percent", "tiene que estar entre 1 y 100")
	}
	if c.DeleteGuardFiles < 1 {
		add("delete_guard_files", "tiene que ser al menos 1")
	}
	if !authorRe.MatchString(c.CommitAuthor) {
		add("commit_author", "tiene que tener la forma 'Nombre <email>'")
	}
	for i, fp := range c.SecretScan.Allow {
		if !fingerprintRe.MatchString(fp) {
			add(fmt.Sprintf("secret_scan.allow[%d]", i), "no es un fingerprint sha256 en hex")
		}
	}
	for i, kh := range c.SSHKnownHosts {
		if len(strings.Fields(kh)) < 3 {
			add(fmt.Sprintf("ssh_known_hosts[%d]", i), "no parece una línea de known_hosts ('host tipo clave')")
		}
	}

	errs = append(errs, validateMappings(c.Mappings)...)
	return errors.Join(errs...)
}

func oneOf(add func(string, string, ...any), field, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		add(field, "%q no es válido; opciones: %s", v, strings.Join(allowed, ", "))
	}
}

// validateBranch aplica un subconjunto conservador de git check-ref-format.
func validateBranch(b string) error {
	switch {
	case b == "":
		return errors.New("está vacío")
	case strings.HasPrefix(b, "-"), strings.HasPrefix(b, "/"), strings.HasSuffix(b, "/"),
		strings.HasSuffix(b, "."), strings.HasSuffix(b, ".lock"),
		strings.Contains(b, ".."), strings.Contains(b, "//"), strings.Contains(b, "@{"),
		strings.ContainsAny(b, " ~^:?*[\\\x7f"):
		return fmt.Errorf("%q no es un nombre de rama válido", b)
	}
	for _, r := range b {
		if r < 0x20 {
			return fmt.Errorf("%q no es un nombre de rama válido", b)
		}
	}
	return nil
}

func validateMappings(ms []Mapping) []error {
	var errs []error
	add := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if len(ms) == 0 {
		add("mappings", "hace falta al menos un mapeo")
	}
	for i, m := range ms {
		f := fmt.Sprintf("mappings[%d]", i)

		if m.Repo == "" || m.Repo != path.Clean(m.Repo) || m.Repo == "." ||
			strings.HasPrefix(m.Repo, "/") || m.Repo == ".." || strings.HasPrefix(m.Repo, "../") ||
			strings.Contains(m.Repo, "\\") || slices.Contains(strings.Split(m.Repo, "/"), ".git") {
			add(f+".repo", "%q tiene que ser un path relativo limpio dentro del repo (p. ej. 'config')", m.Repo)
		}
		if !filepath.IsAbs(m.Box) || m.Box != filepath.Clean(m.Box) || m.Box == "/" {
			add(f+".box", "%q tiene que ser un directorio absoluto y limpio, distinto de '/'", m.Box)
		}

		for j, g := range m.Include {
			validateGlob(add, fmt.Sprintf("%s.include[%d]", f, j), g)
		}
		for j, g := range m.PushOnly {
			validateGlob(add, fmt.Sprintf("%s.push_only[%d]", f, j), g)
		}
		for j, g := range m.Exclude {
			validateGlob(add, fmt.Sprintf("%s.exclude[%d]", f, j), g)
		}

		// Dos mapeos no pueden solaparse, ni del lado del repo ni del box:
		// un mismo archivo tendría dos dueños.
		for k := range i {
			o := ms[k]
			if nested(m.Repo, o.Repo, "/") {
				add(f+".repo", "%q se solapa con mappings[%d].repo %q", m.Repo, k, o.Repo)
			}
			if nested(m.Box, o.Box, string(filepath.Separator)) {
				add(f+".box", "%q se solapa con mappings[%d].box %q", m.Box, k, o.Box)
			}
		}
	}
	return errs
}

func validateGlob(add func(string, string, ...any), field, g string) {
	switch {
	case g == "":
		add(field, "el patrón está vacío")
	case strings.HasPrefix(g, "/"):
		add(field, "%q: los patrones son relativos a la raíz del mapeo, sin '/' inicial", g)
	case strings.Contains(g, "\\"):
		add(field, "%q: usá '/' como separador", g)
	case !doublestar.ValidatePattern(g):
		add(field, "%q no es un glob válido", g)
	}
}

// nested dice si a y b son iguales o uno está dentro del otro.
func nested(a, b, sep string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}
