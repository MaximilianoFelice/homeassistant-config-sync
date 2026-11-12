package apply

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/ha"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/paths"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

type env struct {
	t       *testing.T
	ha      string
	addons  string
	repo    *gitx.Repo
	m       *paths.Matcher
	journal string
	checks  int
	verdict ha.CheckResult
	// results, if not empty, is returned in order before verdict.
	results  []ha.CheckResult
	checkErr error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	ms := config.DefaultMappings()
	ms[0].Box = filepath.Join(root, "homeassistant")
	ms[1].Box = filepath.Join(root, "addon_configs")
	ms[1].Include = []string{"**/*.conf"}
	for _, mp := range ms {
		if err := os.MkdirAll(mp.Box, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := paths.New(ms)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		t: t, ha: ms[0].Box, addons: ms[1].Box,
		repo:    testkit.NewBareRepo(t),
		m:       m,
		journal: filepath.Join(root, "state", "apply.json"),
		verdict: ha.CheckResult{Valid: true},
	}
}

func (e *env) commit(parent string, changes map[string]string) string {
	return testkit.Commit(e.t, e.repo, parent, changes)
}

// boxPath maps a repo path to the env's disk.
func (e *env) boxPath(p string) string {
	if rel, ok := strings.CutPrefix(p, "config/"); ok {
		return filepath.Join(e.ha, rel)
	}
	if rel, ok := strings.CutPrefix(p, "app_configs/"); ok {
		return filepath.Join(e.addons, rel)
	}
	return ""
}

// materialize writes a commit's mapped files to disk (disk == B).
func (e *env) materialize(commit string) {
	e.t.Helper()
	ref := "refs/heads/m"
	cur, _ := e.repo.ResolveRef(ctx, ref)
	if err := e.repo.UpdateRef(ctx, ref, commit, cur); err != nil {
		e.t.Fatal(err)
	}
	modes := testkit.Modes(e.t, e.repo, ref)
	for p, c := range testkit.Tree(e.t, e.repo, ref) {
		if bp := e.boxPath(p); bp != "" {
			perm := os.FileMode(0o644)
			if modes[p] == "100755" {
				perm = 0o755
			}
			testkit.WriteFileMode(e.t, bp, c, perm)
		}
	}
}

func (e *env) disk() map[string]string {
	out := map[string]string{}
	for k, v := range testkit.Disk(e.t, e.ha) {
		out["config/"+k] = v
	}
	for k, v := range testkit.Disk(e.t, e.addons) {
		out["app_configs/"+k] = v
	}
	return out
}

func (e *env) opts() Options {
	return Options{
		Matcher:     e.m,
		JournalPath: e.journal,
		Checker: ha.CheckerFunc(func(context.Context) (ha.CheckResult, error) {
			e.checks++
			if len(e.results) > 0 {
				r := e.results[0]
				e.results = e.results[1:]
				return r, nil
			}
			return e.verdict, e.checkErr
		}),
	}
}

func (e *env) plan(from, to string) *Plan {
	e.t.Helper()
	p, err := MakePlan(ctx, e.repo, e.m, from, to)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) journalExists() bool {
	_, err := os.Stat(e.journal)
	return err == nil
}

func assertDisk(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("disk has %d files, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
}

var baseFiles = map[string]string{
	"README.md":                         "# home\n",
	"config/configuration.yaml":         "homeassistant:\n",
	"config/automations.yaml":           "[]\n",
	"config/scenes.yaml":                "[]\n",
	"config/packages/old.yaml":          "v: 1\n",
	"config/.storage/lovelace":          "{\"v\": 1}\n",
	"app_configs/core_mosquitto/m.conf": "listener 1883\n",
}

// baseDisk is what materialize writes to disk for baseFiles.
func baseDisk() map[string]string {
	out := map[string]string{}
	for p, c := range baseFiles {
		if p != "README.md" {
			out[p] = c
		}
	}
	return out
}

func TestMakePlan(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	r := e.commit(b, map[string]string{
		"README.md":                         "# home v2\n",
		"config/automations.yaml":           "- id: a\n",
		"config/scenes.yaml":                testkit.Del,
		"config/packages/new.yaml":          "n: 1\n",
		"config/.storage/lovelace":          "{\"v\": 2}\n",
		"config/custom_components/x/a.yaml": "x:\n",
		"config/secrets.yaml":               "pw: 1\n",
		"config/notes.txt":                  "hello\n",
	})
	p := e.plan(b, r)

	var ops []string
	for _, op := range p.Ops {
		kind := "W"
		if op.IsDelete() {
			kind = "D"
		}
		ops = append(ops, kind+" "+op.RepoPath)
	}
	if want := []string{"D config/scenes.yaml", "W config/automations.yaml", "W config/packages/new.yaml"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
	ignored := map[string]string{}
	for _, ig := range p.Ignored {
		ignored[ig.RepoPath] = ig.Reason
	}
	want := map[string]string{
		"README.md":                         string(paths.ReasonOutsideMappings),
		"config/.storage/lovelace":          ReasonPushOnly,
		"config/custom_components/x/a.yaml": string(paths.ReasonExcluded),
		"config/secrets.yaml":               string(paths.ReasonHardDenied),
		"config/notes.txt":                  string(paths.ReasonNotIncluded),
	}
	if len(ignored) != len(want) {
		t.Errorf("ignored = %v", ignored)
	}
	for k, v := range want {
		if ignored[k] != v {
			t.Errorf("%s: reason %q, want %q", k, ignored[k], v)
		}
	}
}

func TestApplyWritesDeletesAndChecks(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{
		"config/automations.yaml":      "- id: a\n",
		"config/packages/old.yaml":     testkit.Del,
		"config/packages/sub/new.yaml": "n: 1\n",
		"config/python_scripts/x.py":   "x:print(1)\n",
	})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || !res.Checked || e.checks != 2 { // baseline + after writing
		t.Errorf("res = %+v, checks = %d", res, e.checks)
	}
	want := baseDisk()
	want["config/automations.yaml"] = "- id: a\n"
	delete(want, "config/packages/old.yaml")
	want["config/packages/sub/new.yaml"] = "n: 1\n"
	want["config/python_scripts/x.py"] = "print(1)\n"
	assertDisk(t, e.disk(), want)

	if fi, err := os.Stat(filepath.Join(e.ha, "python_scripts/x.py")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("x.py should be executable: %v %v", fi.Mode(), err)
	}
	// The journal stays in phase checked until the engine moves box.
	j, err := readJournal(e.journal)
	if err != nil || j.Phase != PhaseChecked || j.To != r {
		t.Errorf("journal = %+v, %v", j, err)
	}
	if err := Finish(e.journal); err != nil || e.journalExists() {
		t.Errorf("Finish: %v", err)
	}
	// No temporary files are left.
	for p := range testkit.Disk(t, e.ha) {
		if strings.Contains(p, ".hcs-tmp-") {
			t.Errorf("temporary file left behind: %s", p)
		}
	}
}

func TestDeletingLastFileRemovesEmptyDirs(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", map[string]string{"config/configuration.yaml": "a:\n", "config/packages/a/b/c.yaml": "c:\n"})
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/packages/a/b/c.yaml": testkit.Del})
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.ha, "packages")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("packages/ should have been deleted: %v", err)
	}
	if _, err := os.Stat(e.ha); err != nil {
		t.Errorf("the root is never deleted: %v", err)
	}
}

