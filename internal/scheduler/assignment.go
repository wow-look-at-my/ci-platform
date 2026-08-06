package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// maxSkippedDequeues bounds how many already-finished jobs Acquire will step
// over before giving up, so a queue full of cancelled rows cannot spin.
const maxSkippedDequeues = 16

// SecretsAllowed reports whether a run may receive repository secrets. A pull
// request from a fork never does: its workflow is attacker-controlled.
func SecretsAllowed(run *model.Run) bool { return !run.IsForkPR }

// OIDCAllowed reports whether a run's jobs may mint OIDC tokens. Same rule, and
// the OIDC endpoint must ask it: protocol.Assignment has no field to carry the
// answer to the runner.
func OIDCAllowed(run *model.Run) bool { return !run.IsForkPR }

// Acquire hands one queued job to a runner under a lease and returns the
// assignment to execute. store.ErrNotFound means there was nothing to do.
func (s *Scheduler) Acquire(ctx context.Context, runnerID string, labels []string, now time.Time) (*protocol.Assignment, error) {
	if runnerID == "" {
		return nil, errors.New("scheduler: acquire without a runner id")
	}
	for i := 0; i < maxSkippedDequeues; i++ {
		j, err := s.st.Dequeue(ctx, runnerID, labels, s.opts.LeaseTTL)
		if err != nil {
			return nil, err
		}
		// A job cancelled while it sat in the queue must not start now. The
		// reason it was already stopped for is the honest one to give back;
		// without one, the control plane itself is withdrawing the claim.
		if j.Status == model.StatusCompleted || j.Conclusion != "" {
			reason := model.CancelReason{
				Actor:       model.CancelActorShutdown,
				Sentence:    fmt.Sprintf("%s had already concluded %s when runner %s claimed it, so the control plane withdrew the claim.", j.Name, j.Conclusion, runnerID),
				TriggeredBy: runnerID,
			}
			if j.Cancel != nil {
				reason = *j.Cancel
			}
			if err := s.st.ReleaseLease(ctx, runnerID, j.ID, reason); err != nil {
				return nil, err
			}
			continue
		}
		return s.dispatch(ctx, runnerID, j, now)
	}
	return nil, fmt.Errorf("scheduler: runner %q dequeued %d already-finished jobs in a row; the queue and the job rows disagree",
		runnerID, maxSkippedDequeues)
}

