// Package runnerapi serves the control-plane side of internal/protocol: the
// endpoints runner agents register, acquire work, heartbeat, stream logs, and
// report results on.
//
// Two invariants live here rather than in the scheduler, because this is where
// the wire meets them. A heartbeat is the only channel by which a cancellation
// reaches a running job, so it always carries the reason. And a runner that has
// lost its lease is told so explicitly rather than being allowed to finish and
// report a result for a job somebody else is already running.
package runnerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Scheduler is the part of the scheduler this server drives.
type Scheduler interface {
	Acquire(ctx context.Context, runnerID string, labels []string, now time.Time) (*protocol.Assignment, error)
	JobCompleted(ctx context.Context, jobID int64, res SchedulerResult) error
	JobSetupCompleted(ctx context.Context, jobID int64, at time.Time) error
	ReleaseJob(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error
}

// SchedulerResult mirrors scheduler.Result. It is restated rather than imported
// so this package does not depend on the scheduler's concrete type.
type SchedulerResult struct {
	Conclusion        model.Conclusion
	Class             model.FailureClass
	ClassReason       string
	Explanation       string
	Outputs           map[string]string
	Cancel            *model.CancelReason
	ClassificationLog []string
}

// Logs receives streamed job output.
type Logs interface {
	Append(ctx context.Context, jobID int64, attempt int, lines []model.LogLine) (int64, error)
	Finalize(ctx context.Context, jobID int64, attempt int) error
}

// Options configures the server.
type Options struct {
	Store     store.Store
	Scheduler Scheduler
	Logs      Logs

	// Token authenticates agents. Required: an unauthenticated runner endpoint
	// would let anything on the network claim a job and read its secrets.
	Token string

	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	LogFlushInterval  time.Duration
	// AcquireWait bounds how long a long-poll is held open.
	AcquireWait time.Duration
	// PollInterval is how often a held-open acquire re-checks the queue.
	PollInterval time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

// Server is the runner-facing HTTP surface.
type Server struct {
	opts Options
	mux  *http.ServeMux
	log  *slog.Logger

	// cancels holds pending cancellations keyed by job id, delivered on the
	// job's next heartbeat.
	mu      sync.Mutex
	cancels map[int64]model.CancelReason
}

// New builds the server. Missing required options are an error, never a
// permissive default: an unauthenticated runner API is not a degraded mode.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("runnerapi: Store is required")
	case opts.Scheduler == nil:
		return nil, errors.New("runnerapi: Scheduler is required")
	case opts.Logs == nil:
		return nil, errors.New("runnerapi: Logs is required")
	case opts.Token == "":
		return nil, errors.New("runnerapi: Token is required; an unauthenticated runner endpoint would let anything on the network claim a job and read its secrets")
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 90 * time.Second
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = opts.LeaseTTL / 4
	}
	if opts.HeartbeatInterval >= opts.LeaseTTL {
		return nil, fmt.Errorf("runnerapi: heartbeat interval %s must be shorter than lease TTL %s, or every running job loses its lease",
			opts.HeartbeatInterval, opts.LeaseTTL)
	}
	if opts.LogFlushInterval <= 0 {
		opts.LogFlushInterval = 2 * time.Second
	}
	if opts.AcquireWait <= 0 {
		opts.AcquireWait = 25 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	s := &Server{opts: opts, mux: http.NewServeMux(), log: opts.Logger, cancels: map[int64]model.CancelReason{}}
	s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Handler returns the mux, for mounting alongside the other surfaces.
func (s *Server) Handler() http.Handler { return s.mux }

// RequestCancel queues a cancellation for delivery on the job's next
// heartbeat. The reason is validated here so an unexplained cancellation cannot
// even be queued.
func (s *Server) RequestCancel(jobID int64, reason model.CancelReason) error {
	if err := reason.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[jobID] = reason
	return nil
}

func (s *Server) routes() {
	post := func(path string, h func(http.ResponseWriter, *http.Request)) {
		s.mux.HandleFunc("POST "+path, s.authenticated(h))
	}
	post(protocol.PathRegister, s.register)
	post(protocol.PathAcquire, s.acquire)
	post(protocol.PathHeartbeat, s.heartbeat)
	post(protocol.PathLogs, s.logs)
	post(protocol.PathStepStart, s.stepStart)
	post(protocol.PathStepEnd, s.stepEnd)
	post(protocol.PathComplete, s.complete)
	post(protocol.PathRelease, s.release)
	post(protocol.PathAnnotate, s.annotate)
	post(protocol.PathSetup, s.setup)
}

func (s *Server) authenticated(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtleCompare(got, s.opts.Token) != 1 {
			writeErr(w, http.StatusUnauthorized, "runner token is missing or wrong")
			return
		}
		h(w, r)
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	if req.APIVersion != protocol.APIVersion {
		// Refuse rather than half-understand a runner speaking another
		// version's payloads.
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"runner speaks protocol %q, this control plane speaks %q", req.APIVersion, protocol.APIVersion))
		return
	}
	if req.RunnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id is required")
		return
	}
	now := s.opts.Now()
	rn := &model.Runner{
		ID: req.RunnerID, Name: req.Name, Labels: req.Labels, Group: req.Group,
		State: model.RunnerIdle, Capacity: req.Capacity, Version: req.Version,
		OS: req.OS, Arch: req.Arch, FirstSeenAt: now, LastHeartbeat: now,
	}
	if err := s.opts.Store.RegisterRunner(r.Context(), rn); err != nil {
		writeErr(w, http.StatusInternalServerError, "register runner: "+err.Error())
		return
	}
	s.log.Info("runner registered", "runner", req.RunnerID, "labels", req.Labels)
	writeJSON(w, http.StatusOK, protocol.RegisterResponse{
		LeaseTTL:          protocol.Duration(s.opts.LeaseTTL),
		HeartbeatInterval: protocol.Duration(s.opts.HeartbeatInterval),
		LogFlushInterval:  protocol.Duration(s.opts.LogFlushInterval),
	})
}