func TestNoCheckWhenOnlyAddonConfigsChange(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"app_configs/core_mosquitto/m.conf": "listener 8883\n"})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked || e.checks != 0 {
		t.Errorf("check_config should not run: %+v", res)
	}
}

func TestPrecheckFailureTouchesNothing(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: [broken\n", "config/scenes.yaml": "ok: 1\n"})
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrPrecheck) || !strings.Contains(err.Error(), "config/automations.yaml") {
		t.Fatalf("wanted ErrPrecheck with the path, got %v", err)
	}
	assertDisk(t, e.disk(), baseDisk())
	if e.checks != 0 || e.journalExists() {
		t.Error("there should be no check and no journal")
	}
}

func TestRejectedConfigRollsBackExactly(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	// Unusual permissions the rollback must preserve.
	if err := os.Chmod(filepath.Join(e.ha, "scenes.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.commit(b, map[string]string{
		"config/automations.yaml":  "- id: invalid\n",
		"config/scenes.yaml":       testkit.Del,
		"config/packages/old.yaml": testkit.Del,
		"config/new/dir/x.yaml":    "x: 1\n",
	})
	e.verdict = ha.CheckResult{Valid: false, Errors: "Invalid config for [automation]"}

	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "Invalid config for [automation]") {
		t.Fatalf("wanted ErrRejected with HA's errors, got %v", err)
	}
	if !res.RolledBack || res.Applied {
		t.Errorf("res = %+v", res)
	}
	assertDisk(t, e.disk(), baseDisk())
	if fi, err := os.Stat(filepath.Join(e.ha, "scenes.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("scenes.yaml restored with permissions %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(e.ha, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("directories created by the apply must disappear on rollback")
	}
	if e.journalExists() {
		t.Error("no journal should remain after a clean rollback")
	}
}

func TestWarningsPolicy(t *testing.T) {
	disabled := "Invalid config for 'input_boolean' at configuration.yaml, line 117: invalid boolean value"
	old := "Invalid config for 'timer' at configuration.yaml, line 40: offset bad"
	oldMoved := "Invalid config for 'timer' at configuration.yaml, line 42: offset bad"
	tests := []struct {
		name     string
		policy   string
		results  []ha.CheckResult // [before, after] (or only [after])
		rejected bool
	}{
		{"new: a warning appears", "new", []ha.CheckResult{{Valid: true}, {Valid: true, Warnings: []string{disabled}}}, true},
		{"new: the warning was already there (line shifted)", "new", []ha.CheckResult{{Valid: true, Warnings: []string{old}}, {Valid: true, Warnings: []string{oldMoved}}}, false},
		{"new: invalid baseline, valid after", "new", []ha.CheckResult{{Valid: false, Errors: "x"}, {Valid: true}}, false},
		{"any: any warning", "any", []ha.CheckResult{{Valid: true, Warnings: []string{old}}}, true},
		{"ignore: errors only", "ignore", []ha.CheckResult{{Valid: true, Warnings: []string{disabled}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			b := e.commit("", baseFiles)
			e.materialize(b)
			r := e.commit(b, map[string]string{"config/configuration.yaml": "homeassistant:\ninput_boolean:\n"})
			e.results = tt.results
			opts := e.opts()
			opts.Warnings = tt.policy
			_, err := Apply(ctx, e.repo, e.plan(b, r), opts)
			if tt.rejected {
				if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "warnings") {
					t.Fatalf("wanted a rejection for warnings, got %v", err)
				}
				assertDisk(t, e.disk(), baseDisk())
				return
			}
			if err != nil {
				t.Fatalf("did not want a rejection: %v", err)
			}
			if e.checks != len(tt.results) {
				t.Errorf("checks = %d, want %d", e.checks, len(tt.results))
			}
		})
	}
}

func TestCheckerErrorRollsBack(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	e.checkErr = errors.New("HA is not responding")
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrCheckUnavailable) {
		t.Fatalf("wanted ErrCheckUnavailable, got %v", err)
	}
	assertDisk(t, e.disk(), baseDisk())
}

