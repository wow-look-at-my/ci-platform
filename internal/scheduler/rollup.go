package scheduler

import (
	"context"
	"fmt"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Rollup is a run's outcome and the counts behind it.
type Rollup struct {
	RunID        int64
	Conclusion   model.Conclusion
	Total        int
	Completed    int
	ByConclusion map[model.Conclusion]int
	ByClass      map[model.FailureClass]int
	// Summary is one human sentence, shown on the run page and in the alarm.
	Summary string
}

// Notification kinds.
const (
	// NotifyDefaultBranchNotSuccess fires when a run on the repository's
	// default branch ends as anything other than success. A merged, green PR
	// whose publish never ran is the incident this exists for.
	NotifyDefaultBranchNotSuccess = "default_branch_run_not_success"
)

// Notification is what Options.Notify receives.
type Notification struct {
	Kind       string
	RunID      int64
	Repo       string
	Branch     string
	Workflow   string
	HeadSHA    string
	Conclusion model.Conclusion
	Summary    string
	Rollup     Rollup
}

// RunRollup reduces a run's jobs to one conclusion through model.Aggregate and
// reports the counts behind it.
func (s *Scheduler) RunRollup(ctx context.Context, runID int64) (Rollup, error) {
	jobs, err := s.st.ListJobsForRun(ctx, runID)
	if err != nil {
		return Rollup{}, err
	}
	return rollupOf(runID, jobs), nil
}

// conclusionOrder fixes the order counts are reported in, worst first, so the
// summary line reads the same way every time.
var conclusionOrder = []model.Conclusion{
	model.ConclusionConfigError,
	model.ConclusionInfraFailure,
	model.ConclusionTimedOut,
	model.ConclusionFailure,
	model.ConclusionActionRequired,
	model.ConclusionCancelled,
	model.ConclusionStale,
	model.ConclusionNeutral,
	model.ConclusionSkipped,
	model.ConclusionSuccess,
}

func rollupOf(runID int64, jobs []*model.Job) Rollup {
	r := Rollup{
		RunID:        runID,
		Total:        len(jobs),
		ByConclusion: map[model.Conclusion]int{},
		ByClass:      map[model.FailureClass]int{},
	}
	conclusions := make([]model.Conclusion, 0, len(jobs))
	for _, j := range jobs {
		if j.Status != model.StatusCompleted {
			continue
		}
		r.Completed++
		conclusions = append(conclusions, j.Conclusion)
		r.ByConclusion[j.Conclusion]++
		if j.Class != model.ClassNone {
			r.ByClass[j.Class]++
		}
	}
	// Aggregate is the only reducer. An empty set is neutral, never success:
	// zero jobs cannot have passed anything.
	r.Conclusion = model.Aggregate(conclusions)
	r.Summary = summarize(r)
	return r
}

func summarize(r Rollup) string {
	if r.Total == 0 {
		return "no jobs ran, so this run concluded neutral: there is nothing here that passed"
	}
	parts := make([]string, 0, len(r.ByConclusion))
	for _, c := range conclusionOrder {
		if n := r.ByConclusion[c]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c))
		}
	}
	if r.Completed < r.Total {
		parts = append(parts, fmt.Sprintf("%d still running", r.Total-r.Completed))
	}
	out := fmt.Sprintf("%d job(s): %s -- run concluded %s", r.Total, strings.Join(parts, ", "), r.Conclusion)
	if n := r.ByClass[model.ClassInfra]; n > 0 {
		out += fmt.Sprintf("; %d failure(s) were the platform's, not your code's", n)
	}
	if n := r.ByClass[model.ClassConfig]; n > 0 {
		out += fmt.Sprintf("; %d were workflow configuration errors", n)
	}
	return out
}

// notifyDefaultBranch raises the alarm for a default-branch run that did not
// succeed. A merged, green pull request whose publish never happened is the
// incident this platform was started over, so it is never silent.
func (s *Scheduler) notifyDefaultBranch(ctx context.Context, run *model.Run, roll Rollup) error {
	if s.opts.Notify == nil || roll.Conclusion == model.ConclusionSuccess {
		return nil
	}
	repo, err := s.st.GetRepo(ctx, run.RepoID)
	if err != nil {
		return fmt.Errorf("scheduler: run %d finished %s but its repository could not be read, so the default-branch alarm could not be evaluated: %w",
			run.ID, roll.Conclusion, err)
	}
	if repo.DefaultBranch == "" {
		return fmt.Errorf("scheduler: repository %s has no default branch recorded, so a default-branch failure cannot be detected", repo.FullName())
	}
	if run.HeadBranch != repo.DefaultBranch {
		return nil
	}
	s.opts.Notify(ctx, Notification{
		Kind:       NotifyDefaultBranchNotSuccess,
		RunID:      run.ID,
		Repo:       repo.FullName(),
		Branch:     run.HeadBranch,
		Workflow:   run.WorkflowName,
		HeadSHA:    run.HeadSHA,
		Conclusion: roll.Conclusion,
		Summary: fmt.Sprintf("%s on %s (%s) concluded %s: %s",
			run.WorkflowName, repo.DefaultBranch, run.HeadSHA, roll.Conclusion, roll.Summary),
		Rollup: roll,
	})
	return nil
}
