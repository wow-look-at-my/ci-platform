package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Result is the outcome of one job attempt, as reported by a runner or as
// determined by the scheduler itself on a timeout or a cancellation.
type Result struct {
	Conclusion model.Conclusion
	Class      model.FailureClass
	// ClassReason is the classifier's sentence, recorded as its own event.
	ClassReason string
	// Explanation is the job's headline failure message.
	Explanation string
	Outputs     map[string]string
	// Cancel is required whenever Conclusion is cancelled, and carried on any
	// other conclusion the platform imposed (a timeout, a lost runner).
	Cancel            *model.CancelReason
	ClassificationLog []string
}

func (r Result) validate() error {
	if !r.Conclusion.Valid() {
		return fmt.Errorf("unknown conclusion %q", r.Conclusion)
	}
	if r.Conclusion == "" {
		return errors.New("a completed attempt needs a conclusion")
	}
	if !r.Class.Valid() {
		return fmt.Errorf("unknown failure class %q", r.Class)
	}
	if r.Conclusion == model.ConclusionCancelled && r.Cancel == nil {
		return errors.New("a cancellation without a recorded reason is exactly the incident this platform exists to prevent")
	}
	if r.Cancel != nil {
		if err := r.Cancel.Validate(); err != nil {
			return err
		}
	}
	if r.Conclusion.IsFailure() && r.Class == model.ClassNone {
		return fmt.Errorf("conclusion %q needs a failure class naming whose fault it was", r.Conclusion)
	}
	return nil
}

// JobCompleted records a finished attempt, retrying it when the failure was
// the platform's and the policy still permits.
func (s *Scheduler) JobCompleted(ctx context.Context, jobID int64, res Result) error {
	return s.JobCompletedAt(ctx, jobID, res, time.Now())
}

// JobCompletedAt is JobCompleted with an explicit clock, for tests and for
// replaying a completion whose timestamp the runner reported.
func (s *Scheduler) JobCompletedAt(ctx context.Context, jobID int64, res Result, now time.Time) error {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	// A redelivered completion for an attempt already recorded changes nothing.
	if j.Status == model.StatusCompleted {
		return nil
	}
	pj, err := s.plannedFor(j)
	if err != nil {
		return err
	}
	return s.finishAttempt(ctx, pj, j, res, now, true)
}

func (s *Scheduler) plannedFor(j *model.Job) (*plan.PlannedJob, error) {
	p, err := s.planFor(j.RunID)
	if err != nil {
		return nil, err
	}
	pj := p.Find(j.Key, j.MatrixKey)
	if pj == nil {
		return nil, fmt.Errorf("scheduler: job %q (%s) is not in run %d's plan", j.Key, j.MatrixKey, j.RunID)
	}
	return pj, nil
}

// finishAttempt is the only path that ends a job attempt, so every rule that
// must hold for every ending -- a validated cancel reason, a recorded event, a
// retry decision made once -- holds here or nowhere.
func (s *Scheduler) finishAttempt(ctx context.Context, pj *plan.PlannedJob, j *model.Job, res Result, now time.Time, allowRetry bool) error {
	if err := res.validate(); err != nil {
		return fmt.Errorf("scheduler: job %d (%s): %w", j.ID, j.Name, err)
	}
	if allowRetry && pj == nil {
		return fmt.Errorf("scheduler: job %d (%s): no planned job, so its retry policy is unknown", j.ID, j.Name)
	}
	if res.ClassReason != "" {
		if err := s.emit(ctx, j.RunID, j.ID, EventClassified, res.ClassReason,
			map[string]any{"class": string(res.Class)}, now); err != nil {
			return err
		}
	}
	if len(res.ClassificationLog) > 0 {
		j.ClassificationLog = append(j.ClassificationLog, res.ClassificationLog...)
	}
	if res.Outputs != nil {
		j.Outputs = res.Outputs
	}

	// Only infra failures are ever retried. A user failure retried is a flaky
	// green; a config error retried cannot succeed however often it runs.
	if allowRetry && res.Class == model.ClassInfra && pj.Retry.Retries(model.ClassInfra, j.InfraRetryCount+1) {
		return s.requeueForRetry(ctx, pj, j, res, now)
	}

	j.Status = model.StatusCompleted
	j.Conclusion = res.Conclusion
	j.Class = res.Class
	j.Cancel = res.Cancel
	j.CompletedAt = &now
	j.RunnerID = ""
	j.LeaseExpiresAt = nil
	if res.Explanation != "" {
		j.FailureExplained = res.Explanation
	}
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return err
	}
	s.forgetStepTimeouts(j.ID)

	kind, msg := EventCompleted, fmt.Sprintf("%s concluded %s", j.Name, res.Conclusion)
	switch {
	case res.Conclusion == model.ConclusionSkipped:
		kind = EventSkipped
	case res.Cancel != nil:
		kind = EventCancelled
	}
	if res.Cancel != nil {
		msg = res.Cancel.Sentence
	} else if res.Explanation != "" {
		msg = res.Explanation
	}
	detail := map[string]any{"conclusion": string(res.Conclusion), "attempt": j.Attempt}
	if res.Class != model.ClassNone {
		detail["class"] = string(res.Class)
	}
	if res.Cancel != nil {
		detail["actor"] = string(res.Cancel.Actor)
	}

	return s.emit(ctx, j.RunID, j.ID, kind, msg, detail, now)
}

