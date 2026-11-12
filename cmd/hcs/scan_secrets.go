package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// scanMaxSize is the largest file size scan-secrets looks at.
const scanMaxSize = 1 << 20

// scanSecrets runs the scanner over arbitrary files or directories. It prints
// path:line: rule (severity) [fingerprint], never the value. It exits with 1
// if there are blocking findings.
func scanSecrets(g globalFlags, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: hcs scan-secrets [--config FILE] PATH...")
		return 2
	}
	cfg := config.Default()
	if g.config != "" {
		var err error
		if cfg, err = config.Load(g.config); err != nil {
			_, _ = fmt.Fprintln(stderr, "hcs:", err)
			return 2
		}
	}
	sc, err := secrets.New(cfg.SecretScan)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return 2
	}

	contents := map[string][]byte{}
	for _, root := range args {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > scanMaxSize {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
				return nil // binary
			}
			contents[filepath.ToSlash(p)] = b
			return nil
		})
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "hcs:", err)
			return 2
		}
	}

	warnings, err := sc.Check(contents)
	for _, w := range warnings {
		_, _ = fmt.Fprintln(stdout, w.String())
	}
	var be *secrets.BlockedError
	if errors.As(err, &be) {
		for _, f := range be.Findings {
			_, _ = fmt.Fprintln(stdout, f.String())
		}
		_, _ = fmt.Fprintf(stderr, "%d blocking finding(s), %d warning(s) in %d file(s)\n", len(be.Findings), len(warnings), len(contents))
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return 2
	}
	_, _ = fmt.Fprintf(stderr, "no blocking findings (%d warning(s)) in %d file(s)\n", len(warnings), len(contents))
	return 0
}
