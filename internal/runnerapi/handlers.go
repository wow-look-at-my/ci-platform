// The job-scoped handlers: logs, steps, setup, completion, release, and
// annotations. Every one of them requires holding the job's lease.
package runnerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// holdsJob reports whether this runner still holds the lease on this attempt.
//
// Every job-scoped write goes through it. Without it any host with the runner
// token can complete, annotate, or write logs for a job another runner is
// executing: the agent honouring LeaseLost is cooperation, not enforcement, and
// a partitioned runner that comes back is not cooperating.
func (s *Server) holdsJob(w http.ResponseWriter, r *http.Request, runnerID string, jobID int64, attempt int) bool {
	if runnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id is required")
		return false
	}
	job, err := s.opts.Store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("no job %d", jobID))
			return false
		}
		writeErr(w, http.StatusInternalServerError, "load job: "+err.Error())
		return false
	}
	if job.RunnerID != runnerID {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"job %d is held by another runner; this runner lost its lease and must stop", jobID))
		return false
	}
	if attempt != 0 && job.Attempt != attempt {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"job %d is on attempt %d, not %d; this attempt is over", jobID, job.Attempt, attempt))
		return false
	}
	return true
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	var req protocol.LogBatch
	if !decode(w, r, &req) {
		return
	}
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
	if !s.holdsJob(w, r, req.RunnerID, req.JobID, req.Attempt) {
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
