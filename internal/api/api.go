// Package api es la API HTTP chica del motor (design.md §12): estado,
// cambios pendientes, conflicto, "sync now" y deploy key. Está pensada para
// la UI por ingress (v2) y la integración HACS (v3).
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

// IngressIP es la IP desde la que el Supervisor hace proxy de ingress.
const IngressIP = "172.30.32.2"

// jobTimeout acota un ciclo lanzado por la API o el scheduler.
const jobTimeout = 15 * time.Minute

// maxJobs es cuántos jobs terminados se recuerdan.
const maxJobs = 20

// ErrBusy indica que ya hay un job corriendo.
var ErrBusy = errors.New("ya hay un sync corriendo")

// Estados de un job.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// SyncFunc corre un ciclo. Normalmente es Engine.Sync.
type SyncFunc func(ctx context.Context, so engine.SyncOptions) (*engine.Report, error)

// Job es un ciclo lanzado por la API, el scheduler o stdin.
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

// Options configura el servidor.
type Options struct {
	Config   *config.Config
	StateDir string
	Sync     SyncFunc
	// Token es el bearer token para requests que no vienen de ingress.
	// Vacío = solo se acepta ingress.
	Token string
	// TrustedIPs se aceptan sin token (por defecto, solo IngressIP).
	TrustedIPs []string
	// OnJob se llama al terminar cada job (logs).
	OnJob func(*Job)
}

// Server es el servidor de la API y el dueño de los jobs.
type Server struct {
	opts    Options
	trusted map[string]bool

	mu      sync.Mutex
	running *Job
	jobs    []*Job
	wg      sync.WaitGroup
}

// New crea el servidor.
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

// Trigger lanza un ciclo en segundo plano. Devuelve ErrBusy si ya hay uno.
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
	// Un ciclo nunca se cancela a mitad de camino por un apagado: cortar un
	// apply deja el journal para recuperar, pero es mejor no forzarlo.
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

// Wait espera a que termine el job en curso (apagado prolijo).
func (s *Server) Wait() { s.wg.Wait() }

// Job devuelve una copia de un job por ID.
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

// Running devuelve una copia del job en curso, o nil.
func (s *Server) Running() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		return nil
	}
	return s.snapshot(s.running)
}

// snapshot copia un job; se llama con s.mu tomado.
func (s *Server) snapshot(j *Job) *Job {
	c := *j
	return &c
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler devuelve el http.Handler de la API, con autenticación.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/pending", s.pending)
	mux.HandleFunc("GET /api/v1/conflict", s.conflict)
	mux.HandleFunc("POST /api/v1/sync", s.sync)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.job)
	mux.HandleFunc("GET /api/v1/deploy-key", s.deployKey)
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
			writeError(w, http.StatusUnauthorized, "falta un token válido (Authorization: Bearer …)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StatusResponse es la respuesta de GET /api/v1/status.
type StatusResponse struct {
	*engine.State
	// Syncing indica que hay un ciclo corriendo ahora.
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

// pending corre un dry-run sincrónico: qué subiría el box y qué bajaría de main.
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

// ConflictResponse es la respuesta de GET /api/v1/conflict.
type ConflictResponse struct {
	Conflict bool     `json:"conflict"`
	Paths    []string `json:"paths,omitempty"`
	Box      string   `json:"box,omitempty"`
	Main     string   `json:"main,omitempty"`
	// CompareURL es el compare de GitHub main...box, si el remoto es GitHub.
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

// SyncRequest es el body opcional de POST /api/v1/sync.
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
			writeError(w, http.StatusBadRequest, "body inválido: "+err.Error())
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
		writeError(w, http.StatusNotFound, "no existe ese job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) deployKey(w http.ResponseWriter, _ *http.Request) {
	pub, err := engine.DeployKey(s.opts.StateDir)
	if err != nil {
		writeError(w, http.StatusNotFound, "no hay deploy key: falta inicializar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": pub})
}

var githubRepo = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/)([^/]+)/(.+?)(?:\.git)?$`)

// CompareURL arma el link al compare de GitHub, o "" si el remoto no es GitHub.
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
