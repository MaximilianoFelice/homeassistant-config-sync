package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// githubKnownHosts son las host keys que GitHub publica en
// https://api.github.com/meta (ssh_keys), verificadas el 2026-11-11.
var githubKnownHosts = []string{
	"github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
	"github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=",
	"github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=",
}

// keyComment identifica la deploy key en la lista de GitHub.
const keyComment = "homeassistant-config-sync"

// ensureDeployKey genera el par ed25519 si no existe y devuelve la clave
// pública en formato authorized_keys.
func ensureDeployKey(dir string) (pub string, created bool, err error) {
	privPath := filepath.Join(dir, "id_ed25519")
	pubPath := privPath + ".pub"
	if b, err := os.ReadFile(pubPath); err == nil {
		if _, err := os.Stat(privPath); err == nil {
			return strings.TrimSpace(string(b)), false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, err
	}
	block, err := ssh.MarshalPrivateKey(sk, keyComment)
	if err != nil {
		return "", false, err
	}
	sshPub, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", false, err
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + keyComment
	if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(pubPath, []byte(pub+"\n"), 0o644); err != nil {
		return "", false, err
	}
	return pub, true, nil
}

// writeKnownHosts escribe el known_hosts fijo: GitHub más los extra de la
// config. Se reescribe en cada init para seguir a la config.
func writeKnownHosts(path string, extra []string) error {
	lines := append(append([]string{}, githubKnownHosts...), extra...)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// DeployKey devuelve la clave pública de un state dir ya inicializado.
func DeployKey(stateDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "ssh", "id_ed25519.pub"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
