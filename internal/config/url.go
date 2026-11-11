package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// scpLike matchea la sintaxis corta de SSH de git: [user@]host:path, donde no
// hay ninguna barra antes del primer ':'.
var scpLike = regexp.MustCompile(`^(?:([^@/:\s]+)@)?([^@/:\s]+):([^\s].*)$`)

// ValidateRepoURL acepta solo remotos SSH (design.md §9): git@host:owner/repo
// o ssh://[user@]host[:port]/path. Rechaza HTTP(S), git://, credenciales
// embebidas, transportes de remote helpers (<transport>::<address>) y
// cualquier cosa que git pueda interpretar como una opción.
//
// Con allowFile también acepta file:// y paths absolutos (v0 y tests).
func ValidateRepoURL(raw string, allowFile bool) error {
	if raw == "" {
		return errors.New("está vacío")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\n\r\t") {
		return errors.New("tiene espacios o caracteres de control")
	}
	if strings.HasPrefix(raw, "-") {
		return errors.New("no puede empezar con '-'")
	}
	if strings.Contains(raw, "::") {
		return errors.New("los transportes <transport>::<address> no están permitidos")
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("no es una URL válida: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh":
			if _, hasPass := u.User.Password(); hasPass {
				return errors.New("no puede llevar contraseña embebida: usá la deploy key")
			}
			if u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") {
				return errors.New("host inválido")
			}
			if u.Path == "" || u.Path == "/" {
				return errors.New("falta el path del repo")
			}
			return nil
		case "file":
			if !allowFile {
				return errors.New("file:// solo se permite con --allow-file-remote")
			}
			return nil
		case "http", "https":
			return errors.New("HTTP(S) no está permitido: usá SSH con una deploy key (git@host:owner/repo.git)")
		default:
			return fmt.Errorf("protocolo %q no permitido: solo SSH", u.Scheme)
		}
	}

	if filepath.IsAbs(raw) {
		if !allowFile {
			return errors.New("los paths locales solo se permiten con --allow-file-remote")
		}
		return nil
	}

	m := scpLike.FindStringSubmatch(raw)
	if m == nil {
		return errors.New("formato no reconocido: esperaba git@host:owner/repo.git o ssh://host/path")
	}
	if strings.HasPrefix(m[2], "-") {
		return errors.New("host inválido")
	}
	return nil
}
