package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

// fakeHA records what the engine asks Home Assistant.
type fakeHA struct {
	checks, reloads int
	result          ha.CheckResult
	events          []ha.Event
}

func (f *fakeHA) CheckConfig(context.Context) (ha.CheckResult, error) {
	f.checks++
	return f.result, nil
}
func (f *fakeHA) ReloadAll(context.Context) error { f.reloads++; return nil }
func (f *fakeHA) Notify(_ context.Context, ev ha.Event) error {
	f.events = append(f.events, ev)
	return nil
}

type env struct {
	t      *testing.T
	fs     string // the app's filesystem
	remote *gitx.Repo
	cfg    *config.Config
	ha     *fakeHA
	hooks  hooks
	state  string
}

// newEnv builds a sandbox: a filesystem with the mounts, an empty remote,
// and an engine with root /.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, fs: t.TempDir(), remote: testkit.NewBareRepo(t), ha: &fakeHA{result: ha.CheckResult{Valid: true}}, state: t.TempDir()}
	for _, m := range Mounts {
		if err := os.MkdirAll(filepath.Join(e.fs, m), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = config.Default()
	e.cfg.Repository = "file://" + e.remote.Dir()
	return e
}

func (e *env) engine() *Engine {
	e.t.Helper()
	eng, err := New(Options{
		Config: e.cfg, StateDir: e.state, FSRoot: e.fs, AllowFileRemote: true,
		Checker: e.ha, Reloader: e.ha, Notifier: e.ha, hooks: e.hooks,
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := eng.Init(ctx); err != nil {
		e.t.Fatal(err)
	}
	return eng
}

func (e *env) sync() *Report {
	e.t.Helper()
	rep, err := e.engine().Sync(ctx, SyncOptions{})
	if err != nil {
		e.t.Fatalf("sync: %v (report %+v)", err, rep)
	}
	return rep
}

func (e *env) write(p, content string) { testkit.WriteFile(e.t, filepath.Join(e.fs, p), content) }
func (e *env) remove(p string)         { testkit.Remove(e.t, filepath.Join(e.fs, p)) }
func (e *env) read(p string) string {
	b, err := os.ReadFile(filepath.Join(e.fs, p))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}
func (e *env) branch(name string) map[string]string {
	return testkit.Tree(e.t, e.remote, "refs/heads/"+name)
}

// pushMain commits changes on top of the remote's main, as a person would.
func (e *env) pushMain(changes map[string]string) {
	e.t.Helper()
	parent, _ := e.remote.ResolveRef(ctx, "refs/heads/main")
	c := testkit.Commit(e.t, e.remote, parent, changes)
	if err := e.remote.UpdateRef(ctx, "refs/heads/main", c, parent); err != nil {
		e.t.Fatal(err)
	}
}

// mergeBoxIntoMain merges box into main with git, as a person would.
func (e *env) mergeBoxIntoMain() {
	e.t.Helper()
	box, _ := e.remote.ResolveRef(ctx, "refs/heads/box")
	main, _ := e.remote.ResolveRef(ctx, "refs/heads/main")
	if main == "" {
		if err := e.remote.UpdateRef(ctx, "refs/heads/main", box, ""); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if ok, _ := e.remote.IsAncestor(ctx, main, box); ok {
		if err := e.remote.UpdateRef(ctx, "refs/heads/main", box, main); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	mt, err := e.remote.MergeTree(ctx, main, box, []string{"theirs"}, "")
	if err != nil || !mt.Clean {
		e.t.Fatalf("merge: %+v %v", mt, err)
	}
	id, _ := gitx.ParseIdent("Person <p@example.invalid>", time.Unix(1700000500, 0))
	c, err := e.remote.CommitTree(ctx, mt.Tree, []string{main, box}, "merge box\n", id, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.remote.UpdateRef(ctx, "refs/heads/main", c, main); err != nil {
		e.t.Fatal(err)
	}
}

// firstSync runs the first sync, which waits for the user, merges box into
// main as a person would, and runs the cycle that resumes syncing.
func (e *env) firstSync() *Report {
	e.t.Helper()
	rep := e.sync()
	wantState(e.t, rep, StateReview)
	if rep.Review != ReviewFirstSync {
		e.t.Fatalf("review = %s, want %s", rep.Review, ReviewFirstSync)
	}
	e.mergeBoxIntoMain()
	rep = e.sync()
	wantState(e.t, rep, StateIdle)
	return rep
}

func wantState(t *testing.T, rep *Report, state string) {
	t.Helper()
	if rep.State != state {
		t.Fatalf("state = %s, want %s (report %+v)", rep.State, state, rep)
	}
}

func TestFirstSyncToEmptyRepoWaitsForTheUserWithBaseGitignore(t *testing.T) {
	e := newEnv(t)
	e.write("homeassistant/configuration.yaml", "homeassistant:\n")
	e.write("homeassistant/.storage/auth", "tokens")
	e.write("homeassistant/home-assistant_v2.db", "db")
	e.write("homeassistant/secrets.yaml", "pw: x\n")
	e.write("app_configs/a0d7b954_appdaemon/apps.yaml", "app: 1\n")
	e.write("share/notes.txt", "hello\n")
	e.write("ssl/privkey.pem", "key")
	e.write("backup/full.tar", "tar")

	// Even with an empty repo, only box is pushed: main is the user's call.
	rep := e.sync()
	wantState(t, rep, StateReview)
	if rep.Review != ReviewFirstSync {
		t.Errorf("review = %s", rep.Review)
	}
	if len(e.branch("main")) != 0 {
		t.Error("main must not be created by the app")
	}
	box := e.branch("box")
	for _, p := range []string{"homeassistant/configuration.yaml", "app_configs/a0d7b954_appdaemon/apps.yaml", "share/notes.txt", ".gitignore"} {
		if _, ok := box[p]; !ok {
			t.Errorf("%s missing from box", p)
		}
	}
	for p := range box {
		for _, bad := range []string{".storage", ".db", "secrets.yaml", "ssl/", "backup/"} {
			if strings.Contains(p, bad) {
				t.Errorf("%s should be ignored by the base .gitignore", p)
			}
		}
	}
	wantState(t, e.sync(), StateReview)

	// The user creates main from box; syncing resumes.
	e.mergeBoxIntoMain()
	wantState(t, e.sync(), StateIdle)
	if !maps(e.branch("box"), e.branch("main")) {
		t.Error("box and main differ after the first sync")
	}
	if e.ha.checks != 0 || e.ha.reloads != 0 {
		t.Error("nothing came from main: Home Assistant should not be checked or reloaded")
	}
	// Nothing changed: the next cycle does nothing.
	if rep := e.sync(); rep.SnapshotCommit != "" || len(rep.Pushed) != 0 {
		t.Errorf("idempotency: %+v", rep)
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestBoxAndMainChangesFlowBothWays(t *testing.T) {
	e := newEnv(t)
	e.write("homeassistant/automations.yaml", "[]\n")
	e.write("homeassistant/scripts.yaml", "{}\n")
	e.firstSync()

	e.write("homeassistant/scripts.yaml", "s: 1\n")
	e.pushMain(map[string]string{"homeassistant/automations.yaml": "- id: '1'\n"})
	rep := e.sync()
	wantState(t, rep, StateIdle)
	if e.read("homeassistant/automations.yaml") != "- id: '1'\n" {
		t.Error("main's change did not reach the box")
	}
	if e.branch("main")["homeassistant/scripts.yaml"] != "s: 1\n" {
		t.Error("the box's change did not reach main")
	}
	if !rep.Applied || !rep.Checked || !rep.Reloaded || e.ha.checks != 1 || e.ha.reloads != 1 {
		t.Errorf("check/reload: %+v, ha %+v", rep, e.ha)
	}
	main, _ := e.remote.ResolveRef(ctx, "refs/heads/main")
	box, _ := e.remote.ResolveRef(ctx, "refs/heads/box")
	if main != box {
		t.Error("main and box should point to the same merge")
	}
}

func TestChangeOutsideHomeAssistantIsNotChecked(t *testing.T) {
	e := newEnv(t)
	e.write("share/x.txt", "1\n")
	e.firstSync()
	e.pushMain(map[string]string{"share/x.txt": "2\n"})
	rep := e.sync()
	if !rep.Applied || e.read("share/x.txt") != "2\n" || e.ha.checks != 0 || e.ha.reloads != 0 {
		t.Errorf("rep %+v, ha %+v", rep, e.ha)
	}
}

func TestCheckErrorsSkipReloadAndStayUntilFixed(t *testing.T) {
	e := newEnv(t)
	e.write("homeassistant/configuration.yaml", "ok: 1\n")
	e.firstSync()

	e.ha.result = ha.CheckResult{Valid: false, Errors: "broken"}
	e.pushMain(map[string]string{"homeassistant/configuration.yaml": "broken: [\n"})
	rep := e.sync()
	wantState(t, rep, StateCheckFailed)
	if e.read("homeassistant/configuration.yaml") != "broken: [\n" {
		t.Error("the change must be applied as is (no rollback)")
	}
	if e.ha.reloads != 0 || !rep.Restart {
		t.Errorf("reload must be skipped, restart recommended: %+v", rep)
	}
	if e.branch("main")["homeassistant/configuration.yaml"] != "broken: [\n" {
		t.Error("main must keep the user's change")
	}

	// Still broken on the next cycle: the check is repeated.
	wantState(t, e.sync(), StateCheckFailed)
	// Fixed by hand on the box: the check passes, and the deferred reload runs.
	e.ha.result = ha.CheckResult{Valid: true}
	e.write("homeassistant/configuration.yaml", "fixed: 1\n")
	rep = e.sync()
	wantState(t, rep, StateIdle)
	if e.ha.reloads != 1 {
		t.Errorf("reloads = %d", e.ha.reloads)
	}
}

func TestCheckAndReloadCanBeTurnedOff(t *testing.T) {
	e := newEnv(t)
	e.cfg.CheckConfig, e.cfg.Reload = false, false
	e.write("homeassistant/a.yaml", "1\n")
	e.firstSync()
	e.pushMain(map[string]string{"homeassistant/a.yaml": "2\n"})
	if rep := e.sync(); !rep.Applied || e.ha.checks != 0 || e.ha.reloads != 0 {
		t.Errorf("rep %+v, ha %+v", rep, e.ha)
	}
}

func TestConflictPushesOnlyBoxAndMergeOptionsResolve(t *testing.T) {
	e := newEnv(t)
	e.write("homeassistant/a.yaml", "v: 1\n")
	e.firstSync()
	e.write("homeassistant/a.yaml", "v: box\n")
	e.pushMain(map[string]string{"homeassistant/a.yaml": "v: main\n"})

	rep := e.sync()
	wantState(t, rep, StateConflict)
	if !slices.Equal(rep.ConflictPaths, []string{"homeassistant/a.yaml"}) {
		t.Errorf("conflict paths = %v", rep.ConflictPaths)
	}
	if e.branch("box")["homeassistant/a.yaml"] != "v: box\n" || e.branch("main")["homeassistant/a.yaml"] != "v: main\n" {
		t.Error("only box should be pushed, with the box's version")
	}
	if e.read("homeassistant/a.yaml") != "v: box\n" {
		t.Error("nothing should be applied during a conflict")
	}

	e.cfg.MergeOptions = []string{"theirs"}
	rep = e.sync()
	wantState(t, rep, StateIdle)
	if e.read("homeassistant/a.yaml") != "v: main\n" {
		t.Error("-X theirs should take main's version")
	}
}

func TestGitattributesFromMainApply(t *testing.T) {
	e := newEnv(t)
	e.write("share/list.txt", "a\n")
	e.firstSync()
	e.pushMain(map[string]string{".gitattributes": "share/list.txt merge=union\n"})
	e.sync()
	e.write("share/list.txt", "a\nbox\n")
	e.pushMain(map[string]string{"share/list.txt": "a\nmain\n"})
	wantState(t, e.sync(), StateIdle)
	if got := e.read("share/list.txt"); got != "a\nbox\nmain\n" && got != "a\nmain\nbox\n" {
		t.Errorf("union merge = %q", got)
	}
}

func TestIgnoredPathLeavesTheRepoButStaysOnDisk(t *testing.T) {
	e := newEnv(t)
	e.write("share/keep.txt", "k\n")
	e.write("share/drop.txt", "d\n")
	e.firstSync()
	e.pushMain(map[string]string{".gitignore": e.branch("main")[".gitignore"] + "share/drop.txt\n"})
	e.sync() // applies the new .gitignore
	e.sync() // the next snapshot honors it
	if _, ok := e.branch("main")["share/drop.txt"]; ok {
		t.Error("an ignored path should leave the repo")
	}
	if e.read("share/drop.txt") != "d\n" {
		t.Error("the ignored file must stay on disk")
	}
}

func TestTooLargeStopsTheCycle(t *testing.T) {
	e := newEnv(t)
	e.cfg.MaxFileSizeMB = 1
	e.write("share/ok.txt", "ok\n")
	e.write("share/big.bin", strings.Repeat("x", 2<<20))
	rep := e.sync()
	wantState(t, rep, StateTooLarge)
	if len(rep.TooLarge) != 1 || !strings.HasPrefix(rep.TooLarge[0], "share/big.bin") {
		t.Errorf("too large = %v", rep.TooLarge)
	}
	if len(e.branch("main")) != 0 || len(e.branch("box")) != 0 {
		t.Error("nothing should be committed")
	}
	e.remove("share/big.bin")
	e.firstSync()
}

func TestMassDeletionFromMainIsHeldBack(t *testing.T) {
	e := newEnv(t)
	changes := map[string]string{}
	for i := range 30 {
		e.write(filepath.Join("share", string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt"), "x\n")
	}
	e.firstSync()
	for p := range e.branch("main") {
		if strings.HasPrefix(p, "share/") {
			changes[p] = testkit.Del
		}
	}
	e.pushMain(changes)
	rep := e.sync()
	wantState(t, rep, StateMassDelete)
	if len(rep.Deleted) != 30 || e.read("share/a.txt") != "x\n" {
		t.Errorf("deleted = %d, disk a.txt = %q", len(rep.Deleted), e.read("share/a.txt"))
	}
	e.cfg.DeleteGuardFiles, e.cfg.DeleteGuardPercent = 1000, 100
	wantState(t, e.sync(), StateIdle)
	if e.read("share/a.txt") != "<missing>" {
		t.Error("after raising the threshold the deletions should apply")
	}
}

func TestMassDeletionFromTheBoxNeedsReview(t *testing.T) {
	e := newEnv(t)
	for i := range 30 {
		e.write(filepath.Join("share", "f"+strings.Repeat("x", i)+".txt"), "x\n")
	}
	e.firstSync()
	e.remove("share")
	rep := e.sync()
	wantState(t, rep, StateReview)
	if rep.Review != ReviewMassDelete {
		t.Errorf("review = %s", rep.Review)
	}
	if len(e.branch("main")) == len(e.branch("box")) {
		t.Error("only box should carry the deletions")
	}
	e.mergeBoxIntoMain()
	wantState(t, e.sync(), StateIdle)
}

func TestFirstSyncAgainstExistingMainWaitsForTheUser(t *testing.T) {
	e := newEnv(t)
	e.pushMain(map[string]string{"homeassistant/a.yaml": "repo\n", "README.md": "# home\n"})
	e.write("homeassistant/a.yaml", "box\n")

	rep := e.sync()
	wantState(t, rep, StateReview)
	if rep.Review != ReviewFirstSync {
		t.Errorf("review = %s", rep.Review)
	}
	if e.read("homeassistant/a.yaml") != "box\n" || e.read("README.md") != "<missing>" {
		t.Error("nothing should be applied before the user resolves")
	}
	box := e.branch("box")
	if box["homeassistant/a.yaml"] != "box\n" || box["README.md"] != "" {
		t.Errorf("box = %v (on top of main, main..box shows what differs)", box)
	}
	wantState(t, e.sync(), StateReview)

	e.mergeBoxIntoMain()
	wantState(t, e.sync(), StateIdle)
	if e.read("homeassistant/a.yaml") != "box\n" {
		t.Error("after the merge the box keeps its version")
	}
}

func TestRootChangeNeedsReview(t *testing.T) {
	e := newEnv(t)
	e.write("homeassistant/a.yaml", "1\n")
	e.firstSync()
	e.cfg.Root = "config"
	rep := e.sync()
	wantState(t, rep, StateReview)
	if rep.Review != ReviewRootChanged {
		t.Errorf("review = %s", rep.Review)
	}
	if e.branch("box")["a.yaml"] != "1\n" {
		t.Errorf("box should have the new layout: %v", e.branch("box"))
	}
}

func TestNarrowRootSyncsOnlyThatFolder(t *testing.T) {
	e := newEnv(t)
	e.cfg.Root = "/homeassistant"
	e.write("homeassistant/configuration.yaml", "a: 1\n")
	e.write("homeassistant/.storage/core.config_entries", "{}")
	e.write("share/x.txt", "not synced\n")
	e.firstSync()
	main := e.branch("main")
	if main["configuration.yaml"] != "a: 1\n" || main["share/x.txt"] != "" || main[".storage/core.config_entries"] != "" {
		t.Errorf("main = %v", main)
	}
	if !strings.Contains(main[".gitignore"], ".storage/") || strings.Contains(main[".gitignore"], "homeassistant/") {
		t.Errorf(".gitignore not adapted to the root:\n%s", main[".gitignore"])
	}
	// A change from main still counts as Home Assistant's config.
	e.pushMain(map[string]string{"packages/x.yaml": "x: 1\n"})
	if rep := e.sync(); !rep.Checked || !rep.Restart {
		t.Errorf("rep = %+v", rep)
	}
}

func TestTopLevelCollisionWithTheContainer(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.fs, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write("share/x.txt", "1\n")
	e.firstSync()
	e.pushMain(map[string]string{"data/options.json": "{}", "README.md": "# ok\n"})
	rep := e.sync()
	wantState(t, rep, StatePathCollision)
	if !slices.Equal(rep.Collisions, []string{"data"}) {
		t.Errorf("collisions = %v", rep.Collisions)
	}
	if _, err := os.Stat(filepath.Join(e.fs, "data", "options.json")); err == nil {
		t.Error("nothing may be written into the container's folders")
	}
}

func TestTopLevelFilesAreRestoredWhenTheContainerIsRecreated(t *testing.T) {
	e := newEnv(t)
	e.write("share/x.txt", "1\n")
	e.firstSync()
	e.pushMain(map[string]string{"README.md": "# home\n"})
	e.sync()
	e.remove("README.md")
	e.remove(".gitignore")
	rep := e.sync()
	if rep.SnapshotCommit != "" {
		t.Errorf("restored files must not count as deletions: %+v", rep.Outgoing)
	}
	if e.read("README.md") != "# home\n" || e.read(".gitignore") == "<missing>" {
		t.Error("top-level files were not restored")
	}
}

func TestSecretsWarnByDefaultAndBlockWhenAsked(t *testing.T) {
	const token = "ghp_" + "0123456789abcdefghijABCDEFGHIJ012345"
	e := newEnv(t)
	e.write("homeassistant/configuration.yaml", "token: "+token+"\n")
	rep := e.firstSync()
	if len(rep.Findings) != 1 || e.branch("main")["homeassistant/configuration.yaml"] == "" {
		t.Errorf("warn mode should commit and report: %+v", rep)
	}
	found := slices.ContainsFunc(e.ha.events, func(ev ha.Event) bool { return ev.Type == ha.EventSecretsFound })
	if !found {
		t.Error("the finding should be notified")
	}
	// Same finding next cycle: not notified again, still reported.
	n := len(e.ha.events)
	if rep := e.sync(); len(rep.Findings) != 1 || len(e.ha.events) != n {
		t.Errorf("repeat: %+v, events %d → %d", rep.Findings, n, len(e.ha.events))
	}
	st, _ := LoadState(e.state)
	if len(st.Findings) != 1 || strings.Contains(st.Findings[0].String(), token) {
		t.Errorf("state findings = %+v", st.Findings)
	}

	e2 := newEnv(t)
	e2.cfg.SecretScan.Mode = config.SecretsBlock
	e2.write("homeassistant/configuration.yaml", "token: "+token+"\n")
	wantState(t, e2.sync(), StateBlockedSecrets)
	if len(e2.branch("main")) != 0 || len(e2.branch("box")) != 0 {
		t.Error("block mode must not commit")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.write("share/x.txt", "1\n")
	e.firstSync()
	e.write("share/x.txt", "2\n")
	e.pushMain(map[string]string{"share/y.txt": "new\n"})
	before, _ := e.remote.ResolveRef(ctx, "refs/heads/main")

	rep, err := e.engine().Sync(ctx, SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outgoing) != 1 || len(rep.Incoming) == 0 {
		t.Errorf("dry run should report both directions: %+v", rep)
	}
	after, _ := e.remote.ResolveRef(ctx, "refs/heads/main")
	if before != after || e.read("share/y.txt") != "<missing>" {
		t.Error("the dry run wrote something")
	}
	// The real sync afterwards still sees everything.
	rep = e.sync()
	if rep.SnapshotCommit == "" || e.read("share/y.txt") != "new\n" {
		t.Errorf("after dry run: %+v", rep)
	}
}

func TestEditDuringApplyIsRetried(t *testing.T) {
	e := newEnv(t)
	e.write("share/a.txt", "1\n")
	e.write("share/b.txt", "1\n")
	e.firstSync()
	e.pushMain(map[string]string{"share/a.txt": "main\n"})
	e.hooks.beforeApply = func(attempt int) {
		if attempt == 1 {
			e.write("share/a.txt", "1\nedited\n")
		}
	}
	rep, err := e.engine().Sync(ctx, SyncOptions{})
	if err != nil && !errors.Is(err, ErrTooManyRetries) {
		t.Fatal(err)
	}
	if rep.Attempts < 2 {
		t.Errorf("attempts = %d", rep.Attempts)
	}
	if e.read("share/a.txt") != "1\nedited\n" && rep.State != StateConflict {
		t.Error("the edit made during the apply was lost")
	}
}

func TestPushRaceIsRetried(t *testing.T) {
	e := newEnv(t)
	e.write("share/a.txt", "1\n")
	e.firstSync()
	e.write("share/a.txt", "2\n")
	e.hooks.beforePush = func(attempt int) {
		if attempt == 1 {
			e.pushMain(map[string]string{"share/other.txt": "x\n"})
		}
	}
	rep := e.sync()
	wantState(t, rep, StateIdle)
	if rep.Attempts != 2 || e.read("share/other.txt") != "x\n" || e.branch("main")["share/a.txt"] != "2\n" {
		t.Errorf("rep %+v", rep)
	}
}

func TestBaseGitignoreForRoots(t *testing.T) {
	ha := baseGitignoreFor("/homeassistant")
	for _, want := range []string{".storage/\n", "custom_components/\n", "secrets.yaml\n", "*.db\n"} {
		if !strings.Contains(ha, want) {
			t.Errorf("/homeassistant base lacks %q:\n%s", want, ha)
		}
	}
	for _, bad := range []string{"homeassistant/", "ssl/", "backup/"} {
		if strings.Contains(ha, "\n"+bad) {
			t.Errorf("/homeassistant base has %q", bad)
		}
	}
	other := baseGitignoreFor("/app_configs/x")
	if strings.Contains(other, ".storage") || !strings.Contains(other, "node_modules/") {
		t.Errorf("/app_configs/x base:\n%s", other)
	}
}
