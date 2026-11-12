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

// world is a complete, self-contained scenario: a fake box, a "GitHub" that
// is a local bare repo (file://), a user working "locally" who commits and
// pushes to main, and a fake Home Assistant.
type world struct {
	t      *testing.T
	ha     string
	addons string
	url    string
	origin *gitx.Repo
	dev    *gitx.Repo
	eng    *Engine
	opts   Options

	checks   int
	verdict  ha.CheckResult
	reloads  int
	restarts int
	events   []ha.Event
	// broken is what UnavailableAutomations returns; brokenAfterReload, if
	// set, replaces it after the next reload (simulates a bad automation).
	broken            []string
	brokenAfterReload []string
}

type fakeHA struct{ w *world }

func (f fakeHA) Restart(context.Context) error { f.w.restarts++; return nil }
func (f fakeHA) UnavailableAutomations(context.Context) ([]string, error) {
	return f.w.broken, nil
}

func (f fakeHA) CheckConfig(context.Context) (ha.CheckResult, error) {
	f.w.checks++
	return f.w.verdict, nil
}
func (f fakeHA) ReloadAll(context.Context) error {
	f.w.reloads++
	if f.w.brokenAfterReload != nil {
		// The first reload breaks automations; the second (after the undo)
		// brings them back.
		f.w.broken, f.w.brokenAfterReload = f.w.brokenAfterReload, nil
	} else if f.w.reloads > 1 {
		f.w.broken = nil
	}
	return nil
}
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
	w.opts = Options{Config: cfg, StateDir: filepath.Join(root, "state"), AllowFileRemote: true,
		Checker: f, Reloader: f, Notifier: f, Restarter: f, States: f}
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

// boxPath maps config/… and app_configs/… to the box's disk.
func (w *world) boxPath(p string) string {
	if rel, ok := strings.CutPrefix(p, "config/"); ok {
		return filepath.Join(w.ha, rel)
	}
	if rel, ok := strings.CutPrefix(p, "app_configs/"); ok {
		return filepath.Join(w.addons, rel)
	}
	w.t.Fatalf("path outside the mappings: %s", p)
	return ""
}

// edit simulates an edit on the box (HA UI, file editor…).
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

// devFetch fetches main and box from the remote into the local user's repo.
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

// devPush commits on top of origin/main and pushes, like a local user.
func (w *world) devPush(changes map[string]string) string {
	w.t.Helper()
	main, _ := w.devFetch()
	c := testkit.Commit(w.t, w.dev, main, changes)
	if err := w.dev.Push(ctx, w.url, c+":refs/heads/main"); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// devResolve does what a local user would do: git merge origin/box into
// main, overwrite the conflicting paths with the given resolution and push.
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
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected %s", p)
		}
	}
}

// seedBox sets up a typical box and syncs it against an empty remote.
func (w *world) seedBox() {
	w.t.Helper()
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Home\n")
	w.edit("config/automations.yaml", "- id: '1'\n  alias: one\n")
	w.edit("config/scripts.yaml", "hello:\n  sequence: []\n")
	w.edit("config/secrets.yaml", "pw: never-leaves\n")
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
		"config/configuration.yaml": "homeassistant:\n  name: Home\n",
		"config/automations.yaml":   "- id: '1'\n  alias: one\n",
		"config/scripts.yaml":       "hello:\n  sequence: []\n",
	})
	if w.checks != 0 {
		t.Error("there was nothing to apply: check_config should not run")
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.MainHead != main || st.BoxHead != box {
		t.Errorf("state: main %s box %s, want %s", st.MainHead, st.BoxHead, main)
	}
}

func TestSecondSyncIsANoop(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	before, events := w.remote("main"), len(w.events)
	rep := w.sync()
	if rep.SnapshotCommit != "" || len(rep.Pushed) != 0 || rep.Applied {
		t.Errorf("a sync without changes should do nothing: %+v", rep)
	}
	if w.remote("main") != before || len(w.events) != events {
		t.Error("main moved or an event was emitted without changes")
	}
}

