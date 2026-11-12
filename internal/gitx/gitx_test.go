package gitx_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/scan"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

var ctx = context.Background()

func ident(t *testing.T) gitx.Ident {
	t.Helper()
	id, err := gitx.ParseIdent("Test <test@example.invalid>", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnvIgnoresUserEnvironment(t *testing.T) {
	home := t.TempDir()
	testkit.WriteFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = Real User\n[core]\n\thooksPath = /tmp/hooks\n")
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GIT_DIR", "/nope")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))

	r := testkit.NewBareRepo(t)
	env := r.Env()
	for _, bad := range []string{"SSH_AUTH_SOCK=", "GIT_DIR=/nope", "HOME=" + home} {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, bad) }) {
			t.Errorf("git environment inherits %q", bad)
		}
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(env, want) {
			t.Errorf("%q missing from the environment", want)
		}
	}
	ssh := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
			ssh = v
		}
	}
	for _, want := range []string{"IdentityAgent=none", "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "-F /dev/null"} {
		if !strings.Contains(ssh, want) {
			t.Errorf("GIT_SSH_COMMAND lacks %q: %s", want, ssh)
		}
	}

	// And git really does not read the user's ~/.gitconfig: if it did, this
	// insteadOf would rewrite file:// to https:// and the fetch would fail.
	testkit.WriteFile(t, filepath.Join(home, ".gitconfig"), "[url \"https://example.invalid/\"]\n\tinsteadOf = file://\n")
	origin := testkit.NewBareRepo(t)
	tree, err := origin.BuildTree(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := origin.CommitTree(ctx, tree, nil, "init\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.UpdateRef(ctx, "refs/heads/main", c, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Fetch(ctx, "file://"+origin.Dir(), "refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Errorf("fetch failed: was the user's ~/.gitconfig read? %v", err)
	}
}

func TestRefusesNetworkProtocols(t *testing.T) {
	r := testkit.NewBareRepo(t)
	for _, url := range []string{
		"https://example.invalid/owner/repo.git",
		"http://example.invalid/owner/repo.git",
		"git://example.invalid/owner/repo.git",
	} {
		err := r.Fetch(ctx, url, "refs/heads/main:refs/remotes/origin/main")
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("fetch %s: wanted 'transport not allowed' without touching the network, got %v", url, err)
		}
	}
}

func TestFileRemoteOnlyWhenAllowed(t *testing.T) {
	origin := testkit.NewBareRepo(t)
	blob, err := origin.WriteBlob(ctx, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := origin.BuildTree(ctx, "", []gitx.IndexUpdate{{Path: "a.txt", Mode: "100644", OID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := origin.CommitTree(ctx, tree, nil, "init\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.UpdateRef(ctx, "refs/heads/main", c, ""); err != nil {
		t.Fatal(err)
	}

	denied := gitx.Open(testkit.NewBareRepo(t).Dir(), gitx.Options{})
	if err := denied.Fetch(ctx, "file://"+origin.Dir(), "refs/heads/main:refs/remotes/origin/main"); err == nil {
		t.Error("file:// should be blocked without AllowFile")
	}

	allowed := testkit.NewBareRepo(t)
	if err := allowed.Fetch(ctx, "file://"+origin.Dir(), "refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	got, err := allowed.ResolveRef(ctx, "refs/remotes/origin/main")
	if err != nil || got != c {
		t.Errorf("origin/main = %q (%v), want %s", got, err, c)
	}
}

func TestBuildTreeCommitAndList(t *testing.T) {
	r := testkit.NewBareRepo(t)
	a, _ := r.WriteBlob(ctx, []byte("a\n"))
	b, _ := r.WriteBlob(ctx, []byte("#!/bin/sh\n"))
	tree, err := r.BuildTree(ctx, "", []gitx.IndexUpdate{
		{Path: "config/automations.yaml", Mode: "100644", OID: a},
		{Path: "config/with spaces/ñandú.yaml", Mode: "100644", OID: a},
		{Path: "config/scripts/run.sh", Mode: "100755", OID: b},
	})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := r.CommitTree(ctx, tree, nil, "one\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c1, ""); err != nil {
		t.Fatal(err)
	}

	// File → directory and a deletion, on top of the previous base.
	tree2, err := r.BuildTree(ctx, tree, []gitx.IndexUpdate{
		{Path: "config/automations.yaml"},
		{Path: "config/automations.yaml/kitchen.yaml", Mode: "100644", OID: a},
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.CommitTree(ctx, tree2, []string{c1}, "two\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c1); err != nil {
		t.Fatal(err)
	}

	got := testkit.Modes(t, r, "refs/heads/box")
	want := map[string]string{
		"config/automations.yaml/kitchen.yaml": "100644",
		"config/with spaces/ñandú.yaml":        "100644",
		"config/scripts/run.sh":                "100755",
	}
	if len(got) != len(want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
	for p, m := range want {
		if got[p] != m {
			t.Errorf("%s: mode %q, want %q", p, got[p], m)
		}
	}
}

func TestUpdateRefIsCompareAndSwap(t *testing.T) {
	r := testkit.NewBareRepo(t)
	tree, _ := r.BuildTree(ctx, "", nil)
	c1, _ := r.CommitTree(ctx, tree, nil, "1\n", ident(t), ident(t))
	c2, _ := r.CommitTree(ctx, tree, []string{c1}, "2\n", ident(t), ident(t))

	if err := r.UpdateRef(ctx, "refs/heads/box", c1, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, ""); !errors.Is(err, gitx.ErrRefMoved) {
		t.Errorf("creating a ref that already exists: wanted ErrRefMoved, got %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c2); !errors.Is(err, gitx.ErrRefMoved) {
		t.Errorf("wrong old: wanted ErrRefMoved, got %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c1); err != nil {
		t.Errorf("correct old: %v", err)
	}
}

func TestResolveMissingRef(t *testing.T) {
	r := testkit.NewBareRepo(t)
	if oid, err := r.ResolveRef(ctx, "refs/heads/box"); err != nil || oid != "" {
		t.Errorf("missing ref = %q, %v", oid, err)
	}
}

func TestBlobOIDMatchesGit(t *testing.T) {
	r := testkit.NewBareRepo(t)
	for _, s := range []string{"", "hello\n", "no newline", "with\r\ncrlf\r\n", strings.Repeat("x", 100000)} {
		got, err := r.WriteBlob(ctx, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if want := scan.BlobOID([]byte(s)); got != want {
			t.Errorf("BlobOID(%.20q) = %s, git says %s", s, want, got)
		}
		if ok, err := r.HasObject(ctx, got); err != nil || !ok {
			t.Errorf("HasObject(%s) = %v, %v", got, ok, err)
		}
	}
	if ok, err := r.HasObject(ctx, strings.Repeat("ab", 20)); err != nil || ok {
		t.Errorf("HasObject of a missing object = %v, %v", ok, err)
	}
}

func TestRequireVersion(t *testing.T) {
	if err := testkit.NewBareRepo(t).RequireVersion(ctx); err != nil {
		t.Fatal(err)
	}
}
