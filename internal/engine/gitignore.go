package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// baseGitignore is committed on the first sync when the repo has no
// .gitignore (docs/redesign.md §5). Paths are for root /. It only prevents
// accidents: huge files, state that is rewritten all the time, credentials.
// It is a file like any other afterwards.
const baseGitignore = `# Written by Git Config Sync on the first sync. Edit it like any other file:
# it decides what is synced.

# State Home Assistant rewrites all the time; includes login tokens
homeassistant/.storage/
# Nabu Casa credentials
homeassistant/.cloud/

# Secrets: remove this line to version them (they stay in history for good)
secrets.yaml
# Certificate private keys
ssl/

# Databases and logs, in any folder (recorder, Zigbee2MQTT, …)
*.db
*.db-*
*.sqlite*
*.log
*.log.*

# Caches and dependencies that are regenerated
homeassistant/.cache/
homeassistant/deps/
homeassistant/tts/
__pycache__/
.esphome/
node_modules/

# Installed by HACS; HACS reinstalls them
homeassistant/custom_components/
homeassistant/www/community/

# Huge files
backup/
media/
`

// baseGitignoreFor adapts the base .gitignore to a root: lines for other
// mounts are dropped and the root's own prefix is removed.
func baseGitignoreFor(root string) string {
	if root == "/" {
		return baseGitignore
	}
	rel := strings.TrimPrefix(root, "/") + "/"
	var b strings.Builder
	for line := range strings.SplitSeq(baseGitignore, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			b.WriteString(line + "\n")
		case !strings.Contains(strings.TrimSuffix(trimmed, "/"), "/") && !isMountPattern(trimmed):
			b.WriteString(line + "\n") // matches at any depth
		case strings.HasPrefix(trimmed, rel):
			b.WriteString(strings.TrimPrefix(line, rel) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func isMountPattern(p string) bool {
	for _, m := range Mounts {
		if p == m+"/" {
			return true
		}
	}
	return false
}

// ensureBaseGitignore writes the base .gitignore at the root when this is
// the first sync and neither the repo nor the box has one.
func (e *Engine) ensureBaseGitignore(ctx context.Context, repoTip string) error {
	p := filepath.Join(e.workDir(), ".gitignore")
	if _, err := os.Lstat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if repoTip != "" {
		entries, err := e.repo.LsTree(ctx, repoTip)
		if err != nil {
			return err
		}
		for _, en := range entries {
			if en.Path == ".gitignore" {
				return nil
			}
		}
	}
	return os.WriteFile(p, []byte(baseGitignoreFor(e.root)), 0o644)
}
