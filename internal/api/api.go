// Package api serves the REST + SSE surface the web UI and any gh-alike client
// read. Field names mirror the GitHub Actions API where an equivalent exists;
// everything this platform knows and GitHub does not (failure class, cancel
// reason, phase timing) is added alongside under its own name.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Controller is the scheduler's side of the cancel/re-run actions. The API
// never mutates run state itself; it validates, records the reason, and calls
// through.
type Controller interface {
	Cancel(ctx context.Context, runID int64, reason model.CancelReason) error
	CancelJob(ctx context.Context, jobID int64, reason model.CancelReason) error
	Rerun(ctx context.Context, runID int64, actor string) error
	RerunFailed(ctx context.Context, runID int64, actor string) error
	RerunJob(ctx context.Context, jobID int64, actor string) error
}

// LogSource reads and tails one job attempt's log.
type LogSource interface {
	Read(ctx context.Context, jobID int64, attempt int, fromSeq int64, limit int) ([]model.LogLine, error)
	// Subscribe delivers every line with Seq >= fromSeq and then live lines,
	// closing the channel when the attempt is finished.
	Subscribe(ctx context.Context, jobID int64, attempt int, fromSeq int64) (<-chan model.LogLine, error)
}

// BlobStore hands back artifact bytes. Optional: without one, the download
// endpoint answers 503 rather than pretending the artifact is empty.
type BlobStore interface {
	Open(ctx context.Context, a *model.Artifact) (io.ReadCloser, error)
}

// CacheLister is an optional store capability. store.Caches has no
// list-entries method, so the cache page falls back to reconstructing entries
// from the event log and SAYS SO in the response.
type CacheLister interface {
	ListCacheEntries(ctx context.Context, repoID int64) ([]*model.CacheEntry, error)
}

// SubsystemState is the health of one named part of the control plane.
type SubsystemState string

const (
	StateOK       SubsystemState = "ok"
	StateDegraded SubsystemState = "degraded"
	StateDown     SubsystemState = "down"
)

// Subsystem is one line of /healthz.
type Subsystem struct {
	Name   string         `json:"name"`
	State  SubsystemState `json:"state"`
	Detail string         `json:"detail,omitempty"`
}

// HealthReporter contributes subsystem health beyond what the API can see for
// itself. Nil is fine; the store's own reachability is always reported.
type HealthReporter interface {
	Subsystems(ctx context.Context) []Subsystem
}

// Config wires the server. Store is required; everything else degrades to a
// loud error on the endpoints that need it.
type Config struct {
	Store      store.Store
	Controller Controller
	Logs       LogSource
	Blobs      BlobStore
	Health     HealthReporter

	// Now is injected so tests get deterministic timing output.
	Now func() time.Time
	// Actor names the principal performing a cancel or re-run. The default
	// reads X-CI-Actor and falls back to "operator"; the lead replaces it with
	// the authenticated identity once auth lands.
	Actor func(*http.Request) string
	// SSEHeartbeat is how often an SSE stream emits a comment so proxies do
	// not close an idle tail.
	SSEHeartbeat time.Duration
	// MaxPerPage caps per_page.
	MaxPerPage int
}

// Server implements http.Handler over Config.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the server and registers every route.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Actor == nil {
		cfg.Actor = defaultActor
	}
	if cfg.SSEHeartbeat <= 0 {
		cfg.SSEHeartbeat = 15 * time.Second
	}
	if cfg.MaxPerPage <= 0 {
		cfg.MaxPerPage = 100
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

func defaultActor(r *http.Request) string {
	if a := r.Header.Get("X-CI-Actor"); a != "" {
		return a
	}
	return "operator"
}

func (s *Server) now() time.Time { return s.cfg.Now() }

// ServeHTTP routes the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Handler exposes the mux so the lead can mount the UI alongside it.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/runs", s.listRuns)
	m.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	m.HandleFunc("GET /api/v1/runs/{id}/jobs", s.listRunJobs)
	m.HandleFunc("GET /api/v1/runs/{id}/artifacts", s.listRunArtifacts)
	m.HandleFunc("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	m.HandleFunc("POST /api/v1/runs/{id}/rerun", s.rerunRun)
	m.HandleFunc("POST /api/v1/runs/{id}/rerun-failed", s.rerunRunFailed)

	m.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	m.HandleFunc("GET /api/v1/jobs/{id}/logs", s.getJobLogs)
	m.HandleFunc("GET /api/v1/jobs/{id}/logs/stream", s.streamJobLogs)
	m.HandleFunc("GET /api/v1/jobs/{id}/logs/raw", s.rawJobLogs)
	m.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.cancelJob)
	m.HandleFunc("POST /api/v1/jobs/{id}/rerun", s.rerunJob)

	m.HandleFunc("GET /api/v1/runners", s.listRunners)
	m.HandleFunc("GET /api/v1/queue", s.getQueue)
	m.HandleFunc("GET /api/v1/queue/history", s.getQueueHistory)

	m.HandleFunc("GET /api/v1/artifacts/{id}/download", s.downloadArtifact)
	m.HandleFunc("GET /api/v1/repos/{owner}/{repo}/cache", s.getRepoCache)

	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /.well-known/docker-updater/health", s.updaterHealth)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own DTO cannot fail on valid data; if it does the
		// response body would be a lie, so say so with a 500.
		http.Error(w, `{"error":"failed to encode response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
