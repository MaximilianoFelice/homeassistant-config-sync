package api

import (
	"bufio"
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

func newServer(t *testing.T, f *fake, state string) *Server {
	t.Helper()
	dir := t.TempDir()
	if state != "" {
		testkit.WriteFile(t, filepath.Join(dir, "state.json"), state)
	}
	cfg := config.Default()
	cfg.Repository = "https://github.com/you/my-home-ha.git"
	return New(Options{Config: cfg, StateDir: dir, Sync: f.sync, Token: tok})
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
	s := newServer(t, &fake{}, `{"state": "idle"}`)
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
	s := newServer(t, f, `{"state": "conflict", "box_head": "abc", "cache": {"config/a.yaml": {"size": 1, "oid": "x"}}}`)
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
	s := newServer(t, f, "")
	h := s.Handler()

	rec, out := do(t, h, "POST", "/api/v1/sync", `{"dry_run": true}`, true)
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
	if so := f.lastOpt.Load().(engine.SyncOptions); !so.DryRun {
		t.Errorf("dry_run did not arrive: %+v", so)
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
	s := newServer(t, &fake{}, "")
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
	s := newServer(t, f, "")
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
	s := newServer(t, f, "")
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
	s := newServer(t, f, "")
	rec, out := do(t, s.Handler(), "GET", "/api/v1/pending", "", true)
	if rec.Code != 200 || out["dry_run"] != true {
		t.Errorf("pending = %d %v", rec.Code, out)
	}
	if so := f.lastOpt.Load().(engine.SyncOptions); !so.DryRun {
		t.Error("pending must be a dry-run")
	}
}

func TestConflictEndpoint(t *testing.T) {
	s := newServer(t, &fake{}, `{"state": "conflict", "conflict_paths": ["homeassistant/automations.yaml"], "box_head": "b1", "main_head": "m1"}`)
	_, out := do(t, s.Handler(), "GET", "/api/v1/conflict", "", true)
	if out["conflict"] != true || out["compare_url"] != "https://github.com/you/my-home-ha/compare/main...box" {
		t.Errorf("conflict = %v", out)
	}
	s2 := newServer(t, &fake{}, `{"state": "idle"}`)
	if _, out := do(t, s2.Handler(), "GET", "/api/v1/conflict", "", true); out["conflict"] != false || out["paths"] != nil {
		t.Errorf("without conflict = %v", out)
	}
}

func TestCompareURL(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/you/home.git":  "https://github.com/you/home/compare/main...box",
		"https://github.com/you/home":      "https://github.com/you/home/compare/main...box",
		"https://gitea.local/you/home.git": "",
		"file:///tmp/origin.git":           "",
	} {
		if got := CompareURL(repo, "main", "box"); got != want {
			t.Errorf("CompareURL(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestEventStreamSendsJobEvents(t *testing.T) {
	f := &fake{block: make(chan struct{}), report: engine.Report{State: engine.StateIdle}}
	s := newServer(t, f, "")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(prefix string) string {
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream closed waiting for %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-ctx.Done():
				t.Fatalf("timeout waiting for %q", prefix)
			}
		}
	}
	next("event: hello")
	j, _ := s.Trigger("test", engine.SyncOptions{})
	next("event: job")
	if l := next("data: "); !strings.Contains(l, `"status":"running"`) || !strings.Contains(l, j.ID) {
		t.Errorf("start event = %s", l)
	}
	close(f.block)
	next("event: job")
	if l := next("data: "); !strings.Contains(l, `"status":"done"`) {
		t.Errorf("end event = %s", l)
	}

	// Without a token, no stream.
	req2, _ := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("without token = %d", resp2.StatusCode)
	}
}

func TestUIIsServedWithoutAuth(t *testing.T) {
	s := newServer(t, &fake{}, "")
	rec, _ := do(t, s.Handler(), "GET", "/", "", false)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the static UI must not require the API token")
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET / = %d", rec.Code)
	}
}

func TestSchedulerTriggersAndSkipsWhenBusy(t *testing.T) {
	f := &fake{}
	s := newServer(t, f, "")
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
	s2 := newServer(t, f2, "")
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
	s := newServer(t, f, "")
	RunScheduler(context.Background(), s, 0, nil) // returns right away
	if f.calls.Load() != 0 {
		t.Error("interval 0 = manual only")
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
	fsRoot := filepath.Join(root, "fs")
	testkit.WriteFile(t, filepath.Join(fsRoot, "homeassistant", "automations.yaml"), "[]\n")
	cfg := config.Default()
	cfg.Repository = "file://" + origin.Dir()
	stateDir := filepath.Join(root, "state")
	e, err := engine.New(engine.Options{Config: cfg, StateDir: stateDir, FSRoot: fsRoot, AllowFileRemote: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Init(ctx); err != nil {
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
	// The first sync pushes only box and waits for the user.
	if j.Status != JobDone || j.Report.State != engine.StateReview {
		t.Fatalf("job = %+v", j)
	}
	if got := testkit.Tree(t, origin, "refs/heads/box")["homeassistant/automations.yaml"]; got != "[]\n" {
		t.Errorf("box/automations = %q", got)
	}
	if main := testkit.Tree(t, origin, "refs/heads/main"); len(main) != 0 {
		t.Errorf("main must not be created by the app: %v", main)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); err != nil {
		t.Error("the job must persist the state")
	}
}
