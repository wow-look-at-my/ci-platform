package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// needsState is what a candidate job's dependencies add up to.
type needsState struct {
	// ready is false while any needed job still has work outstanding.
	ready bool
	// context is the needs context exactly as expressions see it:
	// {<job key>: {outputs: {...}, result: "success"|"failure"|"cancelled"|"skipped"}}.
	context map[string]any
	status  plan.Status
	// why explains a non-success state in one sentence, for the skip event.
	why string
}

// computeNeeds reduces every needed job to one result through model.Aggregate,
// which is the only reducer in the platform.
//
// A job marked continue-on-error that failed counts as success HERE and only
// here: that is what "does not fail its dependents" means. Its real conclusion
// stays on the job row and flows into the run rollup unchanged.
func computeNeeds(pj *plan.PlannedJob, byKey map[string][]*model.Job, runCancelled bool) (needsState, error) {
	st := needsState{ready: true, context: map[string]any{}, status: plan.Status{Success: true}}
	var reasons []string
	for _, key := range pj.Needs {
		legs := byKey[key]
		if len(legs) == 0 {
			return needsState{}, fmt.Errorf("job %q needs %q, which has no jobs in this run", pj.Key, key)
		}
		conclusions := make([]model.Conclusion, 0, len(legs))
		outputs := map[string]string{}
		for _, l := range legs {
			if l.Status != model.StatusCompleted {
				st.ready = false
				break
			}
			c := l.Conclusion
			if c.IsFailure() && l.ContinueOnError {
				c = model.ConclusionSuccess
			}
			conclusions = append(conclusions, c)
			for k, v := range l.Outputs {
				outputs[k] = v
			}
		}
		if !st.ready {
			return st, nil
		}
		agg := model.Aggregate(conclusions)
		result := needsResult(conclusions)
		st.context[key] = map[string]any{"outputs": outputs, "result": result}
		switch result {
		case "failure":
			st.status.Success = false
			st.status.Failure = true
			reasons = append(reasons, fmt.Sprintf("%s concluded %s", key, agg))
		case "cancelled":
			st.status.Success = false
			st.status.Cancelled = true
			reasons = append(reasons, fmt.Sprintf("%s was cancelled", key))
		case "skipped":
			st.status.Success = false
			reasons = append(reasons, fmt.Sprintf("%s was %s", key, agg))
		}
	}
	if runCancelled {
		st.status.Success = false
		st.status.Cancelled = true
		reasons = append(reasons, "the run was cancelled")
	}
	sort.Strings(reasons)
	st.why = strings.Join(reasons, "; ")
	return st, nil
}

// needsResult reduces a job's legs to the value the needs context reports.
//
// This deliberately does NOT go through model.Aggregate, and the distinction is
// the important part. Aggregate answers "what should we TELL somebody about
// this run", where a mix of skipped and successful work is not a success.
// needsResult answers "should dependents RUN", which is execution semantics and
// has to match GitHub Actions: there, a matrix job with one skipped leg and one
// successful leg is `success` and its dependents run.
//
// Reducing gating through Aggregate would skip those dependents instead, which
// is a deploy job silently not running because an unrelated matrix leg was
// filtered out. That is incident 5 in docs/incidents.md, reintroduced by the
// mechanism meant to prevent it. The job's real conclusion is untouched and
// still flows into the rollup and the check run.
func needsResult(cs []model.Conclusion) string {
	if len(cs) == 0 {
		return "skipped"
	}
	var sawSuccess, sawCancelled, sawSkipped bool
	for _, c := range cs {
		switch {
		case c.IsFailure():
			return "failure"
		case c == model.ConclusionCancelled:
			sawCancelled = true
		case c == model.ConclusionSkipped:
			sawSkipped = true
		default:
			// Success, neutral, and stale all let dependents proceed.
			sawSuccess = true
		}
	}
	switch {
	case sawCancelled:
		return "cancelled"
	case sawSuccess:
		return "success"
	case sawSkipped:
		return "skipped"
	}
	return "success"
}