func TestEditAfterSnapshotAbortsBeforeWriting(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: main\n", "config/scenes.yaml": "- s\n"})

	// Someone edits from the HA UI between the snapshot and the apply.
	testkit.WriteFile(t, filepath.Join(e.ha, "scenes.yaml"), "- edited in the UI\n")
	want := e.disk()

	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrDiskChanged) || !strings.Contains(err.Error(), "config/scenes.yaml") {
		t.Fatalf("wanted ErrDiskChanged naming the path, got %v", err)
	}
	assertDisk(t, e.disk(), want)
	if e.journalExists() {
		t.Error("there should be no journal")
	}
}

func TestNewFileThatAlreadyExistsAborts(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/scripts.yaml": "s: main\n"})
	testkit.WriteFile(t, filepath.Join(e.ha, "scripts.yaml"), "s: created in the UI\n")
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("wanted ErrDiskChanged, got %v", err)
	}
	if got := e.disk()["config/scripts.yaml"]; got != "s: created in the UI\n" {
		t.Errorf("an edit was overwritten: %q", got)
	}
}

func TestExecBitChangeOnDiskAborts(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	if err := os.Chmod(filepath.Join(e.ha, "automations.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("wanted ErrDiskChanged, got %v", err)
	}
}

func TestSymlinkedParentIsRejected(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.ha, "packages2")); err != nil {
		t.Fatal(err)
	}
	r := e.commit(b, map[string]string{"config/packages2/x.yaml": "x: 1\n"})
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("wanted ErrUnsafePath, got %v", err)
	}
	if len(testkit.Disk(t, outside)) != 0 {
		t.Error("something was written outside the mapping through the symlink")
	}
}

func TestFileBecomesDirectoryAndBack(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": testkit.Del, "config/automations.yaml/kitchen.yaml": "- id: c\n"})
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	if got := e.disk()["config/automations.yaml/kitchen.yaml"]; got != "- id: c\n" {
		t.Errorf("kitchen = %q", got)
	}
	_ = Finish(e.journal)

	back := e.commit(r, map[string]string{"config/automations.yaml/kitchen.yaml": testkit.Del, "config/automations.yaml": "[]\n"})
	if _, err := Apply(ctx, e.repo, e.plan(r, back), e.opts()); err != nil {
		t.Fatal(err)
	}
	assertDisk(t, e.disk(), baseDisk())
}

