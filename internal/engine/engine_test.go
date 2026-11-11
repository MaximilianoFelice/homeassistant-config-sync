package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/merge"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

// world es un escenario completo y autocontenido: un box de mentira, un
// "GitHub" que es un repo bare local (file://), un usuario en "local" que
// commitea y pushea a main, y un Home Assistant de mentira.
type world struct {
	t      *testing.T
	ha     string
	addons string
	url    string
	origin *gitx.Repo
	dev    *gitx.Repo
	eng    *Engine
	opts   Options

	checks  int
	verdict ha.CheckResult
	reloads int
	events  []ha.Event
}

type fakeHA struct{ w *world }

func (f fakeHA) CheckConfig(context.Context) (ha.CheckResult, error) {
	f.w.checks++
	return f.w.verdict, nil
}
func (f fakeHA) ReloadAll(context.Context) error { f.w.reloads++; return nil }
func (f fakeHA) Notify(_ context.Context, ev ha.Event) error {
	f.w.events = append(f.w.events, ev)
	return nil
}

func newWorld(t *testing.T, mutate ...func(*config.Config)) *world {
	t.Helper()
	root := t.TempDir()
	origin, err := gitx.InitBare(ctx, filepath.Join(root, "origin.git"), gitx.Options{AllowFile: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, ha: filepath.Join(root, "homeassistant"), addons: filepath.Join(root, "addon_configs"),
		url: "file://" + origin.Dir(), origin: origin, dev: testkit.NewBareRepo(t),
		verdict: ha.CheckResult{Valid: true},
	}
	for _, d := range []string{w.ha, w.addons} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Repository = w.url
	cfg.Mappings[0].Box = w.ha
	cfg.Mappings[1].Box = w.addons
	cfg.Mappings[1].Include = []string{"**/*.conf"}
	for _, m := range mutate {
		m(cfg)
	}
	f := fakeHA{w}
	w.opts = Options{Config: cfg, StateDir: filepath.Join(root, "state"), AllowFileRemote: true, Checker: f, Reloader: f, Notifier: f}
	w.newEngine()
	if _, err := w.eng.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) newEngine() {
	w.t.Helper()
	e, err := New(w.opts)
	if err != nil {
		w.t.Fatal(err)
	}
	w.eng = e
}

// boxPath traduce config/… y app_configs/… al disco del box.
func (w *world) boxPath(p string) string {
	if rel, ok := strings.CutPrefix(p, "config/"); ok {
		return filepath.Join(w.ha, rel)
	}
	if rel, ok := strings.CutPrefix(p, "app_configs/"); ok {
		return filepath.Join(w.addons, rel)
	}
	w.t.Fatalf("path fuera de los mapeos: %s", p)
	return ""
}

// edit simula una edición en el box (UI de HA, editor de archivos…).
func (w *world) edit(p, content string) { testkit.WriteFile(w.t, w.boxPath(p), content) }

func (w *world) disk() map[string]string {
	out := map[string]string{}
	for k, v := range testkit.Disk(w.t, w.ha) {
		out["config/"+k] = v
	}
	for k, v := range testkit.Disk(w.t, w.addons) {
		out["app_configs/"+k] = v
	}
	return out
}

func (w *world) sync(so ...SyncOptions) *Report {
	w.t.Helper()
	var o SyncOptions
	if len(so) > 0 {
		o = so[0]
	}
	rep, err := w.eng.Sync(ctx, o)
	if err != nil {
		w.t.Fatalf("sync: %v (report: %+v)", err, rep)
	}
	return rep
}

func (w *world) remote(branch string) string {
	w.t.Helper()
	heads, err := w.origin.LsRemote(ctx, w.url)
	if err != nil {
		w.t.Fatal(err)
	}
	return heads[branch]
}

func (w *world) remoteFiles(branch string) map[string]string {
	return testkit.Tree(w.t, w.origin, "refs/heads/"+branch)
}

// devFetch trae main y box del remoto al repo del "usuario en local".
func (w *world) devFetch() (main, box string) {
	w.t.Helper()
	heads, err := w.dev.LsRemote(ctx, w.url)
	if err != nil {
		w.t.Fatal(err)
	}
	var specs []string
	for _, b := range []string{"main", "box"} {
		if _, ok := heads[b]; ok {
			specs = append(specs, "+refs/heads/"+b+":refs/remotes/origin/"+b)
		}
	}
	if len(specs) > 0 {
		if err := w.dev.Fetch(ctx, w.url, specs...); err != nil {
			w.t.Fatal(err)
		}
	}
	main, _ = w.dev.ResolveRef(ctx, "refs/remotes/origin/main")
	box, _ = w.dev.ResolveRef(ctx, "refs/remotes/origin/box")
	return main, box
}

// devPush commitea sobre origin/main y pushea, como un usuario en local.
func (w *world) devPush(changes map[string]string) string {
	w.t.Helper()
	main, _ := w.devFetch()
	c := testkit.Commit(w.t, w.dev, main, changes)
	if err := w.dev.Push(ctx, w.url, c+":refs/heads/main"); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// devResolve hace lo que haría un usuario en local: git merge origin/box en
// main, pisa los paths en conflicto con la resolución dada y pushea.
func (w *world) devResolve(resolution map[string]string) string {
	w.t.Helper()
	main, box := w.devFetch()
	mt, err := w.dev.MergeTree(ctx, main, box)
	if err != nil {
		w.t.Fatal(err)
	}
	base := mt.Tree
	var ups []gitx.IndexUpdate
	for p, c := range resolution {
		oid, err := w.dev.WriteBlob(ctx, []byte(c))
		if err != nil {
			w.t.Fatal(err)
		}
		ups = append(ups, gitx.IndexUpdate{Path: p, Mode: "100644", OID: oid})
	}
	tree, err := w.dev.BuildTree(ctx, base, ups)
	if err != nil {
		w.t.Fatal(err)
	}
	id, _ := gitx.ParseIdent("Dev <dev@example.invalid>", time.Now())
	c, err := w.dev.CommitTree(ctx, tree, []string{main, box}, "merge box\n", id, id)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.dev.Push(ctx, w.url, c+":refs/heads/main"); err != nil {
		w.t.Fatal(err)
	}
	return c
}

func (w *world) lastEvent() ha.Event {
	if len(w.events) == 0 {
		return ha.Event{}
	}
	return w.events[len(w.events)-1]
}

func assertFiles(t *testing.T, got, want map[string]string) {
	t.Helper()
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, quería %q", p, got[p], c)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("sobra %s", p)
		}
	}
}

