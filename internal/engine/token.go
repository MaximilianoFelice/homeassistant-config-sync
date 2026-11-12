package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ensureAPIToken creates the HTTP API's bearer token if it does not exist.
// It is never rotated by init.
func ensureAPIToken(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(b)+"\n"), 0o600)
}

// APIToken reads the HTTP API's token.
func APIToken(stateDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "api_token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