func TestUnmanagedFileBlocksDirectoryToFileAndRollsBack(t *testing.T) {
	e := newEnv(t)
	// pk.yaml is a directory in B and a file in R, but on disk it also has
	// an unmanaged file that the apply cannot delete.
	b := e.commit("", map[string]string{"config/configuration.yaml": "a:\n", "config/pk.yaml/a.yaml": "a:\n"})
	e.materialize(b)
	testkit.WriteFile(t, filepath.Join(e.ha, "pk.yaml/notes.txt"), "unmanaged\n")
	r := e.commit(b, map[string]string{"config/pk.yaml/a.yaml": testkit.Del, "config/pk.yaml": "x: 1\n"})

	before := e.disk()
	_, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err == nil || !strings.Contains(err.Error(), "directory with unmanaged content") {
		t.Fatalf("wanted an error for the directory with unmanaged content, got %v", err)
	}
	assertDisk(t, e.disk(), before)
}

func TestPushOnlyIsNeverWritten(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/.storage/lovelace": "{\"v\": 99}\n"})
	res, err := Apply(ctx, e.repo, e.plan(b, r), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || len(res.Plan.Ops) != 0 {
		t.Errorf("there should be no operations: %+v", res.Plan)
	}
	assertDisk(t, e.disk(), baseDisk())
}

// TestCrashRecovery simulates the process dying after each operation
// (and after the check) and verifies that Recover leaves the disk at B or
// at R, never mixed.
func TestCrashRecovery(t *testing.T) {
	changes := map[string]string{
		"config/automations.yaml":           "- id: a\n",
		"config/scenes.yaml":                testkit.Del,
		"config/packages/old.yaml":          testkit.Del,
		"config/new/x.yaml":                 "x: 1\n",
		"app_configs/core_mosquitto/m.conf": "listener 8883\n",
	}
	nOps := len(changes)
	for crashAt := 0; crashAt <= nOps; crashAt++ {
		t.Run("", func(t *testing.T) {
			e := newEnv(t)
			b := e.commit("", baseFiles)
			e.materialize(b)
			r := e.commit(b, changes)
			opts := e.opts()
			opts.afterOp = func(i int) bool { return i == crashAt }

			if _, err := Apply(ctx, e.repo, e.plan(b, r), opts); !errors.Is(err, errCrash) {
				t.Fatalf("wanted the simulated crash, got %v", err)
			}
			if !e.journalExists() {
				t.Fatal("the journal must remain after a crash")
			}

			outcome, j, err := Recover(ctx, e.repo, e.m, e.journal)
			if err != nil {
				t.Fatal(err)
			}
			// Before the check (crashAt < nOps) or right after the check but
			// before marking "checked": always back to B.
			if outcome != RecoverRolledBack || j.From != b {
				t.Errorf("outcome = %s", outcome)
			}
			assertDisk(t, e.disk(), baseDisk())
			if e.journalExists() {
				t.Error("Recover must delete the journal")
			}
		})
	}
}

func TestRecoverAfterCheckedConfirmsTarget(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n"})
	// Full apply: the journal stays at "checked" and the "process dies"
	// before the engine moves box.
	if _, err := Apply(ctx, e.repo, e.plan(b, r), e.opts()); err != nil {
		t.Fatal(err)
	}
	outcome, j, err := Recover(ctx, e.repo, e.m, e.journal)
	if err != nil || outcome != RecoverAtTarget || j.To != r {
		t.Fatalf("outcome = %s, %v", outcome, err)
	}
	if !e.journalExists() {
		t.Error("with at_target the journal stays until Finish")
	}
	if got := e.disk()["config/automations.yaml"]; got != "- id: a\n" {
		t.Errorf("automations = %q", got)
	}
}

func TestRecoverDoesNotClobberLaterEdits(t *testing.T) {
	e := newEnv(t)
	b := e.commit("", baseFiles)
	e.materialize(b)
	r := e.commit(b, map[string]string{"config/automations.yaml": "- id: a\n", "config/scenes.yaml": "- s\n"})
	opts := e.opts()
	opts.afterOp = func(i int) bool { return i == 1 }
	if _, err := Apply(ctx, e.repo, e.plan(b, r), opts); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	// After the crash, someone edits one of the files.
	testkit.WriteFile(t, filepath.Join(e.ha, "scenes.yaml"), "- edited afterwards\n")

	if _, _, err := Recover(ctx, e.repo, e.m, e.journal); err != nil {
		t.Fatal(err)
	}
	d := e.disk()
	if d["config/automations.yaml"] != "[]\n" {
		t.Errorf("automations should have gone back to B: %q", d["config/automations.yaml"])
	}
	if d["config/scenes.yaml"] != "- edited afterwards\n" {
		t.Errorf("the later edit must not be overwritten: %q", d["config/scenes.yaml"])
	}
}

func TestRecoverWithoutJournal(t *testing.T) {
	e := newEnv(t)
	if outcome, _, err := Recover(ctx, e.repo, e.m, e.journal); err != nil || outcome != RecoverNone {
		t.Errorf("= %s, %v", outcome, err)
	}
}