// shouldRun evaluates a job's if: against its needs.
func (s *Scheduler) shouldRun(p *plan.Plan, pj *plan.PlannedJob, needs needsState) (bool, error) {
	if pj.IR.If.Empty() {
		return needs.status.Success, nil
	}
	contexts := map[string]any{}
	for k, v := range p.Contexts {
		contexts[k] = v
	}
	contexts["needs"] = needs.context
	contexts["matrix"] = matrixContext(pj)
	contexts["strategy"] = map[string]any{
		"fail-fast":    pj.FailFast,
		"job-index":    pj.LegIndex,
		"job-total":    pj.LegTotal,
		"max-parallel": pj.MaxParallel,
	}
	ev := s.opts.NewEval(contexts, needs.status)
	ok, err := ev.EvalBool(pj.IR.If.Raw)
	if err != nil {
		return false, fmt.Errorf("job %q if: %w", pj.Name, err)
	}
	// GitHub wraps a condition naming no status function as
	// "success() && (<condition>)", so a plain `if: github.ref == ...` still
	// does not run after a failed need. This asks the expression parser rather
	// than scanning the text, which a status-function name inside a string
	// literal would fool.
	named, err := expr.ReferencesStatusFunction(pj.IR.If.Raw)
	if err != nil {
		return false, fmt.Errorf("job %q if: %w", pj.Name, err)
	}
	if !named {
		ok = ok && needs.status.Success
	}
	return ok, nil
}

func matrixContext(pj *plan.PlannedJob) any {
	if pj.Matrix == nil {
		return map[string]any{}
	}
	return pj.Matrix
}

// admitReadyJobs is the dispatch decision: what may go on the queue now, what
// is skipped, and what waits for a concurrency group or a max-parallel slot.
func (s *Scheduler) admitReadyJobs(ctx context.Context, run *model.Run, p *plan.Plan, jobs []*model.Job, now time.Time) error {
	if blocked, err := s.runGroupBlocked(ctx, run, p); err != nil {
		return err
	} else if blocked {
		return nil
	}

	byKey := jobsByKey(jobs)
	inFlight := map[string]int{}
	for _, j := range jobs {
		if inFlightJob(j) {
			inFlight[j.Key]++
		}
	}
	groupTaken := map[string]bool{}

	for _, j := range jobs {
		if !pendingJob(j) {
			continue
		}
		pj := p.Find(j.Key, j.MatrixKey)
		if pj == nil {
			return fmt.Errorf("job %q (%s) is not in the plan", j.Key, j.MatrixKey)
		}
		needs, err := computeNeeds(pj, byKey, run.Cancel != nil)
		if err != nil {
			return err
		}
		if !needs.ready {
			continue
		}

		ok, err := s.shouldRun(p, pj, needs)
		if err != nil {
			return err
		}
		if !ok {
			why := needs.why
			if why == "" {
				why = fmt.Sprintf("its if: condition (%s) was false", strings.TrimSpace(pj.IR.If.Raw))
			}
			if err := s.finishAttempt(ctx, pj, j, Result{
				Conclusion:  model.ConclusionSkipped,
				Explanation: fmt.Sprintf("%s did not run: %s.", j.Name, why),
			}, now, false); err != nil {
				return err
			}
			continue
		}

		if pj.MaxParallel > 0 && inFlight[j.Key] >= pj.MaxParallel {
			continue
		}
		admitted, err := s.admitToConcurrencyGroup(ctx, j, pj, groupTaken, now)
		if err != nil {
			return err
		}
		if !admitted {
			continue
		}
		if err := s.enqueue(ctx, j, now); err != nil {
			return err
		}
		inFlight[j.Key]++
	}
	return nil
}

// pendingJob reports whether a job is still waiting for the scheduler to decide
// about it. QueuedAt is the marker for "already handed to the queue".
func pendingJob(j *model.Job) bool {
	return j.Status != model.StatusCompleted && j.QueuedAt == nil && !j.AwaitingApproval
}

// inFlightJob reports whether a job occupies a max-parallel slot.
func inFlightJob(j *model.Job) bool {
	return j.Status != model.StatusCompleted && j.QueuedAt != nil
}

func (s *Scheduler) enqueue(ctx context.Context, j *model.Job, now time.Time) error {
	j.Status = model.StatusQueued
	j.QueuedAt = &now
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return err
	}
	if err := s.st.Enqueue(ctx, store.QueuedJob{
		JobID:    j.ID,
		RunID:    j.RunID,
		Labels:   j.Labels,
		Group:    j.ConcurrencyGroup,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	return s.emit(ctx, j.RunID, j.ID, EventQueued,
		fmt.Sprintf("%s is queued for a runner labelled %s", j.Name, strings.Join(j.Labels, ", ")),
		map[string]any{"labels": j.Labels}, now)
}
