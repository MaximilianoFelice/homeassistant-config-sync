package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// ValidateOptions adjusts validation to the execution context.
type ValidateOptions struct {
	// AllowFileRemote enables file:// remotes and local paths (tests and the
	// local sandbox).
	AllowFileRemote bool
}

var (
	authorRe      = regexp.MustCompile(`^[^<>\n]+ <[^<>@\s]+@[^<>\s]+>$`)
	serviceRe     = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
	mergeOptionRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(=[A-Za-z0-9_.-]+)?$`)
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
	if c.Token == "" && !isLocalRemote(c.Repository) {
		add("token", "is empty: create an access token with read and write access to the repo's contents")
	}
	if strings.ContainsAny(c.Token, "\n\r") || strings.ContainsAny(c.Username, "\n\r") {
		add("token", "cannot contain line breaks")
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
	if r := c.Root; r != "config" && (!strings.HasPrefix(r, "/") || path.Clean(r) != r || strings.Contains(r, "\\")) {
		add("root", "%q must be a clean absolute path, such as / or /homeassistant", r)
	}
	if c.SyncIntervalMinutes < 0 {
		add("sync_interval_minutes", "cannot be negative (0 = manual only)")
	}
	for i, o := range c.MergeOptions {
		if !mergeOptionRe.MatchString(o) {
			add(fmt.Sprintf("merge_options[%d]", i), "%q is not a git merge option (passed as -X<option>), such as ours, theirs or diff-algorithm=histogram", o)
		}
	}
	if c.MaxFileSizeMB < 1 {
		add("max_file_size_mb", "must be at least 1")
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
	if !slices.Contains([]string{SecretsWarn, SecretsBlock, SecretsOff}, c.SecretScan.Mode) {
		add("secret_scan.mode", "%q is not valid; options: warn, block, off", c.SecretScan.Mode)
	}
	return errors.Join(errs...)
}

// CleanRoot resolves the "config" alias and cleans a root path.
func CleanRoot(r string) string {
	if r == "config" || r == "/config" {
		return HomeAssistantDir
	}
	if r == "" {
		return "/"
	}
	return path.Clean(r)
}

// ValidateRepoURL accepts https:// URLs (and, for tests, file:// and local
// paths). Credentials never go in the URL: they go in token.
func ValidateRepoURL(raw string, allowFile bool) error {
	if raw == "" {
		return errors.New("is empty: use the repo's HTTPS URL, e.g. https://github.com/owner/repo.git")
	}
	if allowFile && isLocalRemote(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.Path == "/" {
		return fmt.Errorf("%q is not an HTTPS repo URL, e.g. https://github.com/owner/repo.git", raw)
	}
	if u.User != nil {
		return errors.New("must not contain credentials: put the token in the token option")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return nil
}

func isLocalRemote(raw string) bool {
	return strings.HasPrefix(raw, "file://") || strings.HasPrefix(raw, "/")
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