// seedBox deja un box típico y lo sincroniza contra un remoto vacío.
func (w *world) seedBox() {
	w.t.Helper()
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Casa\n")
	w.edit("config/automations.yaml", "- id: '1'\n  alias: uno\n")
	w.edit("config/scripts.yaml", "hola:\n  sequence: []\n")
	w.edit("config/secrets.yaml", "pw: nunca-sale\n")
	if rep := w.sync(); rep.State != StateIdle {
		w.t.Fatalf("seed: %+v", rep)
	}
}

func TestFirstSyncToEmptyRemote(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	main, box := w.remote("main"), w.remote("box")
	if main == "" || main != box {
		t.Fatalf("main = %s, box = %s", main, box)
	}
	assertFiles(t, w.remoteFiles("main"), map[string]string{
		"config/configuration.yaml": "homeassistant:\n  name: Casa\n",
		"config/automations.yaml":   "- id: '1'\n  alias: uno\n",
		"config/scripts.yaml":       "hola:\n  sequence: []\n",
	})
	if w.checks != 0 {
		t.Error("no había nada que aplicar: no debería correr check_config")
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.MainHead != main || st.BoxHead != box {
		t.Errorf("estado: main %s box %s, quería %s", st.MainHead, st.BoxHead, main)
	}
}

func TestSecondSyncIsANoop(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	before, events := w.remote("main"), len(w.events)
	rep := w.sync()
	if rep.SnapshotCommit != "" || len(rep.Pushed) != 0 || rep.Applied {
		t.Errorf("un sync sin cambios no debería hacer nada: %+v", rep)
	}
	if w.remote("main") != before || len(w.events) != events {
		t.Error("main se movió o se emitió un evento sin cambios")
	}
}

func TestBoxChangeReachesMain(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.edit("config/automations.yaml", "- id: '1'\n  alias: editada en la UI\n")
	rep := w.sync()
	if rep.MergeKind != merge.FastForwardMain || rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	if got := w.remoteFiles("main")["config/automations.yaml"]; got != "- id: '1'\n  alias: editada en la UI\n" {
		t.Errorf("main/automations = %q", got)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main y box deberían coincidir")
	}
	if w.lastEvent().Type != ha.EventSyncFinished {
		t.Errorf("evento = %+v", w.lastEvent())
	}
}

func TestMainChangeIsAppliedCheckedAndReloaded(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence:\n    - delay: 1\n", "README.md": "# casa\n"})
	rep := w.sync()
	if rep.MergeKind != merge.FastForwardBox || !rep.Applied || !rep.Checked || !rep.Reloaded {
		t.Fatalf("= %+v", rep)
	}
	if w.checks != 1 || w.reloads != 1 {
		t.Errorf("checks = %d, reloads = %d", w.checks, w.reloads)
	}
	if got := w.disk()["config/scripts.yaml"]; got != "hola:\n  sequence:\n    - delay: 1\n" {
		t.Errorf("disco = %q", got)
	}
	if _, ok := w.disk()["config/README.md"]; ok {
		t.Error("el README es del repo: no baja al box")
	}
	if w.remote("box") != w.remote("main") {
		t.Error("box debería avanzar a main")
	}
}

func TestBothSidesMergeCleanly(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [main]\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: box\n")
	rep := w.sync()
	if rep.MergeKind != merge.Merged || !rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	d := w.disk()
	if d["config/scripts.yaml"] != "hola:\n  sequence: [main]\n" || d["config/automations.yaml"] != "- id: '1'\n  alias: box\n" {
		t.Errorf("disco = %v", d)
	}
	m := w.remoteFiles("main")
	if m["config/scripts.yaml"] != d["config/scripts.yaml"] || m["config/automations.yaml"] != d["config/automations.yaml"] {
		t.Errorf("main = %v", m)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main y box deberían coincidir")
	}
}

func TestConflictStopsKeepsBoxAndResolves(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	mainBefore := w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: desde main\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: desde la UI\n")

	rep := w.sync()
	if rep.State != StateConflict || !slices.Equal(rep.ConflictPaths, []string{"config/automations.yaml"}) {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != mainBefore {
		t.Error("main no se toca en un conflicto")
	}
	if got := w.remoteFiles("box")["config/automations.yaml"]; got != "- id: '1'\n  alias: desde la UI\n" {
		t.Errorf("box tiene que tener la edición del box: %q", got)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: desde la UI\n" {
		t.Errorf("el disco no se toca en un conflicto: %q", got)
	}
	if ev := w.lastEvent(); ev.Type != ha.EventConflict || !ev.Persistent || !strings.Contains(ev.Message, "config/automations.yaml") {
		t.Errorf("evento = %+v", ev)
	}

	// Mientras hay conflicto, el box se sigue guardando.
	w.edit("config/scripts.yaml", "hola:\n  sequence: [durante el conflicto]\n")
	if rep := w.sync(); rep.State != StateConflict {
		t.Fatalf("sigue en conflicto: %+v", rep)
	}
	if got := w.remoteFiles("box")["config/scripts.yaml"]; got != "hola:\n  sequence: [durante el conflicto]\n" {
		t.Errorf("box no recibió la edición durante el conflicto: %q", got)
	}

	// El usuario resuelve en local y el ciclo siguiente sale limpio.
	w.devResolve(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: resuelta\n"})
	rep = w.sync()
	if rep.State != StateIdle {
		t.Fatalf("después de resolver: %+v", rep)
	}
	d := w.disk()
	if d["config/automations.yaml"] != "- id: '1'\n  alias: resuelta\n" || d["config/scripts.yaml"] != "hola:\n  sequence: [durante el conflicto]\n" {
		t.Errorf("disco = %v", d)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main y box deberían coincidir")
	}
}

func TestRejectedChangeIsNotRetriedUntilMainChanges(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	mainBad := w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  trigger: invalido\n"})
	w.verdict = ha.CheckResult{Valid: false, Errors: "Invalid config for [automation]: trigger"}

	rep := w.sync()
	if rep.State != StateRejected || !strings.Contains(rep.Error, "Invalid config") {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: uno\n" {
		t.Errorf("rollback: %q", got)
	}
	if w.remote("main") != mainBad || w.reloads != 0 {
		t.Error("main no se toca y no hay reload")
	}

	// Mismo main: no se vuelve a intentar.
	if rep := w.sync(); rep.State != StateRejected || w.checks != 1 {
		t.Errorf("no debería reintentar: %+v, checks = %d", rep, w.checks)
	}

	// El usuario arregla main: se aplica.
	w.verdict = ha.CheckResult{Valid: true}
	w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: arreglada\n"})
	if rep := w.sync(); rep.State != StateIdle || !rep.Applied {
		t.Fatalf("después del arreglo: %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: arreglada\n" {
		t.Errorf("disco = %q", got)
	}
}

func TestSecretBlocksEverything(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	before := w.remote("box")
	secret := "sk-" + "Zq8wX2vN7mK4pR9tL3yB"
	w.edit("config/rest.yaml", "sensor:\n  api_key: "+secret+"\n")
	rep := w.sync()
	if rep.State != StateBlockedSecrets || len(rep.Findings) == 0 {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("box") != before || rep.SnapshotCommit != "" {
		t.Error("no se commitea ni se pushea nada")
	}
	ev := w.lastEvent()
	if ev.Type != ha.EventBlockedSecrets || strings.Contains(ev.Message, secret) || strings.Contains(rep.Error, secret) {
		t.Errorf("el evento no puede incluir el valor: %+v", ev)
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.State != StateBlockedSecrets {
		t.Errorf("estado persistido = %s", st.State)
	}

	w.edit("config/rest.yaml", "sensor:\n  api_key: !secret rest_api_key\n")
	if rep := w.sync(); rep.State != StateIdle {
		t.Fatalf("después de arreglarlo: %+v", rep)
	}
	if got := w.remoteFiles("main")["config/rest.yaml"]; !strings.Contains(got, "!secret") {
		t.Errorf("main/rest.yaml = %q", got)
	}
}

func TestBrokenMountIsSuspicious(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	before := w.remote("main")
	testkit.Remove(t, w.ha)
	rep := w.sync()
	if rep.State != StateSuspiciousSnapshot {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != before || w.remote("box") != before {
		t.Error("no se pushea nada")
	}
}

func TestBootstrapOnExistingMain(t *testing.T) {
	w := newWorld(t)
	// El repo ya existe con una config distinta de la del box.
	w.devPush(map[string]string{
		"README.md":                 "# casa\n",
		"config/configuration.yaml": "homeassistant:\n  name: Repo\n",
		"config/viejo.yaml":         "v: 1\n",
	})
	mainBefore := w.remote("main")
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Box\n")
	w.edit("config/automations.yaml", "[]\n")

	rep := w.sync()
	if rep.State != StateBootstrapReview {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != mainBefore {
		t.Error("main no se toca hasta revisar")
	}
	box := w.remoteFiles("box")
	assertFiles(t, box, map[string]string{
		"README.md":                 "# casa\n",
		"config/configuration.yaml": "homeassistant:\n  name: Box\n",
		"config/automations.yaml":   "[]\n",
	})
	if w.checks != 0 || w.disk()["config/configuration.yaml"] != "homeassistant:\n  name: Box\n" {
		t.Error("el disco no se toca")
	}
	if rep := w.sync(); rep.State != StateBootstrapReview {
		t.Fatalf("sigue en revisión: %+v", rep)
	}

	rep = w.sync(SyncOptions{AcceptBootstrap: true})
	if rep.State != StateIdle || rep.MergeKind != merge.FastForwardMain {
		t.Fatalf("aceptado: %+v", rep)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main tiene que quedar igual a box")
	}
}

func TestBootstrapResolvedByMergingLocally(t *testing.T) {
	w := newWorld(t)
	w.devPush(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Repo\n"})
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Box\n")
	if rep := w.sync(); rep.State != StateBootstrapReview {
		t.Fatalf("= %+v", rep)
	}
	// El usuario revisa box y lo mergea en main, quedándose con el nombre del repo.
	w.devResolve(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Repo\n"})
	rep := w.sync()
	if rep.State != StateIdle || !rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/configuration.yaml"]; got != "homeassistant:\n  name: Repo\n" {
		t.Errorf("disco = %q", got)
	}
	if !rep.Restart {
		t.Error("cambiar configuration.yaml recomienda restart")
	}
}

func TestPushRaceRetries(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.opts.hooks.beforePush = func(attempt int) {
		if attempt == 1 {
			w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [colado]\n"})
		}
	}
	w.newEngine()
	w.edit("config/automations.yaml", "- id: '1'\n  alias: box\n")

	rep := w.sync()
	if rep.State != StateIdle || rep.Attempts != 2 {
		t.Fatalf("= %+v", rep)
	}
	m := w.remoteFiles("main")
	if m["config/scripts.yaml"] != "hola:\n  sequence: [colado]\n" || m["config/automations.yaml"] != "- id: '1'\n  alias: box\n" {
		t.Errorf("main perdió algo: %v", m)
	}
	if w.disk()["config/scripts.yaml"] != "hola:\n  sequence: [colado]\n" {
		t.Error("el cambio colado tiene que llegar al box")
	}
}

func TestEditDuringApplyRetries(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [main]\n"})
	w.opts.hooks.beforeApply = func(attempt int) {
		if attempt == 1 {
			w.edit("config/scripts.yaml", "hola:\n  sequence: [editado en la UI]\n")
		}
	}
	w.newEngine()
	rep := w.sync()
	// El segundo intento ve la edición, que choca con main: conflicto, sin
	// haber pisado nada.
	if rep.Attempts != 2 || rep.State != StateConflict {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/scripts.yaml"]; got != "hola:\n  sequence: [editado en la UI]\n" {
		t.Errorf("se pisó la edición: %q", got)
	}
}

func TestCrashAfterApplyIsRecoveredOnNextSync(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [main]\n"})
	w.opts.hooks.afterApply = func() error { return errors.New("se cortó la luz") }
	w.newEngine()
	if _, err := w.eng.Sync(ctx, SyncOptions{}); err == nil {
		t.Fatal("quería el corte simulado")
	}
	w.opts.hooks.afterApply = nil
	w.newEngine()
	rep := w.sync()
	if rep.Recovered != "at_target" || rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("box") != w.remote("main") || w.disk()["config/scripts.yaml"] != "hola:\n  sequence: [main]\n" {
		t.Error("después de recuperar, box tiene que quedar en main")
	}
}

func TestPushOnlyNeverAppliesMain(t *testing.T) {
	w := newWorld(t, func(c *config.Config) { c.Mode = config.ModePushOnly })
	w.edit("config/automations.yaml", "[]\n")
	w.sync()
	if w.remote("box") == "" || w.remote("main") != "" {
		t.Fatalf("push_only solo publica box: main=%s box=%s", w.remote("main"), w.remote("box"))
	}
	w.devPush(map[string]string{"config/automations.yaml": "- desde main\n"})
	w.edit("config/scripts.yaml", "a: {}\n")
	if rep := w.sync(); rep.State != StateIdle || rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "[]\n" {
		t.Errorf("push_only nunca aplica main: %q", got)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [main]\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: local\n")
	main, box, disk := w.remote("main"), w.remote("box"), w.disk()
	stBefore, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "state.json"))

	rep := w.sync(SyncOptions{DryRun: true})
	if !rep.DryRun || len(rep.Changes) != 1 || rep.Plan == nil || len(rep.Plan.Ops) != 1 {
		t.Fatalf("= %+v", rep)
	}
	stAfter, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "state.json"))
	if w.remote("main") != main || w.remote("box") != box || !slices.Equal(stBefore, stAfter) {
		t.Error("dry-run no puede mover refs ni el estado")
	}
	assertFiles(t, w.disk(), disk)
}

func TestConcurrentSyncIsBusy(t *testing.T) {
	w := newWorld(t)
	unlock, err := lock(filepath.Join(w.opts.StateDir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := w.eng.Sync(ctx, SyncOptions{}); !errors.Is(err, ErrBusy) {
		t.Errorf("quería ErrBusy, dio %v", err)
	}
}

func TestSomeonePushedToBox(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	_, box := w.devFetch()
	c := testkit.Commit(t, w.dev, box, map[string]string{"config/x.yaml": "x: 1\n"})
	if err := w.dev.Push(ctx, w.url, c+":refs/heads/box"); err != nil {
		t.Fatal(err)
	}
	// Alguien (un humano u otro box mal configurado) pusheó a box: el remoto
	// tiene un commit que la rama local no tiene, y el motor no puede seguir
	// sin pisarlo.
	w.edit("config/automations.yaml", "[]\n")
	if _, err := w.eng.Sync(ctx, SyncOptions{}); !errors.Is(err, ErrBoxDiverged) {
		t.Errorf("quería ErrBoxDiverged, dio %v", err)
	}
	if got := w.remote("box"); got != c {
		t.Error("el commit ajeno en box no se puede pisar")
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.State != StateError || !strings.Contains(st.LastError, "box") {
		t.Errorf("quería error por box divergente, estado %s: %s", st.State, st.LastError)
	}
}

// TestCycleAgainstSupervisorAPI corre un ciclo con el cliente real del
// Supervisor contra un Core falso (httptest) y verifica las llamadas.
func TestCycleAgainstSupervisorAPI(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/core/api/config/core/check_config" {
			_, _ = w.Write([]byte(`{"result": "valid", "errors": null}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	w := newWorld(t)
	sup := &ha.Supervisor{BaseURL: srv.URL + "/core/api", Token: "tok", HTTP: srv.Client()}
	w.opts.Checker, w.opts.Reloader, w.opts.Notifier = sup, sup, sup
	w.newEngine()
	w.seedBox()
	mu.Lock()
	calls = nil
	mu.Unlock()

	w.devPush(map[string]string{"config/scripts.yaml": "hola:\n  sequence: [main]\n"})
	if rep := w.sync(); rep.State != StateIdle || !rep.Reloaded {
		t.Fatalf("= %+v", rep)
	}
	want := []string{
		"/core/api/config/core/check_config",
		"/core/api/services/homeassistant/reload_all",
		"/core/api/events/homeassistant_config_sync",
		"/core/api/services/persistent_notification/dismiss",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("llamadas a HA = %v\nquería %v", calls, want)
	}
}

func TestNotInitialized(t *testing.T) {
	cfg := config.Default()
	cfg.Repository = "file:///nope"
	e, err := New(Options{Config: cfg, StateDir: t.TempDir(), AllowFileRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, SyncOptions{}); !errors.Is(err, ErrNotInitialized) {
		t.Errorf("= %v", err)
	}
}

func TestInitIsIdempotentAndWritesKeys(t *testing.T) {
	w := newWorld(t)
	pub1, err := DeployKey(w.opts.StateDir)
	if err != nil || !strings.HasPrefix(pub1, "ssh-ed25519 ") || !strings.HasSuffix(pub1, " homeassistant-config-sync") {
		t.Fatalf("deploy key = %q, %v", pub1, err)
	}
	res, err := w.eng.Init(ctx)
	if err != nil || res.Created || res.DeployKey != pub1 {
		t.Errorf("init de nuevo no puede rotar la clave: %+v %v", res, err)
	}
	fi, err := os.Stat(filepath.Join(w.opts.StateDir, "ssh", "id_ed25519"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("permisos de la clave privada: %v %v", fi.Mode(), err)
	}
	kh, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "ssh", "known_hosts"))
	if !strings.Contains(string(kh), "github.com ssh-ed25519 ") {
		t.Error("known_hosts sin GitHub")
	}
	tok, err := APIToken(w.opts.StateDir)
	if err != nil || len(tok) != 64 {
		t.Errorf("api_token = %q, %v", tok, err)
	}
	if _, err := w.eng.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if tok2, _ := APIToken(w.opts.StateDir); tok2 != tok {
		t.Error("init de nuevo no puede rotar el token")
	}
}
