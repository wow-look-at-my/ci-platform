// Package scheduler is the run loop and the reliability layer: it decides what
// is ready, hands work out under a lease, retries what was the platform's
// fault, cancels with a recorded reason every time, and reduces the result
// through model.Aggregate rather than a second set of rules.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Defaults taken from GitHub's documented limits, not invented here.
const (
	DefaultLeaseTTL     = 60 * time.Second
	DefaultSetupTimeout = 10 * time.Minute
	DefaultJobTimeout   = 360 * time.Minute
	DefaultRunTimeout   = 35 * 24 * time.Hour
)

// Options configures a Scheduler. NewEval and MintJobToken have no sensible
// default, so a nil one is a programming error and New says so immediately.
type Options struct {
	// NewEval builds an expression evaluator; the same factory the planner uses.
	NewEval plan.EvaluatorFactory
	// MintJobToken issues the per-job bearer token carried in an assignment.
	MintJobToken func(runID, jobID int64, attempt int) (string, error)
	// Notify fires when a run on the repo's default branch ends non-success.
	Notify func(ctx context.Context, n Notification)

	// LeaseTTL is how long a dispatched job's lease survives without a
	// heartbeat before Tick requeues it.
	LeaseTTL time.Duration
	// SetupTimeout bounds the setup phase separately from execution; exceeding
	// it is an infra failure, because no user command has run yet.
	SetupTimeout time.Duration
	// DefaultJobTimeout applies to a job that declares no timeout-minutes.
	DefaultJobTimeout time.Duration
	// RunTimeout bounds a whole run.
	RunTimeout time.Duration

	// ServerURL is the control plane URL handed to runners.
	ServerURL string
	// RequireForkApproval holds a fork PR's jobs until a maintainer approves.
	RequireForkApproval bool
}

func (o *Options) applyDefaults() {
	if o.LeaseTTL == 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	if o.SetupTimeout == 0 {
		o.SetupTimeout = DefaultSetupTimeout
	}
	if o.DefaultJobTimeout == 0 {
		o.DefaultJobTimeout = DefaultJobTimeout
	}
	if o.RunTimeout == 0 {
		o.RunTimeout = DefaultRunTimeout
	}
}

// Scheduler drives every run it has been given a plan for.
//
// Plans live in memory: a control plane restart loses them, and every entry
// point that needs one reports that loudly rather than guessing at a policy or
// a step list it no longer has.
type Scheduler struct {
	st   store.Store
	opts Options

	mu    sync.Mutex
	plans map[int64]*plan.Plan
	// stepTimeouts caches the evaluated per-step budgets of the assignment a
	// runner is currently executing, so Tick can enforce them as a backstop.
	stepTimeouts map[int64]map[int]time.Duration
}

// New builds a Scheduler. A missing store, evaluator factory, or token minter
// is a wiring bug that would otherwise surface as a silently broken run.
func New(st store.Store, opts Options) *Scheduler {
	if st == nil {
		panic("scheduler: nil store")
	}
	if opts.NewEval == nil {
		panic("scheduler: no evaluator factory; job if: conditions cannot be evaluated without one")
	}
	if opts.MintJobToken == nil {
		panic("scheduler: no job token minter; runners cannot authenticate without one")
	}
	opts.applyDefaults()
	return &Scheduler{
		st:           st,
		opts:         opts,
		plans:        map[int64]*plan.Plan{},
		stepTimeouts: map[int64]map[int]time.Duration{},
	}
}

// ErrNoPlan is returned when the scheduler is asked about a run whose plan it
// does not hold. Guessing at the retry policy or step list of a run planned by
// a previous process would produce a job that looks right and is not.
var ErrNoPlan = errors.New("scheduler: no plan registered for this run")

func (s *Scheduler) planFor(runID int64) (*plan.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[runID]
	if !ok {
		return nil, fmt.Errorf("%w (run %d); re-plan the run before scheduling it", ErrNoPlan, runID)
	}
	return p, nil
}

// RegisterPlan hands the scheduler a plan for a run it is not already driving.
// Re-running a finished run needs this: the plan is dropped when the run
// completes, so the caller re-plans the workflow and registers it again rather
// than the scheduler inventing a job list it no longer has.
func (s *Scheduler) RegisterPlan(runID int64, p *plan.Plan) error {
	if p == nil || len(p.Jobs) == 0 {
		return fmt.Errorf("scheduler: run %d was given an empty plan", runID)
	}
	s.registerPlan(runID, p)
	return nil
}

func (s *Scheduler) registerPlan(runID int64, p *plan.Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans[runID] = p
}

func (s *Scheduler) forgetPlan(runID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, runID)
}

