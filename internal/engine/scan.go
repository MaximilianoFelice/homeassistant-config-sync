package engine

import (
	"bytes"
	"context"
	"sort"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// scanCache keeps the scanner and the findings of every (path, blob) already
// scanned, for as long as the repo's gitleaks files do not change. The whole
// tree is scanned every cycle (docs/redesign.md §9); the cache makes that
// cost one scan per new blob.
type scanCache struct {
	key      string
	scanner  *secrets.Scanner
	findings map[string][]secrets.Finding // path\x00oid → findings
}

// scanTree returns the scanner's findings over a whole tree, with the rules
// and ignores the tree itself carries (.gitleaks.toml, .gitleaksignore).
func (e *Engine) scanTree(ctx context.Context, tree string) ([]secrets.Finding, error) {
	entries, err := e.repo.LsTree(ctx, tree)
	if err != nil {
		return nil, err
	}
	var cfgOID, ignoreOID string
	for _, en := range entries {
		switch en.Path {
		case secrets.ConfigFile:
			cfgOID = en.OID
		case secrets.IgnoreFile:
			ignoreOID = en.OID
		}
	}
	if key := cfgOID + ":" + ignoreOID; e.scan.scanner == nil || e.scan.key != key {
		files, err := e.repo.ReadBlobs(ctx, nonEmpty(cfgOID, ignoreOID))
		if err != nil {
			return nil, err
		}
		sc, err := secrets.New(files[cfgOID], files[ignoreOID])
		if err != nil {
			return nil, err
		}
		e.scan = scanCache{key: key, scanner: sc, findings: map[string][]secrets.Finding{}}
	}

	var pending []string
	todo := map[string][]string{} // oid → paths
	for _, en := range entries {
		if en.Mode == "120000" { // symlinks hold a path, not content
			continue
		}
		if _, ok := e.scan.findings[en.Path+"\x00"+en.OID]; !ok {
			if len(todo[en.OID]) == 0 {
				pending = append(pending, en.OID)
			}
			todo[en.OID] = append(todo[en.OID], en.Path)
		}
	}
	if len(pending) > 0 {
		blobs, err := e.repo.ReadBlobs(ctx, pending)
		if err != nil {
			return nil, err
		}
		for oid, paths := range todo {
			for _, p := range paths {
				var found []secrets.Finding
				if !binary(blobs[oid]) {
					found = e.scan.scanner.Scan(p, blobs[oid])
				}
				e.scan.findings[p+"\x00"+oid] = found
			}
		}
	}

	var out []secrets.Finding
	for _, en := range entries {
		out = append(out, e.scan.findings[en.Path+"\x00"+en.OID]...)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, nil
}

// binary uses git's own heuristic (a NUL byte in the first 8000 bytes): the
// scanner, like gitleaks' CLI, does not look inside binary files. They are
// synced all the same.
func binary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

func nonEmpty(oids ...string) []string {
	var out []string
	for _, o := range oids {
		if o != "" {
			out = append(out, o)
		}
	}
	return out
}
