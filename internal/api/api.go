// Package api is the engine's small HTTP API (design.md §12): state, pending
// changes, conflict, "sync now" and deploy key. It is meant for the ingress
// UI (v2) and the HACS integration (v3).
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/config"
	"github.com/MaximilianoFelice/homeassistant-config-sync/internal/engine"
)

// IngressIP is the IP the Supervisor proxies ingress from.
const IngressIP = "172.30.32.2"

// jobTimeout bounds a cycle started by the API or the scheduler.
const jobTimeout = 15 * time.Minute

// maxJobs is how many finished jobs are remembered.
const maxJobs = 20

// ErrBusy means a job is already running.
var ErrBusy = errors.New("a sync is already running")

// Job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// SyncFunc runs a cycle. Normally it is Engine.Sync.
type SyncFunc func(ctx context.Context, so engine.SyncOptions) (*engine.Report, error)

// Job is a cycle started by the API, the scheduler or stdin.
type Job struct {
	ID       string             `json:"id"`
	Trigger  string             `json:"trigger"`
	Status   string             `json:"status"`
	Started  time.Time          `json:"started"`
	Finished time.Time          `json:"finished,omitzero"`
	Options  engine.SyncOptions `json:"options"`
	Report   *engine.Report     `json:"report,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// Options configures the server.
type Options struct {
	Config   *config.Config
	StateDir string
	Sync     SyncFunc
	// Token is the bearer token for requests that do not come from ingress.
	// Empty = only ingress is accepted.
	Token string
	// TrustedIPs are accepted without a token (by default, only IngressIP).
	TrustedIPs []string
	// OnJob is called when each job finishes (logs).
	OnJob func(*Job)
}

// Server is the API server and the owner of the jobs.
type Server struct {
	opts    Options
	trusted map[string]bool

	mu      sync.Mutex
	running *Job
	jobs    []*Job
	wg      sync.WaitGroup
}

// New creates the server.
func New(opts Options) *Server {
	s := &Server{opts: opts, trusted: map[string]bool{}}
	ips := opts.TrustedIPs
	if ips == nil {
		ips = []string{IngressIP}
	}
	for _, ip := range ips {
		s.trusted[ip] = true
	}
	return s
}

// Trigger starts a cycle in the background. It returns ErrBusy if one is
// already running.
func (s *Server) Trigger(trigger string, so engine.SyncOptions) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != nil {
		return nil, ErrBusy
	}
	j := &Job{ID: newID(), Trigger: trigger, Status: JobRunning, Started: time.Now(), Options: so}
	s.running = j
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > maxJobs {
		s.jobs = s.jobs[len(s.jobs)-maxJobs:]
	}
	s.wg.Add(1)
	go s.run(j)
	return s.snapshot(j), nil
}

func (s *Server) run(j *Job) {
	defer s.wg.Done()
	// A cycle is never cancelled midway by a shutdown: cutting an apply short
	// leaves the journal to recover from, but it is better not to force it.
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	rep, err := s.opts.Sync(ctx, j.Options)

	s.mu.Lock()
	j.Finished, j.Report, j.Status = time.Now(), rep, JobDone
	if err != nil {
		j.Status, j.Error = JobFailed, err.Error()
	}
	s.running = nil
	done := s.snapshot(j)
	s.mu.Unlock()
	if s.opts.OnJob != nil {
		s.opts.OnJob(done)
	}
}

// Wait waits for the running job to finish (graceful shutdown).
func (s *Server) Wait() { s.wg.Wait() }

// Job returns a copy of a job by ID.
func (s *Server) Job(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return s.snapshot(j), true
		}
	}
	return nil, false
}

// Running returns a copy of the running job, or nil.
func (s *Server) Running() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		return nil
	}
	return s.snapshot(s.running)
}

// snapshot copies a job; it is called with s.mu held.
func (s *Server) snapshot(j *Job) *Job {
	c := *j
	return &c
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler returns the API's http.Handler, with authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/pending", s.pending)
	mux.HandleFunc("GET /api/v1/conflict", s.conflict)
	mux.HandleFunc("POST /api/v1/sync", s.sync)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.job)
	mux.HandleFunc("GET /api/v1/deploy-key", s.deployKey)
	mux.HandleFunc("POST /api/v1/deploy-key/rotate", s.rotateKey)
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil && s.trusted[host] {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.opts.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "a valid token is required (Authorization: Bearer …)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StatusResponse is the response of GET /api/v1/status.
type StatusResponse struct {
	*engine.State
	// Syncing means a cycle is running right now.
	Syncing bool `json:"syncing"`
	Job     *Job `json:"job,omitempty"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st, err := engine.LoadState(s.opts.StateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st.Cache = nil
	resp := StatusResponse{State: st, Job: s.Running()}
	resp.Syncing = resp.Job != nil
	writeJSON(w, http.StatusOK, resp)
}

// pending runs a synchronous dry-run: what the box would push and what would
// come down from main.
func (s *Server) pending(w http.ResponseWriter, r *http.Request) {
	if s.Running() != nil {
		writeError(w, http.StatusConflict, ErrBusy.Error())
		return
	}
	rep, err := s.opts.Sync(r.Context(), engine.SyncOptions{DryRun: true})
	switch {
	case errors.Is(err, engine.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}

// ConflictResponse is the response of GET /api/v1/conflict.
type ConflictResponse struct {
	Conflict bool     `json:"conflict"`
	Paths    []string `json:"paths,omitempty"`
	Box      string   `json:"box,omitempty"`
	Main     string   `json:"main,omitempty"`
	// CompareURL is the GitHub main...box compare, if the remote is GitHub.
	CompareURL string `json:"compare_url,omitempty"`
}

func (s *Server) conflict(w http.ResponseWriter, _ *http.Request) {
	st, err := engine.LoadState(s.opts.StateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := ConflictResponse{Conflict: st.State == engine.StateConflict}
	if resp.Conflict {
		resp.Paths, resp.Box, resp.Main = st.ConflictPaths, st.BoxHead, st.MainHead
		resp.CompareURL = CompareURL(s.opts.Config.Repository, s.opts.Config.MainBranch, s.opts.Config.BoxBranch)
	}
	writeJSON(w, http.StatusOK, resp)
}

// SyncRequest is the optional body of POST /api/v1/sync.
type SyncRequest struct {
	DryRun          bool `json:"dry_run"`
	AcceptBootstrap bool `json:"accept_bootstrap"`
	AllowMassDelete bool `json:"allow_mass_delete"`
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	var req SyncRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	j, err := s.Trigger("api", engine.SyncOptions{DryRun: req.DryRun, AcceptBootstrap: req.AcceptBootstrap, AllowMassDelete: req.AllowMassDelete})
	if errors.Is(err, ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": s.Running()})
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Job(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) deployKey(w http.ResponseWriter, _ *http.Request) {
	pub, err := engine.DeployKey(s.opts.StateDir)
	if err != nil {
		writeError(w, http.StatusNotFound, "no deploy key: not initialized yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": pub})
}

// rotateKey generates a new deploy key. The old one keeps working on GitHub
// until the user removes it, but the app stops using it right away.
func (s *Server) rotateKey(w http.ResponseWriter, _ *http.Request) {
	if s.Running() != nil {
		writeError(w, http.StatusConflict, ErrBusy.Error())
		return
	}
	pub, err := engine.RotateDeployKey(s.opts.StateDir)
	switch {
	case errors.Is(err, engine.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"public_key": pub})
	}
}

var githubRepo = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/)([^/]+)/(.+?)(?:\.git)?$`)

// CompareURL builds the link to the GitHub compare, or "" if the remote is
// not GitHub.
func CompareURL(repo, main, box string) string {
	m := githubRepo.FindStringSubmatch(repo)
	if m == nil {
		return ""
	}
	return "https://github.com/" + m[1] + "/" + m[2] + "/compare/" + main + "..." + box
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
