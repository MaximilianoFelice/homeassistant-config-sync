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

// ValidateOptions adjusts validation to the execution context.
type ValidateOptions struct {
	// AllowFileRemote enables file:// remotes and local paths (v0 and tests).
	AllowFileRemote bool
}

var (
	authorRe      = regexp.MustCompile(`^[^<>\n]+ <[^<>@\s]+@[^<>\s]+>$`)
	fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	serviceRe     = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
)

// Validate returns every configuration error at once, each with the field
// name, so they can all be fixed in one go.
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
		add("box_branch", "cannot be the same as main_branch")
	}
	oneOf(add, "mode", c.Mode, ModeBidirectional, ModePushOnly, ModePullOnly)
	oneOf(add, "reload", c.Reload, ReloadAuto, ReloadNone)
	oneOf(add, "restart", c.Restart, RestartNever, RestartAuto)
	oneOf(add, "check_warnings", c.CheckWarnings, WarningsNew, WarningsAny, WarningsIgnore)
	oneOf(add, "secret_scan.webhook_id", c.SecretScan.WebhookID, SeverityWarn, SeverityBlock)

	if c.SyncIntervalMinutes < 0 {
		add("sync_interval_minutes", "cannot be negative (0 = manual only)")
	}
	if c.MaxFileSizeKB <= 0 {
		add("max_file_size_kb", "must be greater than 0")
	}
	if c.DeleteGuardPercent < 1 || c.DeleteGuardPercent > 100 {
		add("delete_guard_percent", "must be between 1 and 100")
	}
	if c.DeleteGuardFiles < 1 {
		add("delete_guard_files", "must be at least 1")
	}
	if !authorRe.MatchString(c.CommitAuthor) {
		add("commit_author", "must have the form 'Name <email>'")
	}
	if c.NotifyService != "" && !serviceRe.MatchString(c.NotifyService) {
		add("notify_service", "%q must be a 'domain.service' service, e.g. notify.mobile_app_pixel", c.NotifyService)
	}
	for i, fp := range c.SecretScan.Allow {
		if !fingerprintRe.MatchString(fp) {
			add(fmt.Sprintf("secret_scan.allow[%d]", i), "is not a hex sha256 fingerprint")
		}
	}
	for i, kh := range c.SSHKnownHosts {
		if len(strings.Fields(kh)) < 3 {
			add(fmt.Sprintf("ssh_known_hosts[%d]", i), "does not look like a known_hosts line ('host type key')")
		}
	}

	errs = append(errs, validateMappings(c.Mappings)...)
	return errors.Join(errs...)
}

func oneOf(add func(string, string, ...any), field, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		add(field, "%q is not valid; options: %s", v, strings.Join(allowed, ", "))
	}
}

// validateBranch applies a conservative subset of git check-ref-format.
func validateBranch(b string) error {
	switch {
	case b == "":
		return errors.New("is empty")
	case strings.HasPrefix(b, "-"), strings.HasPrefix(b, "/"), strings.HasSuffix(b, "/"),
		strings.HasSuffix(b, "."), strings.HasSuffix(b, ".lock"),
		strings.Contains(b, ".."), strings.Contains(b, "//"), strings.Contains(b, "@{"),
		strings.ContainsAny(b, " ~^:?*[\\\x7f"):
		return fmt.Errorf("%q is not a valid branch name", b)
	}
	for _, r := range b {
		if r < 0x20 {
			return fmt.Errorf("%q is not a valid branch name", b)
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
		add("mappings", "at least one mapping is required")
	}
	for i, m := range ms {
		f := fmt.Sprintf("mappings[%d]", i)

		if m.Repo == "" || m.Repo != path.Clean(m.Repo) || m.Repo == "." ||
			strings.HasPrefix(m.Repo, "/") || m.Repo == ".." || strings.HasPrefix(m.Repo, "../") ||
			strings.Contains(m.Repo, "\\") || slices.Contains(strings.Split(m.Repo, "/"), ".git") {
			add(f+".repo", "%q must be a clean relative path inside the repo (e.g. 'config')", m.Repo)
		}
		if !filepath.IsAbs(m.Box) || m.Box != filepath.Clean(m.Box) || m.Box == "/" {
			add(f+".box", "%q must be a clean absolute directory other than '/'", m.Box)
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

		// Two mappings cannot overlap, on either the repo or the box side:
		// the same file would have two owners.
		for k := range i {
			o := ms[k]
			if nested(m.Repo, o.Repo, "/") {
				add(f+".repo", "%q overlaps with mappings[%d].repo %q", m.Repo, k, o.Repo)
			}
			if nested(m.Box, o.Box, string(filepath.Separator)) {
				add(f+".box", "%q overlaps with mappings[%d].box %q", m.Box, k, o.Box)
			}
		}
	}
	return errs
}

func validateGlob(add func(string, string, ...any), field, g string) {
	switch {
	case g == "":
		add(field, "the pattern is empty")
	case strings.HasPrefix(g, "/"):
		add(field, "%q: patterns are relative to the mapping root, with no leading '/'", g)
	case strings.Contains(g, "\\"):
		add(field, "%q: use '/' as the separator", g)
	case !doublestar.ValidatePattern(g):
		add(field, "%q is not a valid glob", g)
	}
}

// nested reports whether a and b are equal or one is inside the other.
func nested(a, b, sep string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}