func TestBoxChangeReachesMain(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.edit("config/automations.yaml", "- id: '1'\n  alias: edited in the UI\n")
	rep := w.sync()
	if rep.MergeKind != merge.FastForwardMain || rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	if got := w.remoteFiles("main")["config/automations.yaml"]; got != "- id: '1'\n  alias: edited in the UI\n" {
		t.Errorf("main/automations = %q", got)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main and box should match")
	}
	if w.lastEvent().Type != ha.EventSyncFinished {
		t.Errorf("event = %+v", w.lastEvent())
	}
}

func TestMainChangeIsAppliedCheckedAndReloaded(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence:\n    - delay: 1\n", "README.md": "# home\n"})
	rep := w.sync()
	if rep.MergeKind != merge.FastForwardBox || !rep.Applied || !rep.Checked || !rep.Reloaded {
		t.Fatalf("= %+v", rep)
	}
	// Two checks: one before writing (baseline warnings) and one after.
	if w.checks != 2 || w.reloads != 1 {
		t.Errorf("checks = %d, reloads = %d", w.checks, w.reloads)
	}
	if got := w.disk()["config/scripts.yaml"]; got != "hello:\n  sequence:\n    - delay: 1\n" {
		t.Errorf("disk = %q", got)
	}
	if _, ok := w.disk()["config/README.md"]; ok {
		t.Error("the README belongs to the repo: it does not go down to the box")
	}
	if w.remote("box") != w.remote("main") {
		t.Error("box should advance to main")
	}
}

func TestBothSidesMergeCleanly(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: box\n")
	rep := w.sync()
	if rep.MergeKind != merge.Merged || !rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	d := w.disk()
	if d["config/scripts.yaml"] != "hello:\n  sequence: [main]\n" || d["config/automations.yaml"] != "- id: '1'\n  alias: box\n" {
		t.Errorf("disk = %v", d)
	}
	m := w.remoteFiles("main")
	if m["config/scripts.yaml"] != d["config/scripts.yaml"] || m["config/automations.yaml"] != d["config/automations.yaml"] {
		t.Errorf("main = %v", m)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main and box should match")
	}
}

