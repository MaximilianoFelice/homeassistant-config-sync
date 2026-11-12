package gitx_test

import (
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

// gitServer serves bare repos over smart HTTP with git http-backend, behind
// basic auth that only accepts token.
func gitServer(t *testing.T, root, token string) *httptest.Server {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path:", err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenOverHTTP(t *testing.T) {
	origin := testkit.NewBareRepo(t)
	tree, _ := origin.BuildTree(ctx, "", nil)
	c, _ := origin.CommitTree(ctx, tree, nil, "init\n", ident(t), ident(t))
	if err := origin.UpdateRef(ctx, "refs/heads/main", c, ""); err != nil {
		t.Fatal(err)
	}
	srv := gitServer(t, filepath.Dir(origin.Dir()), "s3cret")
	url := srv.URL + "/" + filepath.Base(origin.Dir())

	askpass := filepath.Join(t.TempDir(), "askpass")
	if err := gitx.WriteAskPass(askpass); err != nil {
		t.Fatal(err)
	}
	open := func(token string) *gitx.Repo {
		return gitx.Open(testkit.NewBareRepo(t).Dir(), gitx.Options{AllowHTTP: true, Token: token, AskPass: askpass})
	}

	if _, err := open("wrong").LsRemote(ctx, url); !errors.Is(err, gitx.ErrAuth) {
		t.Errorf("wrong token: wanted ErrAuth, got %v", err)
	}
	if _, err := open("").LsRemote(ctx, url); !errors.Is(err, gitx.ErrAuth) {
		t.Errorf("no token: wanted ErrAuth, got %v", err)
	}

	r := open("s3cret")
	heads, err := r.LsRemote(ctx, url)
	if err != nil || heads["main"] != c {
		t.Fatalf("ls-remote = %v, %v", heads, err)
	}
	if err := r.Fetch(ctx, url, "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	c2, _ := r.CommitTree(ctx, tree, []string{c}, "two\n", ident(t), ident(t))
	if err := r.Push(ctx, url, c2+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, _ := origin.ResolveRef(ctx, "refs/heads/main"); got != c2 {
		t.Errorf("origin main = %s, want %s", got, c2)
	}
	// The token never reaches the arguments or the config: only the
	// askpass program's environment.
	for _, e := range r.Env() {
		if strings.Contains(e, "s3cret") && !strings.HasPrefix(e, "HCS_GIT_TOKEN=") {
			t.Errorf("token leaked into %q", e)
		}
	}
}