// requeueForRetry puts a job back on the queue after an infra failure, keeping
// this attempt's logs and starting a new attempt for the next one.
func (s *Scheduler) requeueForRetry(ctx context.Context, pj *plan.PlannedJob, j *model.Job, res Result, now time.Time) error {
	j.InfraRetryCount++
	delay := pj.Retry.Delay(j.InfraRetryCount + 1)
	notBefore := now.Add(delay)

	j.Attempt++
	j.Status = model.StatusQueued
	j.Conclusion = ""
	j.Class = model.ClassNone
	j.Cancel = nil
	j.RunnerID = ""
	j.LeaseExpiresAt = nil
	j.StartedAt = nil
	j.SetupCompletedAt = nil
	j.CompletedAt = nil
	j.QueuedAt = &now
	if res.Explanation != "" {
		j.FailureExplained = res.Explanation
	}
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return err
	}
	s.forgetStepTimeouts(j.ID)
	// Void the old entry first. Enqueue will not disturb a live lease, so
	// without this the previous attempt's leased row survives: the backoff
	// below is discarded and the stale row is later reaped as a lost runner
	// that never existed.
	if err := s.st.DropFromQueue(ctx, j.ID); err != nil {
		return err
	}
	if err := s.st.Enqueue(ctx, store.QueuedJob{
		JobID:     j.ID,
		RunID:     j.RunID,
		Attempt:   j.Attempt,
		Labels:    j.Labels,
		Group:     j.ConcurrencyGroup,
		QueuedAt:  now,
		NotBefore: notBefore,
	}); err != nil {
		return err
	}
	msg := fmt.Sprintf("attempt %d failed on infrastructure, not on your code, so attempt %d runs in %s; the failed attempt's logs are kept",
		j.Attempt-1, j.Attempt, delay)
	return s.emit(ctx, j.RunID, j.ID, EventRetried, msg, map[string]any{
		"attempt":     j.Attempt,
		"retry_count": j.InfraRetryCount,
		"delay":       delay.String(),
		"class":       string(res.Class),
	}, now)
}

// reapExpiredLeases turns "the runner disappeared" into a requeue with a
// sentence, never into a failure and never into a job that quietly stops.
func (s *Scheduler) reapExpiredLeases(ctx context.Context, now time.Time) error {
	reaped, err := s.st.ReapExpiredLeases(ctx, now)
	if err != nil {
		return fmt.Errorf("scheduler: reap leases: %w", err)
	}
	for _, j := range reaped {
		lost := j.RunnerID
		if lost == "" {
			lost = "the runner"
		}
		reason := model.CancelReason{
			Actor: model.CancelActorRunnerLost,
			Sentence: fmt.Sprintf("%s stopped renewing its lease on %s, so the job went back on the queue as attempt %d; it was not failed and no work was lost.",
				lost, j.Name, j.Attempt+1),
			TriggeredBy: j.RunnerID,
		}
		if err := reason.Validate(); err != nil {
			return err
		}
		j.Attempt++
		j.Status = model.StatusQueued
		j.Conclusion = ""
		j.Class = model.ClassNone
		j.Cancel = nil
		j.RunnerID = ""
		j.LeaseExpiresAt = nil
		j.StartedAt = nil
		j.SetupCompletedAt = nil
		j.CompletedAt = nil
		j.QueuedAt = &now
		if err := s.st.UpdateJob(ctx, j); err != nil {
			return err
		}
		s.forgetStepTimeouts(j.ID)
		if err := s.st.Enqueue(ctx, store.QueuedJob{
			JobID:    j.ID,
			RunID:    j.RunID,
			Labels:   j.Labels,
			Group:    j.ConcurrencyGroup,
			QueuedAt: now,
		}); err != nil {
			return err
		}
		if err := s.emit(ctx, j.RunID, j.ID, EventRequeued, reason.Sentence, map[string]any{
			"actor":         string(reason.Actor),
			"requeue_count": j.RequeueCount,
			"attempt":       j.Attempt,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

// JobSetupCompleted records the setup/execute boundary, which is what makes
// "setup took 5m30s" a measurement rather than an inference.
func (s *Scheduler) JobSetupCompleted(ctx context.Context, jobID int64, at time.Time) error {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.SetupCompletedAt != nil {
		return nil
	}
	j.SetupCompletedAt = &at
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return err
	}
	var setup time.Duration
	if j.StartedAt != nil {
		setup = at.Sub(*j.StartedAt)
	}
	return s.emit(ctx, j.RunID, j.ID, EventStarted,
		fmt.Sprintf("setup finished in %s; the job's own steps start now", setup),
		map[string]any{"setup": setup.String()}, at)
}
