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

// env is a fake box plus the engine's bare repo, all in a TempDir.
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

// seed commits an arbitrary tree on box, as if it came from main.
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
		t.Errorf("tree with %d files, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, want %q", p, got[p], c)
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
		t.Fatalf("want a root commit, got %+v", res)
	}
	assertTree(t, e.tree(), map[string]string{
		"config/configuration.yaml": "homeassistant:\n",
		"config/automations.yaml":   "[]\n",
	})
	if got := kinds(res.Changes); got != "A config/automations.yaml, A config/configuration.yaml" {
		t.Errorf("changes = %s", got)
	}
}

func TestSnapshotIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	first := e.take()
	second := e.take()
	if second.Commit != "" || len(second.Changes) != 0 {
		t.Errorf("a second snapshot without changes should not commit: %+v", second)
	}
	if e.head() != first.Commit {
		t.Error("box moved without changes")
	}

	// Also with the first one's stat cache.
	e.opts.Cache = first.Cache
	if third := e.take(); third.Commit != "" {
		t.Errorf("not with the cache either: %+v", third)
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
	e.write("packages/new.yaml", "x:\n")
	res := e.take()

	if res.Parent != first.Commit {
		t.Errorf("parent = %s, want %s", res.Parent, first.Commit)
	}
	if got := kinds(res.Changes); got != "M config/automations.yaml, A config/packages/new.yaml, D config/scenes.yaml" {
		t.Errorf("changes = %s", got)
	}
	assertTree(t, e.tree(), map[string]string{
		"config/automations.yaml":  "- id: '1'\n",
		"config/scripts.yaml":      "a:\n",
		"config/packages/new.yaml": "x:\n",
	})
}

func TestSnapshotPreservesWhatTheRepoOwns(t *testing.T) {
	e := newEnv(t)
	e.seed(map[string]string{
		"README.md":                 "# my home\n",
		".github/workflows/ci.yaml": "on: push\n",
		"config/notes.txt":          "unmanaged\n",
		"config/automations.yaml":   "old\n",
		"config/deleted.yaml":       "bye\n",
	})
	e.write("automations.yaml", "new\n")

	res := e.take()
	if got := kinds(res.Changes); got != "D config/deleted.yaml, M config/automations.yaml" &&
		got != "M config/automations.yaml, D config/deleted.yaml" {
		t.Errorf("changes = %s", got)
	}
	assertTree(t, e.tree(), map[string]string{
		"README.md":                 "# my home\n",
		".github/workflows/ci.yaml": "on: push\n",
		"config/notes.txt":          "unmanaged\n",
		"config/automations.yaml":   "new\n",
	})
}

func TestSnapshotFileBecomesDirectory(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	e.take()
	e.rm("automations.yaml")
	e.write("automations.yaml/kitchen.yaml", "- id: c\n")
	e.take()
	assertTree(t, e.tree(), map[string]string{"config/automations.yaml/kitchen.yaml": "- id: c\n"})
}

func TestSnapshotTracksExecutableBit(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.ha, "python_scripts/hello.py")
	testkit.WriteFileMode(t, p, "print(1)\n", 0o644)
	e.take()
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.take()
	if got := kinds(res.Changes); got != "M config/python_scripts/hello.py" {
		t.Errorf("changes = %s", got)
	}
	if m := testkit.Modes(t, e.repo, boxRef)["config/python_scripts/hello.py"]; m != "100755" {
		t.Errorf("mode = %s", m)
	}
}