// acquire long-polls for work. It returns an empty assignment rather than an
// error when nothing is available, so an idle agent loops quietly.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) {
	var req protocol.AcquireRequest
	if !decode(w, r, &req) {
		return
	}
	if req.RunnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id is required")
		return
	}

	wait := req.Wait.D()
	if wait <= 0 || wait > s.opts.AcquireWait {
		wait = s.opts.AcquireWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	if err := s.opts.Store.RunnerHeartbeat(ctx, req.RunnerID, s.opts.Now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("runner heartbeat during acquire failed", "runner", req.RunnerID, "err", err)
	}

	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		a, err := s.opts.Scheduler.Acquire(ctx, req.RunnerID, req.Labels, s.opts.Now())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, "acquire: "+err.Error())
			return
		}
		if a != nil {
			s.log.Info("job dispatched", "runner", req.RunnerID, "job", a.JobID, "attempt", a.Attempt)
			writeJSON(w, http.StatusOK, protocol.AcquireResponse{Assignment: a})
			return
		}
		select {
		case <-ctx.Done():
			// A poll that found nothing is a normal outcome, not an error.
			writeJSON(w, http.StatusOK, protocol.AcquireResponse{})
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req protocol.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	now := s.opts.Now()

	if err := s.opts.Store.RunnerHeartbeat(ctx, req.RunnerID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("runner heartbeat failed", "runner", req.RunnerID, "err", err)
	}

	var resp protocol.HeartbeatResponse
	if req.JobID != 0 {
		err := s.opts.Store.Heartbeat(ctx, req.RunnerID, req.JobID, s.opts.LeaseTTL)
		switch {
		case errors.Is(err, store.ErrLeaseLost), errors.Is(err, store.ErrNotFound):
			// The job was requeued elsewhere. Tell the agent to stop without
			// reporting a result, so two runners cannot both complete it.
			resp.LeaseLost = true
			s.log.Warn("runner holds a lost lease", "runner", req.RunnerID, "job", req.JobID)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "heartbeat: "+err.Error())
			return
		default:
			s.mu.Lock()
			if reason, ok := s.cancels[req.JobID]; ok {
				delete(s.cancels, req.JobID)
				resp.Cancel = &reason
			}
			s.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	var req protocol.LogBatch
	if !decode(w, r, &req) {
		return
	}
	if len(req.Lines) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"last_seq": 0})
		return
	}
	last, err := s.opts.Logs.Append(r.Context(), req.JobID, req.Attempt, req.Lines)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "append logs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last_seq": last})
}