func (s *Scheduler) dispatch(ctx context.Context, runnerID string, j *model.Job, now time.Time) (*protocol.Assignment, error) {
	run, err := s.st.GetRun(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	repo, err := s.st.GetRepo(ctx, run.RepoID)
	if err != nil {
		return nil, err
	}
	pj, err := s.plannedFor(j)
	if err != nil {
		return nil, err
	}
	p, err := s.planFor(j.RunID)
	if err != nil {
		return nil, err
	}
	jobs, err := s.st.ListJobsForRun(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	needs, err := computeNeeds(pj, jobsByKey(jobs), run.Cancel != nil)
	if err != nil {
		return nil, err
	}

	a, err := s.buildAssignment(ctx, run, repo, p, pj, j, needs)
	if err != nil {
		return nil, err
	}

	lease := now.Add(s.opts.LeaseTTL)
	j.Status = model.StatusInProgress
	j.RunnerID = runnerID
	j.LeaseExpiresAt = &lease
	if j.StartedAt == nil {
		j.StartedAt = &now
	}
	if err := s.st.UpdateJob(ctx, j); err != nil {
		return nil, err
	}
	if err := s.emit(ctx, j.RunID, j.ID, EventDispatched,
		fmt.Sprintf("%s was dispatched to runner %s as attempt %d", j.Name, runnerID, j.Attempt),
		map[string]any{"runner": runnerID, "attempt": j.Attempt, "idempotency_key": a.IdempotencyKey}, now); err != nil {
		return nil, err
	}
	return a, nil
}

// buildAssignment resolves everything a runner needs for one attempt.
//
// A fork pull request gets no secrets and no OIDC: the workflow is under the
// contributor's control, so handing it the repository's credentials would let
// any stranger read them. That is enforced here rather than trusted to callers.
func (s *Scheduler) buildAssignment(ctx context.Context, run *model.Run, repo *model.Repo, p *plan.Plan, pj *plan.PlannedJob, j *model.Job, needs needsState) (*protocol.Assignment, error) {
	if s.opts.ServerURL == "" {
		return nil, errors.New("scheduler: no server URL configured, so a runner would have nowhere to report to")
	}
	token, err := s.opts.MintJobToken(run.ID, j.ID, j.Attempt)
	if err != nil {
		return nil, fmt.Errorf("scheduler: mint job token for %s: %w", j.Name, err)
	}
	if token == "" {
		return nil, fmt.Errorf("scheduler: the job token minter returned an empty token for %s", j.Name)
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

	env := map[string]string{}
	if p.Workflow != nil {
		if err := mergeEnv(env, p.Workflow.Env, ev); err != nil {
			return nil, fmt.Errorf("scheduler: workflow env: %w", err)
		}
	}
	if err := mergeEnv(env, pj.IR.Env, ev); err != nil {
		return nil, fmt.Errorf("scheduler: job %q env: %w", j.Name, err)
	}
	contexts["env"] = env

	steps, budgets, err := buildSteps(pj, ev)
	if err != nil {
		return nil, fmt.Errorf("scheduler: job %q steps: %w", j.Name, err)
	}

	a := &protocol.Assignment{
		RunID:            run.ID,
		JobID:            j.ID,
		Attempt:          j.Attempt,
		IdempotencyKey:   fmt.Sprintf("%d/%d/%d", run.ID, j.ID, j.Attempt),
		JobName:          j.Name,
		JobKey:           j.Key,
		RepoOwner:        repo.Owner,
		RepoName:         repo.Name,
		HeadSHA:          run.HeadSHA,
		HeadRef:          run.HeadBranch,
		Labels:           j.Labels,
		Steps:            steps,
		Env:              env,
		Contexts:         contexts,
		TimeoutMinutes:   pj.TimeoutMinutes,
		SetupTimeout:     protocol.Duration(s.opts.SetupTimeout),
		JobToken:         token,
		ServerURL:        s.opts.ServerURL,
		Retry:            pj.Retry,
		DefaultShell:     defaultString(pj.IR.Defaults.Shell, workflowShell(p)),
		WorkingDirectory: defaultString(pj.IR.Defaults.WorkingDirectory, workflowWorkdir(p)),
	}
	if a.TimeoutMinutes == 0 {
		a.TimeoutMinutes = int(s.opts.DefaultJobTimeout / time.Minute)
	}
	if a.Container, err = resolveContainer(pj.IR.Container, ev); err != nil {
		return nil, fmt.Errorf("scheduler: job %q container: %w", j.Name, err)
	}
	if len(pj.IR.Services) > 0 {
		a.Services = map[string]*protocol.ContainerSpec{}
		for name, spec := range pj.IR.Services {
			c, err := resolveContainer(spec, ev)
			if err != nil {
				return nil, fmt.Errorf("scheduler: job %q service %q: %w", j.Name, name, err)
			}
			a.Services[name] = c
		}
	}

	if SecretsAllowed(run) {
		raw, err := s.st.ResolveSecrets(ctx, repo.Owner, repo.Name, j.Environment)
		if err != nil {
			return nil, fmt.Errorf("scheduler: resolve secrets for %s: %w", j.Name, err)
		}
		if len(raw) > 0 {
			a.Secrets = make(map[string]string, len(raw))
			for k, v := range raw {
				a.Secrets[k] = string(v)
			}
		}
	} else {
		// Say it out loud: a workflow that silently sees no secrets looks
		// broken, and the operator needs to know why it was.
		a.Secrets = nil
		delete(a.Contexts, "secrets")
		if err := s.emit(ctx, run.ID, j.ID, EventRestricted,
			fmt.Sprintf("%s is a pull request from a fork, so %s was given no secrets and cannot mint an OIDC token.", run.HeadBranch, j.Name),
			map[string]any{"fork_pr": true}, time.Now()); err != nil {
			return nil, err
		}
	}

	s.rememberStepTimeouts(j.ID, budgets)
	return a, nil
}

func workflowShell(p *plan.Plan) string {
	if p.Workflow == nil {
		return ""
	}
	return p.Workflow.Defaults.Shell
}

func workflowWorkdir(p *plan.Plan) string {
	if p.Workflow == nil {
		return ""
	}
	return p.Workflow.Defaults.WorkingDirectory
}

func defaultString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func mergeEnv(into map[string]string, src map[string]model.Expr, ev plan.Evaluator) error {
	resolved, err := plan.EvalStringMap(ev, src)
	if err != nil {
		return err
	}
	for k, v := range resolved {
		into[k] = v
	}
	return nil
}

// buildSteps resolves every step expression the runner cannot resolve itself.
// if: is deliberately left raw: it depends on the outcome of earlier steps,
// which only the runner knows.
func buildSteps(pj *plan.PlannedJob, ev plan.Evaluator) ([]protocol.StepSpec, map[int]time.Duration, error) {
	steps := make([]protocol.StepSpec, 0, len(pj.IR.Steps))
	budgets := map[int]time.Duration{}
	for _, st := range pj.IR.Steps {
		name, err := plan.EvalString(ev, st.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d name: %w", st.Number, err)
		}
		if name == "" {
			name = stepFallbackName(st)
		}
		run, err := plan.EvalString(ev, st.Run)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d run: %w", st.Number, err)
		}
		with, err := plan.EvalStringMap(ev, st.With)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d with: %w", st.Number, err)
		}
		env, err := plan.EvalStringMap(ev, st.Env)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d env: %w", st.Number, err)
		}
		wd, err := plan.EvalString(ev, st.WorkingDirectory)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d working-directory: %w", st.Number, err)
		}
		coe, err := plan.EvalBool(ev, st.ContinueOnError, false)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d continue-on-error: %w", st.Number, err)
		}
		timeout, err := plan.EvalInt(ev, st.TimeoutMinutes, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d timeout-minutes: %w", st.Number, err)
		}
		if timeout < 0 {
			return nil, nil, fmt.Errorf("step %d timeout-minutes is %d", st.Number, timeout)
		}
		if timeout > 0 {
			budgets[st.Number] = time.Duration(timeout) * time.Minute
		}
		steps = append(steps, protocol.StepSpec{
			Number:           st.Number,
			ID:               st.ID,
			Name:             name,
			IfExpr:           st.If.Raw,
			Uses:             st.Uses,
			Run:              run,
			With:             with,
			Env:              env,
			Shell:            st.Shell,
			WorkingDirectory: wd,
			ContinueOnError:  coe,
			TimeoutMinutes:   timeout,
			Retry:            st.Retry,
		})
	}
	return steps, budgets, nil
}

