package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// scpLike matches git's short SSH syntax: [user@]host:path, with no slash
// before the first ':'.
var scpLike = regexp.MustCompile(`^(?:([^@/:\s]+)@)?([^@/:\s]+):([^\s].*)$`)

// ValidateRepoURL accepts only SSH remotes (design.md §9): git@host:owner/repo
// or ssh://[user@]host[:port]/path. It rejects HTTP(S), git://, embedded
// credentials, remote-helper transports (<transport>::<address>) and anything
// git could interpret as an option.
//
// With allowFile it also accepts file:// and absolute paths (v0 and tests).
func ValidateRepoURL(raw string, allowFile bool) error {
	if raw == "" {
		return errors.New("is empty")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\n\r\t") {
		return errors.New("contains whitespace or control characters")
	}
	if strings.HasPrefix(raw, "-") {
		return errors.New("cannot start with '-'")
	}
	if strings.Contains(raw, "::") {
		return errors.New("<transport>::<address> transports are not allowed")
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("not a valid URL: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh":
			if _, hasPass := u.User.Password(); hasPass {
				return errors.New("cannot contain an embedded password: use the deploy key")
			}
			if u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") {
				return errors.New("invalid host")
			}
			if u.Path == "" || u.Path == "/" {
				return errors.New("missing the repo path")
			}
			return nil
		case "file":
			if !allowFile {
				return errors.New("file:// is only allowed with --allow-file-remote")
			}
			return nil
		case "http", "https":
			return errors.New("HTTP(S) is not allowed: use SSH with a deploy key (git@host:owner/repo.git)")
		default:
			return fmt.Errorf("protocol %q not allowed: SSH only", u.Scheme)
		}
	}

	if filepath.IsAbs(raw) {
		if !allowFile {
			return errors.New("local paths are only allowed with --allow-file-remote")
		}
		return nil
	}

	m := scpLike.FindStringSubmatch(raw)
	if m == nil {
		return errors.New("unrecognized format: expected git@host:owner/repo.git or ssh://host/path")
	}
	if strings.HasPrefix(m[2], "-") {
		return errors.New("invalid host")
	}
	return nil
}
