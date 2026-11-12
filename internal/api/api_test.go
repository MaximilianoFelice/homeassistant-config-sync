package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/gitx"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/testkit"
)

const tok = "test-token"

// fake is a controllable SyncFunc: it counts calls and can block.
type fake struct {
	calls   atomic.Int32
	block   chan struct{}
	report  engine.Report
	err     error
	lastOpt atomic.Value
}

func (f *fake) sync(_ context.Context, so engine.SyncOptions) (*engine.Report, error) {
	f.calls.Add(1)
	f.lastOpt.Store(so)
	if f.block != nil {
		<-f.block
	}
	r := f.report
	r.DryRun = so.DryRun
	return &r, f.err
}

func newServer(t *testing.T, f *fake, state string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if state != "" {
		testkit.WriteFile(t, filepath.Join(dir, "state.json"), state)
	}
	cfg := config.Default()
	cfg.Repository = "git@github.com:you/my-home-ha.git"
	return New(Options{Config: cfg, StateDir: dir, Sync: f.sync, Token: tok}), dir
}

func do(t *testing.T, h http.Handler, method, path, body string, auth bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body == "" {
		req.ContentLength = 0
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func waitJob(t *testing.T, s *Server, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := s.Job(id); ok && j.Status != JobRunning {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

func TestAuth(t *testing.T) {
	s, _ := newServer(t, &fake{}, `{"state": "idle"}`)
	h := s.Handler()

	if rec, _ := do(t, h, "GET", "/api/v1/status", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("without token = %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer other")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/api/v1/status", "", true); rec.Code != http.StatusOK {
		t.Errorf("with token = %d", rec.Code)
	}

	// Ingress: the Supervisor proxy sends no token.
	req = httptest.NewRequest("GET", "/api/v1/status", nil)
	req.RemoteAddr = IngressIP + ":54321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("from ingress = %d", rec.Code)
	}

	// Without a configured token, only ingress.
	s2 := New(Options{Config: config.Default(), StateDir: t.TempDir(), Sync: (&fake{}).sync})
	req = httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty token = %d", rec.Code)
	}
}

func TestStatusHidesCacheAndShowsRunningJob(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s, _ := newServer(t, f, `{"state": "conflict", "box_head": "abc", "cache": {"config/a.yaml": {"size": 1, "oid": "x"}}}`)
	h := s.Handler()

	rec, out := do(t, h, "GET", "/api/v1/status", "", true)
	if rec.Code != 200 || out["state"] != "conflict" || out["syncing"] != false || out["box_head"] != "abc" {
		t.Errorf("status = %d %v", rec.Code, out)
	}
	if _, ok := out["cache"]; ok {
		t.Error("the stat cache is not exposed through the API")
	}

	j, err := s.Trigger("test", engine.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, out = do(t, h, "GET", "/api/v1/status", "", true)
	if out["syncing"] != true {
		t.Errorf("syncing = %v", out["syncing"])
	}
	close(f.block)
	waitJob(t, s, j.ID)
}

func TestSyncJobLifecycle(t *testing.T) {
	f := &fake{report: engine.Report{State: engine.StateIdle}}
	s, _ := newServer(t, f, "")
	h := s.Handler()

	rec, out := do(t, h, "POST", "/api/v1/sync", `{"accept_bootstrap": true}`, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST sync = %d %v", rec.Code, out)
	}
	id, _ := out["job_id"].(string)
	if rec.Header().Get("Location") != "/api/v1/jobs/"+id {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	j := waitJob(t, s, id)
	if j.Status != JobDone || j.Report.State != engine.StateIdle || j.Trigger != "api" {
		t.Errorf("job = %+v", j)
	}
	if so := f.lastOpt.Load().(engine.SyncOptions); !so.AcceptBootstrap {
		t.Errorf("accept_bootstrap did not arrive: %+v", so)
	}
	rec, out = do(t, h, "GET", "/api/v1/jobs/"+id, "", true)
	if rec.Code != 200 || out["status"] != JobDone {
		t.Errorf("GET job = %d %v", rec.Code, out)
	}
	if rec, _ := do(t, h, "GET", "/api/v1/jobs/nope", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("nonexistent job = %d", rec.Code)
	}
}

func TestSyncWithoutBodyAndBadBody(t *testing.T) {
	s, _ := newServer(t, &fake{}, "")
	h := s.Handler()
	rec, out := do(t, h, "POST", "/api/v1/sync", "", true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("without body = %d", rec.Code)
	}
	waitJob(t, s, out["job_id"].(string))
	if rec, _ := do(t, h, "POST", "/api/v1/sync", `{"force": true}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/api/v1/sync", "", true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /sync = %d", rec.Code)
	}
}

func TestOnlyOneJobAtATime(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s, _ := newServer(t, f, "")
	h := s.Handler()
	_, out := do(t, h, "POST", "/api/v1/sync", "", true)
	id := out["job_id"].(string)

	if rec, out := do(t, h, "POST", "/api/v1/sync", "", true); rec.Code != http.StatusConflict || out["job"] == nil {
		t.Errorf("second sync = %d %v", rec.Code, out)
	}
	if rec, _ := do(t, h, "GET", "/api/v1/pending", "", true); rec.Code != http.StatusConflict {
		t.Errorf("pending during a sync = %d", rec.Code)
	}
	if _, err := s.Trigger("schedule", engine.SyncOptions{}); !errors.Is(err, ErrBusy) {
		t.Errorf("trigger = %v", err)
	}
	close(f.block)
	waitJob(t, s, id)
	if f.calls.Load() != 1 {
		t.Errorf("calls = %d", f.calls.Load())
	}
}

func TestFailedJob(t *testing.T) {
	f := &fake{err: errors.New("the network is down")}
	var seen atomic.Value
	s, _ := newServer(t, f, "")
	s.opts.OnJob = func(j *Job) { seen.Store(j.Status) }
	j, _ := s.Trigger("test", engine.SyncOptions{})
	done := waitJob(t, s, j.ID)
	if done.Status != JobFailed || done.Error != "the network is down" {
		t.Errorf("job = %+v", done)
	}
	s.Wait()
	if seen.Load() != JobFailed {
		t.Errorf("OnJob saw %v", seen.Load())
	}
}

func TestPendingIsADryRun(t *testing.T) {
	f := &fake{report: engine.Report{State: engine.StateIdle}}
	s, _ := newServer(t, f, "")
	rec, out := do(t, s.Handler(), "GET", "/api/v1/pending", "", true)
	if rec.Code != 200 || out["dry_run"] != true {
		t.Errorf("pending = %d %v", rec.Code, out)
	}
	if so := f.lastOpt.Load().(engine.SyncOptions); !so.DryRun {
		t.Error("pending must be a dry-run")
	}
}

func TestConflictEndpoint(t *testing.T) {
	s, _ := newServer(t, &fake{}, `{"state": "conflict", "conflict_paths": ["config/automations.yaml"], "box_head": "b1", "main_head": "m1"}`)
	_, out := do(t, s.Handler(), "GET", "/api/v1/conflict", "", true)
	if out["conflict"] != true || out["compare_url"] != "https://github.com/you/my-home-ha/compare/main...box" {
		t.Errorf("conflict = %v", out)
	}
	s2, _ := newServer(t, &fake{}, `{"state": "idle"}`)
	if _, out := do(t, s2.Handler(), "GET", "/api/v1/conflict", "", true); out["conflict"] != false || out["paths"] != nil {
		t.Errorf("without conflict = %v", out)
	}
}

func TestCompareURL(t *testing.T) {
	for repo, want := range map[string]string{
		"git@github.com:you/home.git":       "https://github.com/you/home/compare/main...box",
		"git@github.com:you/home":           "https://github.com/you/home/compare/main...box",
		"ssh://git@github.com/you/home.git": "https://github.com/you/home/compare/main...box",
		"git@gitea.local:you/home.git":      "",
		"file:///tmp/origin.git":            "",
	} {
		if got := CompareURL(repo, "main", "box"); got != want {
			t.Errorf("CompareURL(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestDeployKeyEndpoint(t *testing.T) {
	s, dir := newServer(t, &fake{}, "")
	if rec, _ := do(t, s.Handler(), "GET", "/api/v1/deploy-key", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("without init = %d", rec.Code)
	}
	testkit.WriteFile(t, filepath.Join(dir, "ssh", "id_ed25519.pub"), "ssh-ed25519 AAAA homeassistant-config-sync\n")
	if _, out := do(t, s.Handler(), "GET", "/api/v1/deploy-key", "", true); out["public_key"] != "ssh-ed25519 AAAA homeassistant-config-sync" {
		t.Errorf("= %v", out)
	}
}

func TestRotateKeyEndpoint(t *testing.T) {
	s, dir := newServer(t, &fake{}, "")
	if rec, _ := do(t, s.Handler(), "POST", "/api/v1/deploy-key/rotate", "", true); rec.Code != http.StatusInternalServerError {
		t.Errorf("without a key = %d", rec.Code)
	}
	testkit.WriteFile(t, filepath.Join(dir, "ssh", "id_ed25519"), "old private\n")
	testkit.WriteFile(t, filepath.Join(dir, "ssh", "id_ed25519.pub"), "ssh-ed25519 OLD homeassistant-config-sync\n")
	rec, out := do(t, s.Handler(), "POST", "/api/v1/deploy-key/rotate", "", true)
	pub, _ := out["public_key"].(string)
	if rec.Code != http.StatusOK || !strings.HasPrefix(pub, "ssh-ed25519 ") || strings.Contains(pub, "OLD") {
		t.Errorf("rotate = %d %v", rec.Code, out)
	}

	// Not while a sync is running.
	f := &fake{block: make(chan struct{})}
	s2, _ := newServer(t, f, "")
	j, _ := s2.Trigger("test", engine.SyncOptions{})
	if rec, _ := do(t, s2.Handler(), "POST", "/api/v1/deploy-key/rotate", "", true); rec.Code != http.StatusConflict {
		t.Errorf("during a sync = %d", rec.Code)
	}
	close(f.block)
	waitJob(t, s2, j.ID)
}

func TestSchedulerTriggersAndSkipsWhenBusy(t *testing.T) {
	f := &fake{}
	s, _ := newServer(t, f, "")
	ctx, cancel := context.WithCancel(context.Background())
	go RunScheduler(ctx, s, 10*time.Millisecond, nil)
	deadline := time.Now().Add(5 * time.Second)
	for f.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	s.Wait()
	if f.calls.Load() < 3 {
		t.Errorf("the scheduler fired %d times", f.calls.Load())
	}

	// With a blocked sync, ticks are skipped.
	f2 := &fake{block: make(chan struct{})}
	s2, _ := newServer(t, f2, "")
	var skips atomic.Int32
	ctx2, cancel2 := context.WithCancel(context.Background())
	go RunScheduler(ctx2, s2, 10*time.Millisecond, func() { skips.Add(1) })
	for skips.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
	close(f2.block)
	s2.Wait()
	if f2.calls.Load() != 1 || skips.Load() < 2 {
		t.Errorf("calls = %d, skips = %d", f2.calls.Load(), skips.Load())
	}
}

func TestSchedulerDisabledWithZeroInterval(t *testing.T) {
	f := &fake{}
	s, _ := newServer(t, f, "")
	RunScheduler(context.Background(), s, 0, nil) // returns right away
	if f.calls.Load() != 0 {
		t.Error("interval 0 = manual only")
	}
}

func TestReadCommands(t *testing.T) {
	f := &fake{}
	s, _ := newServer(t, f, "")
	var unknown []string
	// HA sends the hassio.addon_stdin input as JSON: "sync" with quotes.
	ReadCommands(context.Background(), strings.NewReader("\"sync\"\n\nreboot\n"), s, func(c string) { unknown = append(unknown, c) }, nil)
	s.Wait()
	if f.calls.Load() != 1 || len(unknown) != 1 || unknown[0] != "reboot" {
		t.Errorf("calls = %d, unknown = %v", f.calls.Load(), unknown)
	}
}

// TestRealEngineThroughAPI starts the API on top of a real engine (local
// file:// remote) and runs an end-to-end sync over HTTP.
func TestRealEngineThroughAPI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin, err := gitx.InitBare(ctx, filepath.Join(root, "origin.git"), gitx.Options{AllowFile: true})
	if err != nil {
		t.Fatal(err)
	}
	box := filepath.Join(root, "homeassistant")
	testkit.WriteFile(t, filepath.Join(box, "automations.yaml"), "[]\n")
	cfg := config.Default()
	cfg.Repository = "file://" + origin.Dir()
	cfg.Mappings = cfg.Mappings[:1]
	cfg.Mappings[0].Box = box
	stateDir := filepath.Join(root, "state")
	e, err := engine.New(engine.Options{Config: cfg, StateDir: stateDir, AllowFileRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := engine.APIToken(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Config: cfg, StateDir: stateDir, Sync: e.Sync, Token: token})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/sync", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	j := waitJob(t, s, out["job_id"])
	if j.Status != JobDone || j.Report.State != engine.StateIdle {
		t.Fatalf("job = %+v", j)
	}
	if got := testkit.Tree(t, origin, "refs/heads/main")["config/automations.yaml"]; got != "[]\n" {
		t.Errorf("main/automations = %q", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); err != nil {
		t.Error("the job must persist the state")
	}
}