func TestSkippedFilesKeepTheirPreviousVersion(t *testing.T) {
	e := newEnv(t)
	e.write("large.yaml", "small\n")
	e.write("binary.yaml", "text\n")
	e.write("link.yaml", "file\n")
	e.take()

	e.write("large.yaml", strings.Repeat("x", 2048))
	e.write("binary.yaml", "a\x00b")
	e.rm("link.yaml")
	if err := os.Symlink(filepath.Join(e.ha, "large.yaml"), filepath.Join(e.ha, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	res := e.take()
	if res.Commit != "" {
		t.Errorf("skipping is neither deleting nor modifying; changes = %s", kinds(res.Changes))
	}
	if len(res.Skipped) != 3 {
		t.Errorf("skipped = %+v", res.Skipped)
	}
}

func TestMissingRootIsSuspicious(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	before := e.take().Commit

	e.rm("")
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, snapshot.ErrSuspicious) {
		t.Fatalf("want ErrSuspicious, got %v", err)
	}
	if e.head() != before {
		t.Error("box should not move")
	}
}

func TestEmptyRootIsSuspicious(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	e.write("notes.txt", "this is not managed\n")
	before := e.take().Commit

	e.rm("automations.yaml")
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, snapshot.ErrSuspicious) {
		t.Fatalf("want ErrSuspicious (empty mount), got %v", err)
	}
	if e.head() != before {
		t.Error("box should not move")
	}
}

func TestMassDeleteGuard(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		deleted int
		blocked bool
	}{
		{"small config, delete 1 of 4", 4, 1, false},
		{"delete 4 of 10: 40% but fewer than 5", 10, 4, false},
		{"delete 5 of 10: 50% and at least 5", 10, 5, true},
		{"delete 5 of 40: 12.5%", 40, 5, false},
		{"delete 26 of 200: more than 25", 200, 26, true},
		{"delete 25 of 200: exactly 25", 200, 25, false},
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
					t.Fatalf("want ErrMassDelete, got %v", err)
				}
				if e.head() != before {
					t.Error("box should not move")
				}
				// With explicit confirmation it goes through.
				e.opts.AllowMassDelete = true
				if res := e.take(); res.Commit == "" {
					t.Error("with AllowMassDelete it should commit")
				}
				return
			}
			if err != nil || res.Commit == "" {
				t.Fatalf("want no block: %v %+v", err, res)
			}
		})
	}
}

func TestBeforeCommitErrorWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.write("automations.yaml", "[]\n")
	before := e.take().Commit

	secret := "api_key: sk-this-is-a-secret\n"
	e.write("rest.yaml", secret)
	blocked := errors.New("secret found")
	var seen map[string][]byte
	e.opts.BeforeCommit = func(_ context.Context, cs []snapshot.Change, contents map[string][]byte) error {
		seen = contents
		return blocked
	}
	_, err := snapshot.Take(ctx, e.repo, e.opts)
	if !errors.Is(err, blocked) {
		t.Fatalf("want the hook's error, got %v", err)
	}
	if string(seen["config/rest.yaml"]) != secret {
		t.Errorf("the hook did not receive the content: %q", seen)
	}
	if e.head() != before {
		t.Error("box should not move")
	}
	// Not even the blob was written to the local repo.
	oid := gitBlobOID(t, secret)
	if ok, err := e.repo.HasObject(ctx, oid); err != nil || ok {
		t.Errorf("the secret's blob ended up in the repo (%v, %v)", ok, err)
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
		t.Errorf("the cache hid an edit: %s", kinds(res.Changes))
	}
	assertTree(t, e.tree(), map[string]string{"config/automations.yaml": "bbbb\n"})
}

func TestMessage(t *testing.T) {
	one := snapshot.Message([]snapshot.Change{{Path: "config/a.yaml", Kind: snapshot.Modified}})
	if !strings.HasPrefix(one, "box: config/a.yaml\n\nM config/a.yaml\n") {
		t.Errorf("message = %q", one)
	}
	var many []snapshot.Change
	for i := range 105 {
		many = append(many, snapshot.Change{Path: fmt.Sprintf("config/%d.yaml", i), Kind: snapshot.Added})
	}
	msg := snapshot.Message(many)
	if !strings.HasPrefix(msg, "box: 105 changes\n\n") || !strings.Contains(msg, "… and 5 more") {
		t.Errorf("message = %q", msg)
	}
}

// gitBlobOID computes the OID with git in a throwaway repo, so it does not
// depend on scan.BlobOID (the code under test).
func gitBlobOID(t *testing.T, s string) string {
	t.Helper()
	oid, err := testkit.NewBareRepo(t).WriteBlob(ctx, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return oid
}
