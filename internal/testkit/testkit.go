// Package testkit tiene helpers para tests autocontenidos: todo vive en
// t.TempDir(), los remotos son repos bare locales (file://) y git corre con
// el entorno aislado de gitx (sin la config, el SSH ni la red del usuario).
package testkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