func (s *Scheduler) trackedRuns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.plans))
	for id := range s.plans {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// StartRun records a run's jobs and makes it eligible for scheduling. It does
// not dispatch anything; Tick does, so that dispatch has exactly one code path.
func (s *Scheduler) StartRun(ctx context.Context, run *model.Run, p *plan.Plan) error {
	if run == nil {
		return errors.New("scheduler: nil run")
	}
	if p == nil {
		return errors.New("scheduler: nil plan")
	}
	if len(p.Jobs) == 0 {
		return fmt.Errorf("scheduler: run %d has no jobs; a run with nothing in it cannot satisfy anything", run.ID)
	}
	now := run.CreatedAt
	if now.IsZero() {
		return fmt.Errorf("scheduler: run %d has no creation time", run.ID)
	}

	if err := s.supersedeOlderRuns(ctx, run, p, now); err != nil {
		return err
	}

	for _, pj := range p.Jobs {
		j := &model.Job{
			RunID:            run.ID,
			Key:              pj.Key,
			Name:             pj.Name,
			MatrixKey:        pj.MatrixKey,
			Matrix:           pj.Matrix,
			Needs:            pj.Needs,
			Labels:           pj.Labels,
			Attempt:          1,
			MaxAttempts:      pj.Retry.Attempts,
			Status:           model.StatusQueued,
			ConcurrencyGroup: pj.ConcurrencyGroup,
			CancelInProgress: pj.CancelInProgress,
			ContinueOnError:  pj.ContinueOnError,
			TimeoutMinutes:   pj.TimeoutMinutes,
			Environment:      pj.Environment,
			CreatedAt:        now,
		}
		if run.IsForkPR && s.opts.RequireForkApproval && !run.Approved {
			j.Status = model.StatusWaiting
			j.AwaitingApproval = true
		}
		if err := s.st.CreateJob(ctx, j); err != nil {
			return fmt.Errorf("scheduler: create job %q: %w", pj.Name, err)
		}
	}

	s.registerPlan(run.ID, p)

	run.Status = model.StatusInProgress
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	if err := s.st.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("scheduler: update run %d: %w", run.ID, err)
	}
	msg := fmt.Sprintf("run %d started with %d job(s)", run.ID, len(p.Jobs))
	if j := s.heldJobCount(run, p); j > 0 {
		msg += fmt.Sprintf("; %d held for fork-PR approval", j)
	}
	return s.emit(ctx, run.ID, 0, EventRunStarted, msg, nil, now)
}

func (s *Scheduler) heldJobCount(run *model.Run, p *plan.Plan) int {
	if run.IsForkPR && s.opts.RequireForkApproval && !run.Approved {
		return len(p.Jobs)
	}
	return 0
}

// Approve lifts the fork-PR hold on a run's jobs.
func (s *Scheduler) Approve(ctx context.Context, runID int64, actor string, now time.Time) error {
	if actor == "" {
		return errors.New("scheduler: approving a fork PR requires the approver's login")
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	run.Approved = true
	run.ApprovedBy = actor
	if err := s.st.UpdateRun(ctx, run); err != nil {
		return err
	}
	jobs, err := s.st.ListJobsForRun(ctx, runID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.AwaitingApproval {
			continue
		}
		j.AwaitingApproval = false
		j.Status = model.StatusQueued
		if err := s.st.UpdateJob(ctx, j); err != nil {
			return err
		}
	}
	return s.emit(ctx, runID, 0, EventRunStarted,
		fmt.Sprintf("%s approved this fork pull request, releasing its jobs to run", actor), nil, now)
}

// Tick drives everything: expired leases, timeouts, fail-fast, readiness,
// concurrency admission, dispatch eligibility and run rollup. It is idempotent,
// so calling it twice for the same instant changes nothing the first call did
// not already change.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("scheduler: Tick needs the current time")
	}
	if err := s.reapExpiredLeases(ctx, now); err != nil {
		return err
	}
	for _, runID := range s.trackedRuns() {
		if err := s.tickRun(ctx, runID, now); err != nil {
			return fmt.Errorf("scheduler: run %d: %w", runID, err)
		}
	}
	return nil
}

func (s *Scheduler) tickRun(ctx context.Context, runID int64, now time.Time) error {
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == model.StatusCompleted {
		s.forgetPlan(runID)
		return nil
	}
	p, err := s.planFor(runID)
	if err != nil {
		return err
	}
	jobs, err := s.st.ListJobsForRun(ctx, runID)
	if err != nil {
		return err
	}

	if err := s.enforceRunTimeout(ctx, run, jobs, now); err != nil {
		return err
	}
	if run.Status == model.StatusCompleted {
		return nil
	}
	if err := s.enforceJobTimeouts(ctx, run, jobs, now); err != nil {
		return err
	}
	if err := s.applyFailFast(ctx, p, jobs, now); err != nil {
		return err
	}
	if err := s.admitReadyJobs(ctx, run, p, jobs, now); err != nil {
		return err
	}
	return s.finalizeIfComplete(ctx, run, jobs, now)
}

// finalizeIfComplete closes a run once every job is terminal.
func (s *Scheduler) finalizeIfComplete(ctx context.Context, run *model.Run, jobs []*model.Job, now time.Time) error {
	if run.Status == model.StatusCompleted && run.CompletedAt != nil {
		return nil
	}
	for _, j := range jobs {
		if j.Status != model.StatusCompleted {
			return nil
		}
	}
	roll := rollupOf(run.ID, jobs)
	run.Status = model.StatusCompleted
	run.Conclusion = roll.Conclusion
	run.CompletedAt = &now
	if err := s.st.UpdateRun(ctx, run); err != nil {
		return err
	}
	if err := s.emit(ctx, run.ID, 0, EventRunCompleted, roll.Summary,
		map[string]any{"conclusion": string(roll.Conclusion)}, now); err != nil {
		return err
	}
	s.forgetPlan(run.ID)
	return s.notifyDefaultBranch(ctx, run, roll)
}

// jobsByKey groups a run's jobs by workflow job key, so a matrixed job's legs
// are reduced together.
func jobsByKey(jobs []*model.Job) map[string][]*model.Job {
	out := map[string][]*model.Job{}
	for _, j := range jobs {
		out[j.Key] = append(out[j.Key], j)
	}
	return out
}
