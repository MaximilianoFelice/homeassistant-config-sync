// Package paths decides which files the engine manages and translates between
// repo paths and box paths (design.md §4 and §5).
//
// Every repo path uses '/' and is relative to the repo root. Every box path
// is absolute. Classification is pure: it does not touch the disk (symlinks,
// size and binaries are handled by the scan).
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

// HardDeny are the patterns that are never managed, whatever the
// configuration says. They are evaluated against the path relative to the
// mapping root, lowercased, at any depth.
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

// Direction tells which way a managed path travels.
type Direction int

const (
	// Both: box → repo and repo → box.
	Both Direction = iota
	// PushOnly: box → repo only; what comes from main is not applied.
	PushOnly
)

func (d Direction) String() string {
	if d == PushOnly {
		return "push"
	}
	return "both"
}

// Reason explains why a path is not managed.
type Reason string

const (
	ReasonNone            Reason = ""
	ReasonInvalidPath     Reason = "invalid_path"
	ReasonOutsideMappings Reason = "outside_mappings"
	ReasonNotIncluded     Reason = "not_included"
	ReasonExcluded        Reason = "excluded"
	ReasonHardDenied      Reason = "hard_denied"
)

// Result is the classification of a path.
type Result struct {
	// Managed says whether the engine manages the path.
	Managed bool
	// Direction is only meaningful if Managed.
	Direction Direction
	// Reason is only meaningful if !Managed.
	Reason Reason
	// Mapping is the index of the mapping that contains the path, or -1.
	Mapping int
	// RepoPath is the path in the repo ('/'), empty if invalid.
	RepoPath string
	// BoxPath is the absolute path on the box, empty if outside the mappings.
	BoxPath string
	// Rel is the path relative to the mapping root ('/').
	Rel string
}

// Matcher classifies paths according to an already validated list of mappings.
type Matcher struct {
	mappings []config.Mapping
}

// New creates a Matcher. The mappings must have passed Config.Validate; New
// only re-checks the globs, which are what could make it fail later.
func New(mappings []config.Mapping) (*Matcher, error) {
	for i, m := range mappings {
		for _, g := range slices.Concat(m.Include, m.PushOnly, m.Exclude) {
			if !doublestar.ValidatePattern(g) {
				return nil, fmt.Errorf("mappings[%d]: invalid glob %q", i, g)
			}
		}
	}
	return &Matcher{mappings: slices.Clone(mappings)}, nil
}

// Mappings returns the Matcher's mappings.
func (m *Matcher) Mappings() []config.Mapping { return slices.Clone(m.mappings) }

// ClassifyRepoPath classifies a repo path. It is the entry point for
// everything that comes from main, so it rejects any path that could escape
// the mapping.
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

// ClassifyBoxPath classifies an absolute box path.
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
	// If a path matches both Include and PushOnly, the more restrictive
	// direction wins.
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

// PruneDir says whether the scan can skip an entire directory of mapping i
// (relDir relative to its root, with '/'): true for .git directories and when
// an "X/**" pattern from exclude or the denylist covers everything inside.
// It saves walking custom_components, deps, .cache, etc.
func (m *Matcher) PruneDir(i int, relDir string) bool {
	if strings.EqualFold(path.Base(relDir), ".git") {
		return true
	}
	return dirCovered(HardDeny, strings.ToLower(relDir)) || dirCovered(m.mappings[i].Exclude, relDir)
}

// dirCovered says whether some "X/**" pattern covers dir or one of its ancestors.
func dirCovered(globs []string, dir string) bool {
	for _, g := range globs {
		prefix, ok := strings.CutSuffix(g, "/**")
		if !ok {
			continue
		}
		for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
			if ok, err := doublestar.Match(prefix, d); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// HardDenied says whether a path relative to a mapping root falls in the hard
// denylist. It is case-insensitive.
func HardDenied(rel string) bool {
	return matchAny(HardDeny, strings.ToLower(rel))
}

func matchAny(globs []string, p string) bool {
	for _, g := range globs {
		// The globs are already validated: an error here is impossible and,
		// just in case, it is treated as "no match".
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

// ValidRepoPath says whether a repo path is safe to write to disk: relative,
// with '/' as the separator, with no empty, '.', '..' or '.git' components,
// and no backslashes or control bytes.
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
