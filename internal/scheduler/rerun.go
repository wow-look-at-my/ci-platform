package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
)

// RerunFailed re-runs everything in a run that did not succeed, plus everything
// downstream of it. A job that succeeded keeps its result and its logs.
func (s *Scheduler) RerunFailed(ctx context.Context, runID int64, actor string) error {
	return s.rerun(ctx, runID, actor, time.Now(), func(j *model.Job) bool {
		return j.Conclusion != model.ConclusionSuccess
	})
}

// RerunJob re-runs one job and everything downstream of it.
func (s *Scheduler) RerunJob(ctx context.Context, jobID int64, actor string) error {
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	return s.rerun(ctx, j.RunID, actor, time.Now(), func(c *model.Job) bool {
		return c.ID == jobID
	})
}

// rerun resets the selected jobs and their transitive dependents. Dependents
// are included because a downstream job's result was computed from a result
// that is about to change.
func (s *Scheduler) rerun(ctx context.Context, runID int64, actor string, now time.Time, selected func(*model.Job) bool) error {
	if actor == "" {
		return errors.New("scheduler: a re-run needs the login of whoever asked for it")
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	p, err := s.planFor(runID)
	if err != nil {
		return err
	}
	jobs, err := s.st.ListJobsForRun(ctx, runID)
	if err != nil {
		return err
	}

	keys := map[string]bool{}
	for _, j := range jobs {
		if selected(j) {
			keys[j.Key] = true
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("scheduler: run %d has nothing matching to re-run", runID)
	}
	expandDependents(p, keys)

	var reset int
	for _, j := range jobs {
		if !keys[j.Key] {
			continue
		}
		pj := p.Find(j.Key, j.MatrixKey)
		if pj == nil {
			return fmt.Errorf("scheduler: job %q (%s) is not in run %d's plan", j.Key, j.MatrixKey, runID)
		}
		j.Attempt++
		j.Status = model.StatusQueued
		j.Conclusion = ""
		j.Class = model.ClassNone
		j.Cancel = nil
		j.FailureExplained = ""
		j.Outputs = nil
		j.RunnerID = ""
		j.LeaseExpiresAt = nil
		j.QueuedAt = nil
		j.StartedAt = nil
		j.SetupCompletedAt = nil
		j.CompletedAt = nil
		j.InfraRetryCount = 0
		j.MaxAttempts = pj.Retry.Attempts
		if err := s.st.UpdateJob(ctx, j); err != nil {
			return err
		}
		s.forgetStepTimeouts(j.ID)
		if err := s.emit(ctx, runID, j.ID, EventRerun,
			fmt.Sprintf("%s re-ran %s as attempt %d; the previous attempt's logs are kept", actor, j.Name, j.Attempt),
			map[string]any{"actor": actor, "attempt": j.Attempt}, now); err != nil {
			return err
		}
		reset++
	}

	run.Attempt++
	run.Status = model.StatusInProgress
	run.Conclusion = ""
	run.Cancel = nil
	run.CompletedAt = nil
	if err := s.st.UpdateRun(ctx, run); err != nil {
		return err
	}
	s.registerPlan(runID, p)
	return s.emit(ctx, runID, 0, EventRerun,
		fmt.Sprintf("%s started run attempt %d, re-running %d job(s)", actor, run.Attempt, reset),
		map[string]any{"actor": actor, "run_attempt": run.Attempt}, now)
}

// expandDependents grows a set of job keys to include everything that needs
// one of them, transitively.
func expandDependents(p *plan.Plan, keys map[string]bool) {
	for changed := true; changed; {
		changed = false
		for _, pj := range p.Jobs {
			if keys[pj.Key] {
				continue
			}
			for _, n := range pj.Needs {
				if keys[n] {
					keys[pj.Key] = true
					changed = true
					break
				}
			}
		}
	}
}