func TestConflictStopsKeepsBoxAndResolves(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	mainBefore := w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: from main\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: from the UI\n")

	rep := w.sync()
	if rep.State != StateConflict || !slices.Equal(rep.ConflictPaths, []string{"config/automations.yaml"}) {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != mainBefore {
		t.Error("main is not touched on a conflict")
	}
	if got := w.remoteFiles("box")["config/automations.yaml"]; got != "- id: '1'\n  alias: from the UI\n" {
		t.Errorf("box must have the box's edit: %q", got)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: from the UI\n" {
		t.Errorf("the disk is not touched on a conflict: %q", got)
	}
	if ev := w.lastEvent(); ev.Type != ha.EventConflict || !ev.Persistent || !strings.Contains(ev.Message, "config/automations.yaml") {
		t.Errorf("event = %+v", ev)
	}

	// While there is a conflict, the box keeps being saved.
	w.edit("config/scripts.yaml", "hello:\n  sequence: [during the conflict]\n")
	if rep := w.sync(); rep.State != StateConflict {
		t.Fatalf("still in conflict: %+v", rep)
	}
	if got := w.remoteFiles("box")["config/scripts.yaml"]; got != "hello:\n  sequence: [during the conflict]\n" {
		t.Errorf("box did not get the edit made during the conflict: %q", got)
	}

	// The user resolves locally and the next cycle comes out clean.
	w.devResolve(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: resolved\n"})
	rep = w.sync()
	if rep.State != StateIdle {
		t.Fatalf("after resolving: %+v", rep)
	}
	d := w.disk()
	if d["config/automations.yaml"] != "- id: '1'\n  alias: resolved\n" || d["config/scripts.yaml"] != "hello:\n  sequence: [during the conflict]\n" {
		t.Errorf("disk = %v", d)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main and box should match")
	}
}

func TestRejectedChangeIsNotRetriedUntilMainChanges(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	mainBad := w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  trigger: invalid\n"})
	w.verdict = ha.CheckResult{Valid: false, Errors: "Invalid config for [automation]: trigger"}

	rep := w.sync()
	if rep.State != StateRejected || !strings.Contains(rep.Error, "Invalid config") {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: one\n" {
		t.Errorf("rollback: %q", got)
	}
	if w.remote("main") != mainBad || w.reloads != 0 {
		t.Error("main is not touched and there is no reload")
	}

	// Same main: not retried.
	if rep := w.sync(); rep.State != StateRejected || w.checks != 2 {
		t.Errorf("should not retry: %+v, checks = %d", rep, w.checks)
	}

	// The user fixes main: it is applied.
	w.verdict = ha.CheckResult{Valid: true}
	w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: fixed\n"})
	if rep := w.sync(); rep.State != StateIdle || !rep.Applied {
		t.Fatalf("after the fix: %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: fixed\n" {
		t.Errorf("disk = %q", got)
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
		t.Error("nothing is committed or pushed")
	}
	ev := w.lastEvent()
	if ev.Type != ha.EventBlockedSecrets || strings.Contains(ev.Message, secret) || strings.Contains(rep.Error, secret) {
		t.Errorf("the event must not include the value: %+v", ev)
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.State != StateBlockedSecrets {
		t.Errorf("persisted state = %s", st.State)
	}

	w.edit("config/rest.yaml", "sensor:\n  api_key: !secret rest_api_key\n")
	if rep := w.sync(); rep.State != StateIdle {
		t.Fatalf("after fixing it: %+v", rep)
	}
	if got := w.remoteFiles("main")["config/rest.yaml"]; !strings.Contains(got, "!secret") {
		t.Errorf("main/rest.yaml = %q", got)
	}
}

func TestRepeatedProblemIsNotifiedOnce(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: from main\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: from the UI\n")

	before := len(w.events)
	for range 3 {
		if rep := w.sync(); rep.State != StateConflict {
			t.Fatalf("= %+v", rep)
		}
	}
	if got := len(w.events) - before; got != 1 {
		t.Errorf("three cycles with the same conflict sent %d notifications, want 1", got)
	}

	// A different conflict (another path) is notified.
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	w.edit("config/scripts.yaml", "hello:\n  sequence: [ui]\n")
	w.sync()
	if got := len(w.events) - before; got != 2 {
		t.Errorf("a new conflict should have been notified: %d notifications", got)
	}

	// Once resolved it returns to idle, and the same problem is notified again.
	w.devResolve(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: r\n", "config/scripts.yaml": "hello:\n  sequence: [r]\n"})
	if rep := w.sync(); rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.LastNotified != "" {
		t.Error("returning to idle resets it")
	}
}

func TestPullOnlyFollowsMain(t *testing.T) {
	w := newWorld(t, func(c *config.Config) { c.Mode = config.ModePullOnly })
	// pull_only on an empty remote: there is no main to follow yet; the box
	// is published to box only.
	w.edit("config/automations.yaml", "[]\n")
	w.edit("config/scripts.yaml", "a: {}\n")
	if rep := w.sync(); rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != "" {
		t.Fatal("pull_only never writes main")
	}

	// main is created by someone else, and the box edits a file locally.
	w.devPush(map[string]string{"config/automations.yaml": "- id: main\n", "config/scripts.yaml": "a: {}\n"})
	mainBefore := w.remote("main")
	w.edit("config/scripts.yaml", "a: {edited: locally}\n")
	rep := w.sync()
	if rep.State != StateIdle || rep.MergeKind != merge.FollowMain || !rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	d := w.disk()
	if d["config/automations.yaml"] != "- id: main\n" || d["config/scripts.yaml"] != "a: {}\n" {
		t.Errorf("the box must follow main, local edit discarded: %v", d)
	}
	if w.remote("main") != mainBefore {
		t.Error("pull_only never writes main")
	}
	// The discarded edit is kept in the box branch history.
	log, err := w.origin.LsTree(ctx, w.remote("box")+"^1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range log {
		if e.Path == "config/scripts.yaml" {
			b, _ := w.origin.ReadBlob(ctx, e.OID)
			found = string(b) == "a: {edited: locally}\n"
		}
	}
	if !found {
		t.Error("the local edit must stay in the history of the box branch")
	}
	// Idempotent.
	if rep := w.sync(); rep.SnapshotCommit != "" || len(rep.Pushed) != 0 {
		t.Errorf("second sync should do nothing: %+v", rep)
	}
}

func TestRestartAuto(t *testing.T) {
	w := newWorld(t, func(c *config.Config) { c.Restart = config.RestartAuto })
	w.seedBox()
	w.devPush(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Other\n"})
	rep := w.sync()
	if !rep.Restarted || w.restarts != 1 || rep.Restart {
		t.Fatalf("= %+v, restarts = %d", rep, w.restarts)
	}
	if !strings.Contains(w.lastEvent().Message, "restarting") {
		t.Errorf("event = %+v", w.lastEvent())
	}
	// A change that does not need a restart does not restart.
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [x]\n"})
	if rep := w.sync(); rep.Restarted || w.restarts != 1 {
		t.Errorf("= %+v, restarts = %d", rep, w.restarts)
	}
}

func TestRestartNeverIsTheDefault(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Other\n"})
	rep := w.sync()
	if rep.Restarted || w.restarts != 0 || !rep.Restart {
		t.Fatalf("= %+v, restarts = %d", rep, w.restarts)
	}
}

func TestBrokenAutomationIsUndone(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.broken = []string{"automation.already_broken"}
	before := w.remote("box")
	mainBad := w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  triggers: [{trigger: nope}]\n"})
	w.brokenAfterReload = []string{"automation.already_broken", "automation.uno"}

	rep := w.sync()
	if rep.State != StateRejected || !slices.Equal(rep.BrokenAutomations, []string{"automation.uno"}) {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "- id: '1'\n  alias: one\n" {
		t.Errorf("the file must be rolled back: %q", got)
	}
	if w.reloads != 2 {
		t.Errorf("reloads = %d, want 2 (apply + undo)", w.reloads)
	}
	if w.remote("box") != before || w.remote("main") != mainBad {
		t.Error("nothing is published after an undo")
	}
	if ev := w.lastEvent(); ev.Type != ha.EventRejected || !strings.Contains(ev.Message, "automation.uno") {
		t.Errorf("event = %+v", ev)
	}
	// Same main again: not retried.
	if rep := w.sync(); rep.State != StateRejected || w.reloads != 2 {
		t.Errorf("should not retry: %+v, reloads = %d", rep, w.reloads)
	}
}

func TestDryRunMergesUncommittedBoxChanges(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/automations.yaml": "- id: '1'\n  alias: from main\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: from the UI\n")
	rep := w.sync(SyncOptions{DryRun: true})
	if rep.MergeKind != merge.Conflict || !slices.Equal(rep.ConflictPaths, []string{"config/automations.yaml"}) {
		t.Fatalf("the plan must see the conflict the real sync would hit: %+v", rep)
	}
	if b, _ := w.eng.repo.ResolveRef(ctx, w.eng.boxRef); b == rep.SnapshotCommit {
		t.Error("dry-run must not move the box branch")
	}
}

func TestRotateDeployKey(t *testing.T) {
	w := newWorld(t)
	old, _ := DeployKey(w.opts.StateDir)
	pub, err := RotateDeployKey(w.opts.StateDir)
	if err != nil || pub == old || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("= %q, %v", pub, err)
	}
	if cur, _ := DeployKey(w.opts.StateDir); cur != pub {
		t.Error("DeployKey must return the new key")
	}
	if b, err := os.ReadFile(filepath.Join(w.opts.StateDir, "ssh", "id_ed25519.old.pub")); err != nil || strings.TrimSpace(string(b)) != old {
		t.Errorf("the old key is kept as .old: %v", err)
	}
	// Busy while a sync holds the lock.
	unlock, err := lock(filepath.Join(w.opts.StateDir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := RotateDeployKey(w.opts.StateDir); !errors.Is(err, ErrBusy) {
		t.Errorf("= %v, want ErrBusy", err)
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
		t.Error("nothing is pushed")
	}
}

func TestBootstrapOnExistingMain(t *testing.T) {
	w := newWorld(t)
	// The repo already exists with a config different from the box's.
	w.devPush(map[string]string{
		"README.md":                 "# home\n",
		"config/configuration.yaml": "homeassistant:\n  name: Repo\n",
		"config/old.yaml":           "v: 1\n",
	})
	mainBefore := w.remote("main")
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Box\n")
	w.edit("config/automations.yaml", "[]\n")

	rep := w.sync()
	if rep.State != StateBootstrapReview {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("main") != mainBefore {
		t.Error("main is not touched until reviewed")
	}
	box := w.remoteFiles("box")
	assertFiles(t, box, map[string]string{
		"README.md":                 "# home\n",
		"config/configuration.yaml": "homeassistant:\n  name: Box\n",
		"config/automations.yaml":   "[]\n",
	})
	if w.checks != 0 || w.disk()["config/configuration.yaml"] != "homeassistant:\n  name: Box\n" {
		t.Error("the disk is not touched")
	}
	if rep := w.sync(); rep.State != StateBootstrapReview {
		t.Fatalf("still in review: %+v", rep)
	}

	rep = w.sync(SyncOptions{AcceptBootstrap: true})
	if rep.State != StateIdle || rep.MergeKind != merge.FastForwardMain {
		t.Fatalf("accepted: %+v", rep)
	}
	if w.remote("main") != w.remote("box") {
		t.Error("main must end up equal to box")
	}
}

func TestBootstrapResolvedByMergingLocally(t *testing.T) {
	w := newWorld(t)
	w.devPush(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Repo\n"})
	w.edit("config/configuration.yaml", "homeassistant:\n  name: Box\n")
	if rep := w.sync(); rep.State != StateBootstrapReview {
		t.Fatalf("= %+v", rep)
	}
	// The user reviews box and merges it into main, keeping the repo's name.
	w.devResolve(map[string]string{"config/configuration.yaml": "homeassistant:\n  name: Repo\n"})
	rep := w.sync()
	if rep.State != StateIdle || !rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/configuration.yaml"]; got != "homeassistant:\n  name: Repo\n" {
		t.Errorf("disk = %q", got)
	}
	if !rep.Restart {
		t.Error("changing configuration.yaml recommends a restart")
	}
}

func TestPushRaceRetries(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.opts.hooks.beforePush = func(attempt int) {
		if attempt == 1 {
			w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [sneaked-in]\n"})
		}
	}
	w.newEngine()
	w.edit("config/automations.yaml", "- id: '1'\n  alias: box\n")

	rep := w.sync()
	if rep.State != StateIdle || rep.Attempts != 2 {
		t.Fatalf("= %+v", rep)
	}
	m := w.remoteFiles("main")
	if m["config/scripts.yaml"] != "hello:\n  sequence: [sneaked-in]\n" || m["config/automations.yaml"] != "- id: '1'\n  alias: box\n" {
		t.Errorf("main lost something: %v", m)
	}
	if w.disk()["config/scripts.yaml"] != "hello:\n  sequence: [sneaked-in]\n" {
		t.Error("the sneaked-in change must reach the box")
	}
}

func TestEditDuringApplyRetries(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	w.opts.hooks.beforeApply = func(attempt int) {
		if attempt == 1 {
			w.edit("config/scripts.yaml", "hello:\n  sequence: [edited in the UI]\n")
		}
	}
	w.newEngine()
	rep := w.sync()
	// The second attempt sees the edit, which clashes with main: conflict,
	// without having overwritten anything.
	if rep.Attempts != 2 || rep.State != StateConflict {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/scripts.yaml"]; got != "hello:\n  sequence: [edited in the UI]\n" {
		t.Errorf("the edit was overwritten: %q", got)
	}
}

func TestCrashAfterApplyIsRecoveredOnNextSync(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	w.opts.hooks.afterApply = func() error { return errors.New("power outage") }
	w.newEngine()
	if _, err := w.eng.Sync(ctx, SyncOptions{}); err == nil {
		t.Fatal("wanted the simulated crash")
	}
	w.opts.hooks.afterApply = nil
	w.newEngine()
	rep := w.sync()
	if rep.Recovered != "at_target" || rep.State != StateIdle {
		t.Fatalf("= %+v", rep)
	}
	if w.remote("box") != w.remote("main") || w.disk()["config/scripts.yaml"] != "hello:\n  sequence: [main]\n" {
		t.Error("after recovering, box must end up at main")
	}
}

func TestPushOnlyNeverAppliesMain(t *testing.T) {
	w := newWorld(t, func(c *config.Config) { c.Mode = config.ModePushOnly })
	w.edit("config/automations.yaml", "[]\n")
	w.sync()
	if w.remote("box") == "" || w.remote("main") != "" {
		t.Fatalf("push_only only publishes box: main=%s box=%s", w.remote("main"), w.remote("box"))
	}
	w.devPush(map[string]string{"config/automations.yaml": "- from main\n"})
	w.edit("config/scripts.yaml", "a: {}\n")
	if rep := w.sync(); rep.State != StateIdle || rep.Applied {
		t.Fatalf("= %+v", rep)
	}
	if got := w.disk()["config/automations.yaml"]; got != "[]\n" {
		t.Errorf("push_only never applies main: %q", got)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.seedBox()
	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	w.edit("config/automations.yaml", "- id: '1'\n  alias: local\n")
	main, box, disk := w.remote("main"), w.remote("box"), w.disk()
	stBefore, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "state.json"))

	rep := w.sync(SyncOptions{DryRun: true})
	if !rep.DryRun || len(rep.Changes) != 1 || rep.Plan == nil || len(rep.Plan.Ops) != 1 {
		t.Fatalf("= %+v", rep)
	}
	stAfter, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "state.json"))
	if w.remote("main") != main || w.remote("box") != box || !slices.Equal(stBefore, stAfter) {
		t.Error("dry-run must not move refs or the state")
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
		t.Errorf("want ErrBusy, got %v", err)
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
	// Someone (a human or another misconfigured box) pushed to box: the remote
	// has a commit the local branch lacks, and the engine cannot continue
	// without overwriting it.
	w.edit("config/automations.yaml", "[]\n")
	if _, err := w.eng.Sync(ctx, SyncOptions{}); !errors.Is(err, ErrBoxDiverged) {
		t.Errorf("want ErrBoxDiverged, got %v", err)
	}
	if got := w.remote("box"); got != c {
		t.Error("the foreign commit on box must not be overwritten")
	}
	st, _ := LoadState(w.opts.StateDir)
	if st.State != StateError || !strings.Contains(st.LastError, "box") {
		t.Errorf("want an error for diverged box, state %s: %s", st.State, st.LastError)
	}
}

// TestCycleAgainstSupervisorAPI runs a cycle with the real Supervisor client
// against a fake Core (httptest) and checks the calls.
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

	w.devPush(map[string]string{"config/scripts.yaml": "hello:\n  sequence: [main]\n"})
	if rep := w.sync(); rep.State != StateIdle || !rep.Reloaded {
		t.Fatalf("= %+v", rep)
	}
	want := []string{
		"/core/api/config/core/check_config", // baseline warnings
		"/core/api/config/core/check_config",
		"/core/api/services/homeassistant/reload_all",
		"/core/api/events/homeassistant_config_sync",
		"/core/api/services/persistent_notification/dismiss",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls to HA = %v\nwant %v", calls, want)
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
		t.Errorf("a second init must not rotate the key: %+v %v", res, err)
	}
	fi, err := os.Stat(filepath.Join(w.opts.StateDir, "ssh", "id_ed25519"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("private key permissions: %v %v", fi.Mode(), err)
	}
	kh, _ := os.ReadFile(filepath.Join(w.opts.StateDir, "ssh", "known_hosts"))
	if !strings.Contains(string(kh), "github.com ssh-ed25519 ") {
		t.Error("known_hosts without GitHub")
	}
	tok, err := APIToken(w.opts.StateDir)
	if err != nil || len(tok) != 64 {
		t.Errorf("api_token = %q, %v", tok, err)
	}
	if _, err := w.eng.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if tok2, _ := APIToken(w.opts.StateDir); tok2 != tok {
		t.Error("a second init must not rotate the token")
	}
}
