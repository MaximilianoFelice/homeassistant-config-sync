// Package paths decide qué archivos gestiona el motor y traduce entre paths
// del repo y paths del box (design.md §4 y §5).
//
// Todo path del repo se expresa con '/' y relativo a la raíz del repo. Todo
// path del box es absoluto. La clasificación es pura: no toca el disco
// (symlinks, tamaño y binarios se resuelven en el escaneo).
package paths

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
)

// HardDeny son los patrones que nunca se gestionan, diga lo que diga la
// configuración. Se evalúan contra el path relativo a la raíz del mapeo, en
// minúsculas, y en cualquier nivel de profundidad.
var HardDeny = []string{
	"**/secrets.yaml",
	"**/secrets.yml",
	"**/*.db",
	"**/*.db-shm",
	"**/*.db-wal",
	"**/*.db-journal",
	"**/*.sqlite*",
	"**/.storage/auth*",
	"**/.storage/core.config_entries",
	"**/.storage/http*",
	"**/.storage/cloud",
	"**/.cloud/**",
	"**/.ssh/**",
	"**/id_rsa*",
	"**/id_dsa*",
	"**/id_ecdsa*",
	"**/id_ed25519*",
	"**/*.pem",
	"**/*.key",
	"**/*.p12",
	"**/*.pfx",
	"**/*.log",
	"**/*.log.*",
}

// Direction indica en qué sentido viaja un path gestionado.
type Direction int

const (
	// Both: box → repo y repo → box.
	Both Direction = iota
	// PushOnly: solo box → repo; lo que llega desde main no se aplica.
	PushOnly
)

func (d Direction) String() string {
	if d == PushOnly {
		return "push"
	}
	return "both"
}

// Reason explica por qué un path no se gestiona.
type Reason string

const (
	ReasonNone            Reason = ""
	ReasonInvalidPath     Reason = "invalid_path"
	ReasonOutsideMappings Reason = "outside_mappings"
	ReasonNotIncluded     Reason = "not_included"
	ReasonExcluded        Reason = "excluded"
	ReasonHardDenied      Reason = "hard_denied"
)

// Result es la clasificación de un path.
type Result struct {
	// Managed indica si el motor gestiona el path.
	Managed bool
	// Direction vale solo si Managed.
	Direction Direction
	// Reason vale solo si !Managed.
	Reason Reason
	// Mapping es el índice del mapeo que contiene al path, o -1.
	Mapping int
	// RepoPath es el path en el repo ('/'), vacío si es inválido.
	RepoPath string
	// BoxPath es el path absoluto en el box, vacío si cae fuera de los mapeos.
	BoxPath string
	// Rel es el path relativo a la raíz del mapeo ('/').
	Rel string
}

// Matcher clasifica paths según una lista de mapeos ya validada.
type Matcher struct {
	mappings []config.Mapping
}

// New crea un Matcher. Los mapeos tienen que haber pasado Config.Validate;
// New solo vuelve a chequear los globs, que son lo que podría hacerlo fallar
// más tarde.
func New(mappings []config.Mapping) (*Matcher, error) {
	for i, m := range mappings {
		for _, g := range slices.Concat(m.Include, m.PushOnly, m.Exclude) {
			if !doublestar.ValidatePattern(g) {
				return nil, fmt.Errorf("mappings[%d]: glob inválido %q", i, g)
			}
		}
	}
	return &Matcher{mappings: slices.Clone(mappings)}, nil
}

// Mappings devuelve los mapeos del Matcher.
func (m *Matcher) Mappings() []config.Mapping { return slices.Clone(m.mappings) }

// ClassifyRepoPath clasifica un path del repo. Es la puerta de entrada para
// todo lo que llega desde main, así que rechaza cualquier path que pudiera
// escaparse del mapeo.
func (m *Matcher) ClassifyRepoPath(repoPath string) Result {
	if !ValidRepoPath(repoPath) {
		return Result{Reason: ReasonInvalidPath, Mapping: -1}
	}
	for i, mp := range m.mappings {
		rel, ok := strings.CutPrefix(repoPath, mp.Repo+"/")
		if !ok {
			continue
		}
		r := m.classifyRel(i, rel)
		r.RepoPath = repoPath
		r.BoxPath = filepath.Join(mp.Box, filepath.FromSlash(rel))
		return r
	}
	return Result{Reason: ReasonOutsideMappings, Mapping: -1, RepoPath: repoPath}
}

// ClassifyBoxPath clasifica un path absoluto del box.
func (m *Matcher) ClassifyBoxPath(boxPath string) Result {
	clean := filepath.Clean(boxPath)
	if !filepath.IsAbs(boxPath) || clean != boxPath {
		return Result{Reason: ReasonInvalidPath, Mapping: -1}
	}
	for i, mp := range m.mappings {
		relOS, err := filepath.Rel(mp.Box, clean)
		if err != nil || relOS == "." || relOS == ".." || strings.HasPrefix(relOS, ".."+string(filepath.Separator)) {
			continue
		}
		rel := filepath.ToSlash(relOS)
		repoPath := mp.Repo + "/" + rel
		if !ValidRepoPath(repoPath) {
			return Result{Reason: ReasonInvalidPath, Mapping: i, BoxPath: clean}
		}
		r := m.classifyRel(i, rel)
		r.RepoPath = repoPath
		r.BoxPath = clean
		return r
	}
	return Result{Reason: ReasonOutsideMappings, Mapping: -1, BoxPath: clean}
}

func (m *Matcher) classifyRel(i int, rel string) Result {
	mp := m.mappings[i]
	r := Result{Mapping: i, Rel: rel}

	if HardDenied(rel) {
		r.Reason = ReasonHardDenied
		return r
	}
	if matchAny(mp.Exclude, rel) {
		r.Reason = ReasonExcluded
		return r
	}
	// Si un path matchea Include y PushOnly a la vez, gana la dirección más
	// restrictiva.
	switch {
	case matchAny(mp.PushOnly, rel):
		r.Managed, r.Direction = true, PushOnly
	case matchAny(mp.Include, rel):
		r.Managed, r.Direction = true, Both
	default:
		r.Reason = ReasonNotIncluded
	}
	return r
}

// HardDenied dice si un path relativo a la raíz de un mapeo cae en la
// denylist dura. No distingue mayúsculas.
func HardDenied(rel string) bool {
	return matchAny(HardDeny, strings.ToLower(rel))
}

func matchAny(globs []string, p string) bool {
	for _, g := range globs {
		// Los globs ya están validados: un error acá es imposible y, por las
		// dudas, se trata como "no matchea".
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

// ValidRepoPath dice si un path del repo es seguro para escribir en disco:
// relativo, con '/' como separador, sin componentes vacíos, '.', '..' ni
// '.git', sin barras invertidas ni bytes de control.
func ValidRepoPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return false
		}
	}
	return true
}
