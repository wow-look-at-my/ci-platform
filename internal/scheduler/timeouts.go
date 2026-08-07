package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// enforceRunTimeout stops a whole run that has outlived its budget.
func (s *Scheduler) enforceRunTimeout(ctx context.Context, run *model.Run, jobs []*model.Job, now time.Time) error {
	start := run.CreatedAt
	if run.StartedAt != nil {
		start = *run.StartedAt
	}
	if start.IsZero() || now.Sub(start) <= s.opts.RunTimeout {
		return nil
	}
	reason := model.CancelReason{
		Actor: model.CancelActorTimeout,
		Sentence: fmt.Sprintf("the run exceeded the %s limit for a single workflow run and was stopped after %s.",
			s.opts.RunTimeout, now.Sub(start).Truncate(time.Second)),
	}
	for _, j := range jobs {
		if j.Status == model.StatusCompleted {
			continue
		}
		jobReason := reason
		jobReason.Sentence = fmt.Sprintf("%s (%s was still %s)", reason.Sentence, j.Name, j.Status)
		if err := s.stopJob(ctx, nil, j, jobReason, model.ConclusionTimedOut, model.ClassUser, now, false); err != nil {
			return err
		}
	}
	run.Cancel = &reason
	return s.finalizeIfComplete(ctx, run, jobs, now)
}

// enforceJobTimeouts applies the three job-scoped budgets, most specific first:
// a setup phase that never finished, a step that overran its own budget, and
// the job as a whole.
func (s *Scheduler) enforceJobTimeouts(ctx context.Context, run *model.Run, jobs []*model.Job, now time.Time) error {
	for _, j := range jobs {
		if j.Status != model.StatusInProgress || j.StartedAt == nil {
			continue
		}
		elapsed := now.Sub(*j.StartedAt)

		// Setup is measured separately because a job stuck preparing has run
		// no user command, so the failure is the platform's and it retries.
		if j.SetupCompletedAt == nil && elapsed > s.opts.SetupTimeout {
			pj, err := s.plannedFor(j)
			if err != nil {
				return err
			}
			reason := model.CancelReason{
				Actor: model.CancelActorTimeout,
				Sentence: fmt.Sprintf("setup for %s did not finish within %s, so the platform failed to prepare the job; no step of yours had started.",
					j.Name, s.opts.SetupTimeout),
			}
			if err := s.stopJob(ctx, pj, j, reason, model.ConclusionInfraFailure, model.ClassInfra, now, true); err != nil {
				return err
			}
			continue
		}

		if done, err := s.enforceStepTimeout(ctx, run, j, now); err != nil {
			return err
		} else if done {
			continue
		}

		budget := time.Duration(j.TimeoutMinutes) * time.Minute
		if budget == 0 {
			budget = s.opts.DefaultJobTimeout
		}
		if elapsed > budget {
			reason := model.CancelReason{
				Actor: model.CancelActorTimeout,
				Sentence: fmt.Sprintf("%s ran for %s, past the %s this job allows, and was stopped.",
					j.Name, elapsed.Truncate(time.Second), budget),
			}
			if err := s.stopJob(ctx, nil, j, reason, model.ConclusionTimedOut, model.ClassUser, now, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// enforceStepTimeout is the control plane's backstop for a step budget the
// runner also enforces. It uses the budgets from the assignment this process
// issued; a job dispatched by a previous process is left to its runner, which
// carries the same numbers in its own assignment.
func (s *Scheduler) enforceStepTimeout(ctx context.Context, run *model.Run, j *model.Job, now time.Time) (bool, error) {
	budgets := s.stepTimeoutsFor(j.ID)
	if len(budgets) == 0 {
		return false, nil
	}
	steps, err := s.st.ListSteps(ctx, j.ID, j.Attempt)
	if err != nil {
		return false, err
	}
	for _, step := range steps {
		if step.Status != model.StatusInProgress || step.StartedAt == nil {
			continue
		}
		budget, ok := budgets[step.Number]
		if !ok || budget <= 0 {
			continue
		}
		ran := now.Sub(*step.StartedAt)
		if ran <= budget {
			continue
		}
		reason := model.CancelReason{
			Actor: model.CancelActorTimeout,
			Sentence: fmt.Sprintf("step %d (%s) ran for %s, past the %s it allows, so %s was stopped.",
				step.Number, step.Name, ran.Truncate(time.Second), budget, j.Name),
		}
		if err := s.stopJob(ctx, nil, j, reason, model.ConclusionTimedOut, model.ClassUser, now, false); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (s *Scheduler) rememberStepTimeouts(jobID int64, budgets map[int]time.Duration) {
	if len(budgets) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepTimeouts[jobID] = budgets
}

func (s *Scheduler) stepTimeoutsFor(jobID int64) map[int]time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepTimeouts[jobID]
}

func (s *Scheduler) forgetStepTimeouts(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stepTimeouts, jobID)
}