func (s *Server) stepStart(w http.ResponseWriter, r *http.Request) {
	var req protocol.StepStartRequest
	if !decode(w, r, &req) {
		return
	}
	now := s.opts.Now()
	step := &model.Step{
		JobID: req.JobID, Number: req.Number, Name: req.Name, StepID: req.StepID,
		Status: model.StatusInProgress, Attempt: req.Attempt,
		StartedAt: &now, LogStart: req.LogStart,
	}
	if err := s.opts.Store.UpsertStep(r.Context(), step); err != nil {
		writeErr(w, http.StatusInternalServerError, "start step: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) stepEnd(w http.ResponseWriter, r *http.Request) {
	var req protocol.StepEndRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	now := s.opts.Now()

	steps, err := s.opts.Store.ListSteps(ctx, req.JobID, req.Attempt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list steps: "+err.Error())
		return
	}
	var step *model.Step
	for _, st := range steps {
		if st.Number == req.Number {
			step = st
			break
		}
	}
	if step == nil {
		step = &model.Step{JobID: req.JobID, Number: req.Number, Attempt: req.Attempt, StartedAt: &now}
	}
	step.Status = model.StatusCompleted
	step.Conclusion = req.Conclusion
	step.Class = req.Class
	step.ExitCode = req.ExitCode
	step.Outputs = req.Outputs
	step.CompletedAt = &now
	step.LogEnd = req.LogEnd
	if err := s.opts.Store.UpsertStep(ctx, step); err != nil {
		writeErr(w, http.StatusInternalServerError, "end step: "+err.Error())
		return
	}

	// The classification decision is recorded as its own event so an operator
	// can see why a step was called infrastructure rather than trusting it.
	if req.ClassReason != "" {
		_ = s.opts.Store.RecordEvent(ctx, store.Event{
			JobID: req.JobID, Kind: "classified", Message: req.ClassReason, At: now,
			Detail: map[string]any{"step": req.Number, "class": string(req.Class)},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetupRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	now := s.opts.Now()

	detail := map[string]any{"cache_warm": req.CacheWarm}
	for k, v := range req.Breakdown {
		detail[k] = v.D().String()
	}
	if err := s.opts.Store.RecordEvent(ctx, store.Event{
		JobID: req.JobID, Kind: "setup_" + req.Phase,
		Message: setupMessage(req), Detail: detail, At: now,
	}); err != nil {
		s.log.Warn("record setup event failed", "job", req.JobID, "err", err)
	}

	if req.Phase == "completed" {
		if err := s.opts.Scheduler.JobSetupCompleted(ctx, req.JobID, now); err != nil {
			writeErr(w, http.StatusInternalServerError, "setup completed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func setupMessage(req protocol.SetupRequest) string {
	if req.Phase != "completed" {
		return "sandbox setup started"
	}
	var total time.Duration
	for _, d := range req.Breakdown {
		total += d.D()
	}
	warm := "cold"
	if req.CacheWarm {
		warm = "warm"
	}
	return fmt.Sprintf("sandbox setup finished in %s with a %s image cache", total, warm)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	var req protocol.CompleteRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Conclusion == model.ConclusionCancelled && req.Cancel == nil {
		writeErr(w, http.StatusBadRequest,
			"a cancelled job must carry the reason it was cancelled")
		return
	}
	if req.Cancel != nil {
		if err := req.Cancel.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx := r.Context()
	if err := s.opts.Logs.Finalize(ctx, req.JobID, req.Attempt); err != nil {
		s.log.Warn("finalize logs failed", "job", req.JobID, "attempt", req.Attempt, "err", err)
	}

	err := s.opts.Scheduler.JobCompleted(ctx, req.JobID, SchedulerResult{
		Conclusion:        req.Conclusion,
		Class:             req.Class,
		ClassReason:       req.ClassReason,
		Explanation:       req.Explanation,
		Outputs:           req.Outputs,
		Cancel:            req.Cancel,
		ClassificationLog: req.ClassificationLog,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "complete job: "+err.Error())
		return
	}

	// Report back whether the attempt will be retried, purely so the agent can
	// log it; the control plane owns the decision.
	resp := protocol.CompleteResponse{}
	if job, jerr := s.opts.Store.GetJob(ctx, req.JobID); jerr == nil {
		if job.Status != model.StatusCompleted && job.Attempt > req.Attempt {
			resp.WillRetry = true
			resp.NextAttempt = job.Attempt
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	// A job reappearing in the queue must say why it did.
	if err := req.Reason.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.opts.Scheduler.ReleaseJob(r.Context(), req.RunnerID, req.JobID, req.Reason); err != nil {
		writeErr(w, http.StatusInternalServerError, "release: "+err.Error())
		return
	}
	s.log.Info("job released back to the queue", "job", req.JobID, "actor", req.Reason.Actor, "reason", req.Reason.Sentence)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) annotate(w http.ResponseWriter, r *http.Request) {
	var req protocol.AnnotateRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Annotations) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err := s.opts.Store.AddAnnotations(r.Context(), req.JobID, req.Annotations); err != nil {
		writeErr(w, http.StatusInternalServerError, "add annotations: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// subtleCompare is a constant-time string comparison returning 1 on equality.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	if diff == 0 {
		return 1
	}
	return 0
}
