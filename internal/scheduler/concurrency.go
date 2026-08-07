package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
)

// admitToConcurrencyGroup enforces at most one live job per group.
//
// With cancel-in-progress the newcomer supersedes whatever holds the group, and
// the older job is cancelled with the concurrency_group actor and a sentence
// naming what displaced it. Without it, the newcomer waits, visibly, in the
// waiting status rather than silently sitting in the queue.
func (s *Scheduler) admitToConcurrencyGroup(ctx context.Context, j *model.Job, pj *plan.PlannedJob, groupTaken map[string]bool, now time.Time) (bool, error) {
	group := j.ConcurrencyGroup
	if group == "" {
		return true, nil
	}
	if groupTaken[group] {
		return false, s.holdForGroup(ctx, j, group, now)
	}
	live, err := s.st.ListJobsInConcurrencyGroup(ctx, group)
	if err != nil {
		return false, err
	}
	// Only a job that has actually been admitted holds the group. A sibling
	// still waiting for the scheduler's decision holds nothing, and treating
	// it as a holder would have two jobs cancel or block each other.
	var holders []*model.Job
	for _, other := range live {
		if other.ID == j.ID || other.Status == model.StatusCompleted {
			continue
		}
		if other.QueuedAt == nil && other.Status != model.StatusInProgress {
			continue
		}
		holders = append(holders, other)
	}
	if len(holders) > 0 {
		if !pj.CancelInProgress {
			return false, s.holdForGroup(ctx, j, group, now)
		}
		for _, other := range holders {
			reason := model.CancelReason{
				Actor: model.CancelActorConcurrencyGroup,
				Sentence: fmt.Sprintf("%s from run %d took the concurrency group %q, which sets cancel-in-progress, so this job was superseded.",
					j.Name, j.RunID, group),
				TriggeredBy: fmt.Sprintf("run/%d", j.RunID),
			}
			if err := s.stopJob(ctx, nil, other, reason, model.ConclusionCancelled, model.ClassNone, now, false); err != nil {
				return false, err
			}
		}
	}
	groupTaken[group] = true
	return true, nil
}

// holdForGroup parks a job in the waiting status. The event is written only on
// the transition, so a job waiting for an hour does not write an event a tick.
func (s *Scheduler) holdForGroup(ctx context.Context, j *model.Job, group string, now time.Time) error {
	if j.Status == model.StatusWaiting {
		return nil
	}
	j.Status = model.StatusWaiting
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return err
	}
	return s.emit(ctx, j.RunID, j.ID, EventWaiting,
		fmt.Sprintf("%s is waiting for the concurrency group %q, which another job holds; it will run when the group frees up.", j.Name, group),
		map[string]any{"group": group}, now)
}

// supersedeOlderRuns applies workflow-level concurrency when a run starts.
//
// The group is tracked across the runs this scheduler holds plans for, which is
// every run it is driving. A control plane restart forgets in-flight runs, so
// the group is only enforced against runs this process knows about.
func (s *Scheduler) supersedeOlderRuns(ctx context.Context, run *model.Run, p *plan.Plan, now time.Time) error {
	if p.RunConcurrencyGroup == "" || !p.RunCancelInProgress {
		return nil
	}
	for _, otherID := range s.trackedRuns() {
		if otherID == run.ID {
			continue
		}
		otherPlan, err := s.planFor(otherID)
		if err != nil {
			continue
		}
		if otherPlan.RunConcurrencyGroup != p.RunConcurrencyGroup {
			continue
		}
		other, err := s.st.GetRun(ctx, otherID)
		if err != nil {
			return err
		}
		if other.Status == model.StatusCompleted {
			continue
		}
		reason := model.CancelReason{
			Actor: model.CancelActorSupersededByRun,
			Sentence: fmt.Sprintf("run %d (%s) started on the same concurrency group %q, which sets cancel-in-progress, so this older run was superseded.",
				run.ID, run.HeadSHA, p.RunConcurrencyGroup),
			TriggeredBy: fmt.Sprintf("run/%d", run.ID),
		}
		if err := s.CancelAt(ctx, otherID, reason, now); err != nil {
			return err
		}
	}
	return nil
}

// runGroupBlocked reports whether an older run holds this run's workflow-level
// concurrency group without cancel-in-progress, in which case this run waits.
func (s *Scheduler) runGroupBlocked(ctx context.Context, run *model.Run, p *plan.Plan) (bool, error) {
	if p.RunConcurrencyGroup == "" || p.RunCancelInProgress {
		return false, nil
	}
	for _, otherID := range s.trackedRuns() {
		if otherID == run.ID {
			continue
		}
		otherPlan, err := s.planFor(otherID)
		if err != nil || otherPlan.RunConcurrencyGroup != p.RunConcurrencyGroup {
			continue
		}
		other, err := s.st.GetRun(ctx, otherID)
		if err != nil {
			return false, err
		}
		if other.Status == model.StatusCompleted {
			continue
		}
		if other.CreatedAt.Before(run.CreatedAt) || (other.CreatedAt.Equal(run.CreatedAt) && otherID < run.ID) {
			return true, nil
		}
	}
	return false, nil
}
