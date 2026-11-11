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
	testkit.WriteFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = Usuario Real\n[core]\n\thooksPath = /tmp/hooks\n")
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GIT_DIR", "/nope")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))

	r := testkit.NewBareRepo(t)
	env := r.Env()
	for _, bad := range []string{"SSH_AUTH_SOCK=", "GIT_DIR=/nope", "HOME=" + home} {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, bad) }) {
			t.Errorf("el entorno de git hereda %q", bad)
		}
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(env, want) {
			t.Errorf("falta %q en el entorno", want)
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
			t.Errorf("GIT_SSH_COMMAND no tiene %q: %s", want, ssh)
		}
	}

	// Y git realmente no lee el ~/.gitconfig del usuario: si lo leyera, este
	// insteadOf reescribiría file:// a https:// y el fetch fallaría.
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
		t.Errorf("el fetch falló: ¿se leyó el ~/.gitconfig del usuario? %v", err)
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
			t.Errorf("fetch %s: quería 'transport not allowed' sin tocar la red, dio %v", url, err)
		}
	}
}

func TestFileRemoteOnlyWhenAllowed(t *testing.T) {
	origin := testkit.NewBareRepo(t)
	blob, err := origin.WriteBlob(ctx, []byte("hola\n"))
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
		t.Error("file:// debería estar bloqueado sin AllowFile")
	}

	allowed := testkit.NewBareRepo(t)
	if err := allowed.Fetch(ctx, "file://"+origin.Dir(), "refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	got, err := allowed.ResolveRef(ctx, "refs/remotes/origin/main")
	if err != nil || got != c {
		t.Errorf("origin/main = %q (%v), quería %s", got, err, c)
	}
}

func TestBuildTreeCommitAndList(t *testing.T) {
	r := testkit.NewBareRepo(t)
	a, _ := r.WriteBlob(ctx, []byte("a\n"))
	b, _ := r.WriteBlob(ctx, []byte("#!/bin/sh\n"))
	tree, err := r.BuildTree(ctx, "", []gitx.IndexUpdate{
		{Path: "config/automations.yaml", Mode: "100644", OID: a},
		{Path: "config/con espacios/ñandú.yaml", Mode: "100644", OID: a},
		{Path: "config/scripts/run.sh", Mode: "100755", OID: b},
	})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := r.CommitTree(ctx, tree, nil, "uno\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c1, ""); err != nil {
		t.Fatal(err)
	}

	// Archivo → directorio y un borrado, sobre la base anterior.
	tree2, err := r.BuildTree(ctx, tree, []gitx.IndexUpdate{
		{Path: "config/automations.yaml"},
		{Path: "config/automations.yaml/cocina.yaml", Mode: "100644", OID: a},
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.CommitTree(ctx, tree2, []string{c1}, "dos\n", ident(t), ident(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c1); err != nil {
		t.Fatal(err)
	}

	got := testkit.Modes(t, r, "refs/heads/box")
	want := map[string]string{
		"config/automations.yaml/cocina.yaml": "100644",
		"config/con espacios/ñandú.yaml":      "100644",
		"config/scripts/run.sh":               "100755",
	}
	if len(got) != len(want) {
		t.Fatalf("árbol = %v, quería %v", got, want)
	}
	for p, m := range want {
		if got[p] != m {
			t.Errorf("%s: modo %q, quería %q", p, got[p], m)
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
		t.Errorf("crear un ref que ya existe: quería ErrRefMoved, dio %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c2); !errors.Is(err, gitx.ErrRefMoved) {
		t.Errorf("old equivocado: quería ErrRefMoved, dio %v", err)
	}
	if err := r.UpdateRef(ctx, "refs/heads/box", c2, c1); err != nil {
		t.Errorf("old correcto: %v", err)
	}
}

func TestResolveMissingRef(t *testing.T) {
	r := testkit.NewBareRepo(t)
	if oid, err := r.ResolveRef(ctx, "refs/heads/box"); err != nil || oid != "" {
		t.Errorf("ref inexistente = %q, %v", oid, err)
	}
}

func TestBlobOIDMatchesGit(t *testing.T) {
	r := testkit.NewBareRepo(t)
	for _, s := range []string{"", "hola\n", "sin newline", "con\r\ncrlf\r\n", strings.Repeat("x", 100000)} {
		got, err := r.WriteBlob(ctx, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if want := scan.BlobOID([]byte(s)); got != want {
			t.Errorf("BlobOID(%.20q) = %s, git dice %s", s, want, got)
		}
		if ok, err := r.HasObject(ctx, got); err != nil || !ok {
			t.Errorf("HasObject(%s) = %v, %v", got, ok, err)
		}
	}
	if ok, err := r.HasObject(ctx, strings.Repeat("ab", 20)); err != nil || ok {
		t.Errorf("HasObject de un objeto inexistente = %v, %v", ok, err)
	}
}

func TestRequireVersion(t *testing.T) {
	if err := testkit.NewBareRepo(t).RequireVersion(ctx); err != nil {
		t.Fatal(err)
	}
}
