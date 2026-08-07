package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
)

// stopJob is the only way a job stops without finishing its own work. Every
// caller supplies a validated model.CancelReason, so there is no path that
// leaves a user asking why something stopped.
//
// conclusion is separate from the reason because the reason says who stopped it
// and the conclusion says what that means: a timeout is timed_out, a setup
// timeout is an infra failure, everything else is cancelled.
func (s *Scheduler) stopJob(ctx context.Context, pj *plan.PlannedJob, j *model.Job, reason model.CancelReason, conclusion model.Conclusion, class model.FailureClass, now time.Time, allowRetry bool) error {
	if err := reason.Validate(); err != nil {
		return fmt.Errorf("scheduler: job %d (%s): %w", j.ID, j.Name, err)
	}
	return s.finishAttempt(ctx, pj, j, Result{
		Conclusion:  conclusion,
		Class:       class,
		Cancel:      &reason,
		Explanation: reason.Sentence,
	}, now, allowRetry)
}

// Cancel stops a whole run.
func (s *Scheduler) Cancel(ctx context.Context, runID int64, reason model.CancelReason) error {
	return s.CancelAt(ctx, runID, reason, time.Now())
}

// CancelAt is Cancel with an explicit clock.
func (s *Scheduler) CancelAt(ctx context.Context, runID int64, reason model.CancelReason, now time.Time) error {
	if err := reason.Validate(); err != nil {
		return err
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == model.StatusCompleted {
		return nil
	}
	jobs, err := s.st.ListJobsForRun(ctx, runID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Status == model.StatusCompleted {
			continue
		}
		jobReason := reason
		jobReason.Sentence = fmt.Sprintf("%s (%s was cancelled with it)", reason.Sentence, j.Name)
		if err := s.stopJob(ctx, nil, j, jobReason, model.ConclusionCancelled, model.ClassNone, now, false); err != nil {
			return err
		}
	}
	run.Cancel = &reason
	return s.finalizeIfComplete(ctx, run, jobs, now)
}

// CancelJob stops one job. Its dependents are not cancelled with it: they are
// skipped by the readiness rules, which is what GitHub does and what keeps a
// dependent's own if: in charge of the decision.
func (s *Scheduler) CancelJob(ctx context.Context, jobID int64, reason model.CancelReason) error {
	return s.CancelJobAt(ctx, jobID, reason, time.Now())
}

// CancelJobAt is CancelJob with an explicit clock.
func (s *Scheduler) CancelJobAt(ctx context.Context, jobID int64, reason model.CancelReason, now time.Time) error {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.Status == model.StatusCompleted {
		return nil
	}
	return s.stopJob(ctx, nil, j, reason, model.ConclusionCancelled, model.ClassNone, now, false)
}

// applyFailFast cancels the remaining legs of a matrixed job once one leg has
// failed, when the strategy asks for it.
func (s *Scheduler) applyFailFast(ctx context.Context, p *plan.Plan, jobs []*model.Job, now time.Time) error {
	byKey := jobsByKey(jobs)
	for key, legs := range byKey {
		if len(legs) < 2 {
			continue
		}
		pj := p.Find(key, legs[0].MatrixKey)
		if pj == nil || !pj.FailFast {
			continue
		}
		var failed *model.Job
		for _, l := range legs {
			if l.Status == model.StatusCompleted && l.Conclusion.IsFailure() && !l.ContinueOnError {
				failed = l
				break
			}
		}
		if failed == nil {
			continue
		}
		for _, l := range legs {
			if l.Status == model.StatusCompleted {
				continue
			}
			reason := model.CancelReason{
				Actor: model.CancelActorDependencyFailed,
				Sentence: fmt.Sprintf("%s concluded %s and this job's strategy sets fail-fast, so its remaining matrix legs were cancelled.",
					failed.Name, failed.Conclusion),
				TriggeredBy: fmt.Sprintf("job/%d", failed.ID),
			}
			if err := s.stopJob(ctx, nil, l, reason, model.ConclusionCancelled, model.ClassNone, now, false); err != nil {
				return err
			}
		}
	}
	return nil
}
