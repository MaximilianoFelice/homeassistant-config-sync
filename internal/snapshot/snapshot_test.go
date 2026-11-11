package snapshot_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/snapshot"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

const boxRef = "refs/heads/box"

var ctx = context.Background()

// env es un box de mentira + el repo bare del motor, todo en un TempDir.
type env struct {
	t    *testing.T
	ha   string
	repo *gitx.Repo
	opts snapshot.Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	ms := config.DefaultMappings()
	ms[0].Box = filepath.Join(root, "homeassistant")
	ms[1].Box = filepath.Join(root, "addon_configs")
	if err := os.MkdirAll(ms[0].Box, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := paths.New(ms)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		t:    t,
		ha:   ms[0].Box,
		repo: testkit.NewBareRepo(t),
		opts: snapshot.Options{
			Matcher:            m,
			Ref:                boxRef,
			MaxFileSize:        1024,
			DeleteGuardFiles:   25,
			DeleteGuardPercent: 20,
			Author:             "Home Assistant <ha@box.local>",
		},
	}
}

func (e *env) write(p, content string) { testkit.WriteFile(e.t, filepath.Join(e.ha, p), content) }
func (e *env) rm(p string)             { testkit.Remove(e.t, filepath.Join(e.ha, p)) }
func (e *env) tree() map[string]string { return testkit.Tree(e.t, e.repo, boxRef) }

func (e *env) take() *snapshot.Result {
	e.t.Helper()
	res, err := snapshot.Take(ctx, e.repo, e.opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) head() string {
	e.t.Helper()
	oid, err := e.repo.ResolveRef(ctx, boxRef)
	if err != nil {
		e.t.Fatal(err)
	}
	return oid
}

// seed commitea un árbol arbitrario en box, como si viniera de main.
func (e *env) seed(files map[string]string) string {
	e.t.Helper()
	var ups []gitx.IndexUpdate
	for p, c := range files {
		oid, err := e.repo.WriteBlob(ctx, []byte(c))
		if err != nil {
			e.t.Fatal(err)
		}
		ups = append(ups, gitx.IndexUpdate{Path: p, Mode: "100644", OID: oid})
	}
	tree, err := e.repo.BuildTree(ctx, "", ups)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := gitx.ParseIdent("Dev <dev@example.invalid>", time.Now())
	c, err := e.repo.CommitTree(ctx, tree, nil, "seed\n", id, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.repo.UpdateRef(ctx, boxRef, c, e.head()); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func assertTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("árbol con %d archivos, quería %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, quería %q", p, got[p], c)
		}
	}
}

func kinds(cs []snapshot.Change) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, string(c.Kind)+" "+c.Path)
	}
	return strings.Join(parts, ", ")
}

func TestFirstSnapshotIsARootCommitWithOnlyManagedFiles(t *testing.T) {
	e := newEnv(t)
	e.write("configuration.yaml", "homeassistant:\n")
	e.write("automations.yaml", "[]\n")
	e.write("secrets.yaml", "pw: 1\n")
	e.write("home-assistant_v2.db", "SQLite")
	e.write("custom_components/x/services.yaml", "a:\n")

	res := e.take()
	if res.Parent != "" || res.Commit == "" {
		t.Fatalf("quería un commit raíz, dio %+v", res)
	}
	assertTree(t, e.tree(), map[string]string{
		"config/configuration.yaml": "homeassistant:\n",
		"config/automations.yaml":   "[]\n",
	})
	if got := kinds(res.Changes); got != "A config/automations.yaml, A config/configuration.yaml" {
		t.Errorf("cambios = %s", got)
	}
}

func TestSnapshotIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	first := e.take()
	second := e.take()
	if second.Commit != "" || len(second.Changes) != 0 {
		t.Errorf("un segundo snapshot sin cambios no debería commitear: %+v", second)
	}
	if e.head() != first.Commit {
		t.Error("box se movió sin cambios")
	}

	// También con la cache de stat del primero.
	e.opts.Cache = first.Cache
	if third := e.take(); third.Commit != "" {
		t.Errorf("con cache tampoco: %+v", third)
	}
}

