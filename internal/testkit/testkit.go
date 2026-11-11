// Package testkit tiene helpers para tests autocontenidos: todo vive en
// t.TempDir(), los remotos son repos bare locales (file://) y git corre con
// el entorno aislado de gitx (sin la config, el SSH ni la red del usuario).
package testkit

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
)

// WriteFile crea los directorios padre y escribe el archivo.
func WriteFile(t testing.TB, path, content string) {
	t.Helper()
	WriteFileMode(t, path, content, 0o644)
}

// WriteFileMode es WriteFile con permisos explícitos.
func WriteFileMode(t testing.TB, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// Remove borra un archivo o directorio.
func Remove(t testing.TB, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// NewBareRepo crea un repo bare en un directorio temporal, con file://
// habilitado.
func NewBareRepo(t testing.TB) *gitx.Repo {
	t.Helper()
	r, err := gitx.InitBare(context.Background(), filepath.Join(t.TempDir(), "repo.git"), gitx.Options{AllowFile: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Tree devuelve el contenido de un ref como mapa path → contenido. Un ref
// inexistente da un mapa vacío.
func Tree(t testing.TB, r *gitx.Repo, ref string) map[string]string {
	t.Helper()
	ctx := context.Background()
	oid, err := r.ResolveRef(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if oid == "" {
		return out
	}
	entries, err := r.LsTree(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := r.ReadBlob(ctx, e.OID)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Path] = string(b)
	}
	return out
}

// Del marca un borrado en Commit.
const Del = "\x00borrar"

// Commit crea un commit sobre parent ("" = commit raíz) aplicando changes
// (path → contenido, o Del para borrar). Un contenido con el prefijo "x:" se
// guarda sin el prefijo y con modo ejecutable.
func Commit(t testing.TB, r *gitx.Repo, parent string, changes map[string]string) string {
	t.Helper()
	ctx := context.Background()
	base := ""
	if parent != "" {
		var err error
		if base, err = r.TreeOf(ctx, parent); err != nil {
			t.Fatal(err)
		}
	}
	var ups []gitx.IndexUpdate
	for p, c := range changes {
		if c == Del {
			ups = append(ups, gitx.IndexUpdate{Path: p})
			continue
		}
		mode := "100644"
		if rest, ok := strings.CutPrefix(c, "x:"); ok {
			mode, c = "100755", rest
		}
		oid, err := r.WriteBlob(ctx, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, gitx.IndexUpdate{Path: p, Mode: mode, OID: oid})
	}
	tree, err := r.BuildTree(ctx, base, ups)
	if err != nil {
		t.Fatal(err)
	}
	commitSeq++
	id, _ := gitx.ParseIdent("Test <t@example.invalid>", time.Unix(1700000000+commitSeq, 0))
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	c, err := r.CommitTree(ctx, tree, parents, "test\n", id, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var commitSeq int64

// Disk devuelve los archivos regulares bajo root como mapa path relativo
// ('/') → contenido. Los symlinks y directorios no aparecen.
func Disk(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Modes devuelve los modos de git de un ref como mapa path → modo.
func Modes(t testing.TB, r *gitx.Repo, ref string) map[string]string {
	t.Helper()
	ctx := context.Background()
	oid, err := r.ResolveRef(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	entries, err := r.LsTree(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Path] = e.Mode
	}
	return out
}