func stepFallbackName(st *model.StepIR) string {
	if st.Uses != "" {
		return st.Uses
	}
	return fmt.Sprintf("Run step %d", st.Number)
}

func resolveContainer(spec *model.ContainerSpec, ev plan.Evaluator) (*protocol.ContainerSpec, error) {
	if spec == nil {
		return nil, nil
	}
	image, err := plan.EvalString(ev, spec.Image)
	if err != nil {
		return nil, err
	}
	if image == "" {
		return nil, errors.New("image evaluated to an empty string")
	}
	creds, err := plan.EvalStringMap(ev, spec.Credentials)
	if err != nil {
		return nil, err
	}
	env, err := plan.EvalStringMap(ev, spec.Env)
	if err != nil {
		return nil, err
	}
	ports, err := evalList(ev, spec.Ports)
	if err != nil {
		return nil, err
	}
	volumes, err := evalList(ev, spec.Volumes)
	if err != nil {
		return nil, err
	}
	options, err := plan.EvalString(ev, spec.Options)
	if err != nil {
		return nil, err
	}
	return &protocol.ContainerSpec{
		Image:       image,
		Credentials: creds,
		Env:         env,
		Ports:       ports,
		Volumes:     volumes,
		Options:     options,
	}, nil
}

func evalList(ev plan.Evaluator, in []model.Expr) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		v, err := plan.EvalString(ev, e)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ReleaseJob returns a job a runner cannot finish, with the runner's reason.
func (s *Scheduler) ReleaseJob(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	if err := reason.Validate(); err != nil {
		return err
	}
	if err := s.st.ReleaseLease(ctx, runnerID, jobID, reason); err != nil {
		return err
	}
	return s.emit(ctx, 0, jobID, EventRequeued, reason.Sentence,
		map[string]any{"actor": string(reason.Actor), "runner": runnerID}, time.Now())
}