func TestSnapshotAddModifyDelete(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	e.write("scripts.yaml", "a:\n")
	e.write("scenes.yaml", "[]\n")
	first := e.take()

	e.write("automations.yaml", "- id: '1'\n")
	e.rm("scenes.yaml")
	e.write("packages/nuevo.yaml", "x:\n")
	res := e.take()

	if res.Parent != first.Commit {
		t.Errorf("padre = %s, quería %s", res.Parent, first.Commit)
	}
	if got := kinds(res.Changes); got != "M config/automations.yaml, A config/packages/nuevo.yaml, D config/scenes.yaml" {
		t.Errorf("cambios = %s", got)
	}
	assertTree(t, e.tree(), map[string]string{
		"config/automations.yaml":    "- id: '1'\n",
		"config/scripts.yaml":        "a:\n",
		"config/packages/nuevo.yaml": "x:\n",
	})
}

func TestSnapshotPreservesWhatTheRepoOwns(t *testing.T) {
	e := newEnv(t)
	e.seed(map[string]string{
		"README.md":                 "# mi casa\n",
		".github/workflows/ci.yaml": "on: push\n",
		"config/notas.txt":          "no gestionado\n",
		"config/automations.yaml":   "viejo\n",
		"config/borrado.yaml":       "chau\n",
	})
	e.write("automations.yaml", "nuevo\n")

	res := e.take()
	if got := kinds(res.Changes); got != "D config/borrado.yaml, M config/automations.yaml" &&
		got != "M config/automations.yaml, D config/borrado.yaml" {
		t.Errorf("cambios = %s", got)
	}
	assertTree(t, e.tree(), map[string]string{
		"README.md":                 "# mi casa\n",
		".github/workflows/ci.yaml": "on: push\n",
		"config/notas.txt":          "no gestionado\n",
		"config/automations.yaml":   "nuevo\n",
	})
}

func TestSnapshotFileBecomesDirectory(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	e.take()
	e.rm("automations.yaml")
	e.write("automations.yaml/cocina.yaml", "- id: c\n")
	e.take()
	assertTree(t, e.tree(), map[string]string{"config/automations.yaml/cocina.yaml": "- id: c\n"})
}

func TestSnapshotTracksExecutableBit(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.ha, "python_scripts/hola.py")
	testkit.WriteFileMode(t, p, "print(1)\n", 0o644)
	e.take()
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.take()
	if got := kinds(res.Changes); got != "M config/python_scripts/hola.py" {
		t.Errorf("cambios = %s", got)
	}
	if m := testkit.Modes(t, e.repo, boxRef)["config/python_scripts/hola.py"]; m != "100755" {
		t.Errorf("modo = %s", m)
	}
}

func TestSkippedFilesKeepTheirPreviousVersion(t *testing.T) {
	e := newEnv(t)
	e.write("grande.yaml", "chico\n")
	e.write("binario.yaml", "texto\n")
	e.write("link.yaml", "archivo\n")
	e.take()

	e.write("grande.yaml", strings.Repeat("x", 2048))
	e.write("binario.yaml", "a\x00b")
	e.rm("link.yaml")
	if err := os.Symlink(filepath.Join(e.ha, "grande.yaml"), filepath.Join(e.ha, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	res := e.take()
	if res.Commit != "" {
		t.Errorf("saltear no es borrar ni modificar; cambios = %s", kinds(res.Changes))
	}
	if len(res.Skipped) != 3 {
		t.Errorf("salteados = %+v", res.Skipped)
	}
}

func TestMissingRootIsSuspicious(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	before := e.take().Commit

	e.rm("")
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, snapshot.ErrSuspicious) {
		t.Fatalf("quería ErrSuspicious, dio %v", err)
	}
	if e.head() != before {
		t.Error("box no debería moverse")
	}
}

func TestEmptyRootIsSuspicious(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	e.write("notas.txt", "esto no es gestionado\n")
	before := e.take().Commit

	e.rm("automations.yaml")
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, snapshot.ErrSuspicious) {
		t.Fatalf("quería ErrSuspicious (mount vacío), dio %v", err)
	}
	if e.head() != before {
		t.Error("box no debería moverse")
	}
}

