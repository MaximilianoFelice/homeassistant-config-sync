package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/secrets"
)

// scanSecrets runs the secret scanner over a folder, the way the app scans
// the repo: gitleaks with the folder's .gitleaks.toml and .gitleaksignore if
// it has them. It prints path:line: rule [fingerprint], never the value, and
// exits with 1 if there are findings.
func scanSecrets(_ globalFlags, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: hcs scan-secrets DIR")
		return 2
	}
	root := args[0]
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			_, _ = fmt.Fprintln(stderr, "hcs:", err)
		}
		return b
	}
	sc, err := secrets.New(read(secrets.ConfigFile), read(secrets.IgnoreFile))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return 2
	}
	files, total := 0, 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files++
		for _, f := range sc.Scan(filepath.ToSlash(rel), b) {
			total++
			_, _ = fmt.Fprintln(stdout, f.String())
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hcs:", err)
		return 2
	}
	_, _ = fmt.Fprintf(stderr, "%d finding(s) in %d file(s)\n", total, files)
	if total > 0 {
		return 1
	}
	return 0
}