func TestMassDeleteGuard(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		deleted int
		blocked bool
	}{
		{"config chica, borro 1 de 4", 4, 1, false},
		{"borro 4 de 10: 40% pero menos de 5", 10, 4, false},
		{"borro 5 de 10: 50% y al menos 5", 10, 5, true},
		{"borro 5 de 40: 12.5%", 40, 5, false},
		{"borro 26 de 200: más de 25", 200, 26, true},
		{"borro 25 de 200: justo 25", 200, 25, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			for i := range tt.total {
				e.write(fmt.Sprintf("packages/p%03d.yaml", i), fmt.Sprintf("n: %d\n", i))
			}
			before := e.take().Commit
			for i := range tt.deleted {
				e.rm(fmt.Sprintf("packages/p%03d.yaml", i))
			}
			res, err := snapshot.Take(ctx, e.repo, e.opts)
			if tt.blocked {
				if !errors.Is(err, snapshot.ErrMassDelete) {
					t.Fatalf("quería ErrMassDelete, dio %v", err)
				}
				if e.head() != before {
					t.Error("box no debería moverse")
				}
				// Con confirmación explícita pasa.
				e.opts.AllowMassDelete = true
				if res := e.take(); res.Commit == "" {
					t.Error("con AllowMassDelete debería commitear")
				}
				return
			}
			if err != nil || res.Commit == "" {
				t.Fatalf("no quería bloqueo: %v %+v", err, res)
			}
		})
	}
}

func TestBeforeCommitErrorWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	before := e.take().Commit

	secret := "api_key: sk-esto-es-un-secreto\n"
	e.write("rest.yaml", secret)
	blocked := errors.New("secreto encontrado")
	var seen map[string][]byte
	e.opts.BeforeCommit = func(_ context.Context, cs []snapshot.Change, contents map[string][]byte) error {
		seen = contents
		return blocked
	}
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, blocked) {
		t.Fatalf("quería el error del hook, dio %v", err)
	}
	if string(seen["config/rest.yaml"]) != secret {
		t.Errorf("el hook no recibió el contenido: %q", seen)
	}
	if e.head() != before {
		t.Error("box no debería moverse")
	}
	// Ni siquiera el blob quedó escrito en el repo local.
	oid := gitBlobOID(t, secret)
	if ok, err := e.repo.HasObject(ctx, oid); err != nil || ok {
		t.Errorf("el blob del secreto quedó en el repo (%v, %v)", ok, err)
	}
}

func TestSnapshotUsesCacheButStillSeesEdits(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.ha, "automations.yaml")
	e.write("automations.yaml", "aaaa\n")
	ts := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	first := e.take()
	if len(first.Cache) != 1 {
		t.Fatalf("cache = %v", first.Cache)
	}

	e.opts.Cache = first.Cache
	e.write("automations.yaml", "bbbb\n")
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	if res := e.take(); kinds(res.Changes) != "M config/automations.yaml" {
		t.Errorf("la cache escondió una edición: %s", kinds(res.Changes))
	}
	assertTree(t, e.tree(), map[string]string{"config/automations.yaml": "bbbb\n"})
}

func TestMessage(t *testing.T) {
	one := snapshot.Message([]snapshot.Change{{Path: "config/a.yaml", Kind: snapshot.Modified}})
	if !strings.HasPrefix(one, "box: config/a.yaml\n\nM config/a.yaml\n") {
		t.Errorf("mensaje = %q", one)
	}
	var many []snapshot.Change
	for i := range 105 {
		many = append(many, snapshot.Change{Path: fmt.Sprintf("config/%d.yaml", i), Kind: snapshot.Added})
	}
	msg := snapshot.Message(many)
	if !strings.HasPrefix(msg, "box: 105 cambios\n\n") || !strings.Contains(msg, "… y 5 más") {
		t.Errorf("mensaje = %q", msg)
	}
}

// gitBlobOID calcula el OID con git en un repo descartable, para no depender
// de scan.BlobOID (el código bajo test).
func gitBlobOID(t *testing.T, s string) string {
	t.Helper()
	oid, err := testkit.NewBareRepo(t).WriteBlob(ctx, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return oid
}
