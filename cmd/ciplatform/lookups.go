package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/oidc"
	"github.com/wow-look-at-my/ci-platform/internal/scheduler"
)

// jobTokenLookup resolves the facts a per-job token carries. The token grants
// access to this job's artifacts, cache, logs, and ID tokens, and nothing else;
// it never carries repository access.
func (a *app) jobTokenLookup(runID, jobID int64, attempt int) (jobtoken.Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	run, job, repo, err := a.resolve(ctx, runID, jobID)
	if err != nil {
		return jobtoken.Job{}, err
	}
	if job.Attempt != attempt {
		return jobtoken.Job{}, fmt.Errorf("job %d is on attempt %d, not %d", jobID, job.Attempt, attempt)
	}

	scopes := []jobtoken.Scope{
		jobtoken.ScopeArtifactsWrite, jobtoken.ScopeArtifactsRead,
		jobtoken.ScopeCacheRW, jobtoken.ScopeLogsWrite,
	}
	// A fork PR's workflow is attacker-controlled, so it gets no ID token and
	// no cache write. Enforced here as well as in the OIDC endpoint, because a
	// scope the token never carries cannot be forgotten downstream.
	if scheduler.OIDCAllowed(run) {
		scopes = append(scopes, jobtoken.ScopeOIDCIssue)
	} else {
		scopes = []jobtoken.Scope{
			jobtoken.ScopeArtifactsWrite, jobtoken.ScopeArtifactsRead,
			jobtoken.ScopeCacheRead, jobtoken.ScopeLogsWrite,
		}
	}

	return jobtoken.Job{
		RunID: runID, JobID: jobID, Attempt: attempt,
		RepoID:    repo.ID,
		Repo:      repo.FullName(),
		Ref:       refOf(run),
		Scopes:    scopes,
		ExpiresAt: time.Now().Add(a.jobTokenTTL(job)),
	}, nil
}

// jobTokenTTL bounds a job token by the job's own timeout rather than by the
// run's. A five-minute job holding a six-hour credential is six hours of
// artifact and cache writes available to anything that captured the token,
// long after the container that held it is gone. The signer adds its own
// clock-skew grace on top of this.
func (a *app) jobTokenTTL(job *model.Job) time.Duration {
	ttl := time.Duration(job.TimeoutMinutes) * time.Minute
	if ttl <= 0 {
		ttl = a.cfg.RunTimeout
	}
	return min(ttl, a.cfg.RunTimeout)
}

// oidcLookup builds the claim set for an ID token. Nothing is defaulted: a
// claim this cannot determine is an error, because a wrong `sub` silently
// grants a workflow access meant for a different one.
func (a *app) oidcLookup(ctx context.Context, runID, jobID int64, attempt int) (*oidc.Subject, error) {
	run, job, repo, err := a.resolve(ctx, runID, jobID)
	if err != nil {
		return nil, err
	}
	if job.Attempt != attempt {
		return nil, fmt.Errorf("job %d is on attempt %d, not %d", jobID, job.Attempt, attempt)
	}

	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	ref := refOf(run)
	refType := "branch"
	if strings.HasPrefix(ref, "refs/tags/") {
		refType = "tag"
	}
	workflowRef := fmt.Sprintf("%s/%s@%s", repo.FullName(), run.WorkflowPath, ref)

	// RepositoryOwnerID and ActorID are deliberately absent rather than
	// guessed. A relying party keying on repository_owner_id would otherwise
	// match every repository whose repo id happened to equal some org id, and
	// an actor_id of 0 would match every job on the platform. An omitted claim
	// fails a policy; a wrong one satisfies the wrong policy.
	return &oidc.Subject{
		Repository:           repo.FullName(),
		RepositoryOwner:      repo.Owner,
		RepositoryID:         repo.ID,
		RepositoryVisibility: visibility,
		Ref:                  ref,
		RefType:              refType,
		SHA:                  run.HeadSHA,
		Actor:                run.Actor,
		Workflow:             run.WorkflowName,
		WorkflowRef:          workflowRef,
		// Reusable workflows are not executed yet, so a job is always defined
		// in the workflow that started the run.
		JobWorkflowRef: workflowRef,
		RunID:          run.ID,
		RunNumber:      run.RunNumber,
		RunAttempt:     run.Attempt,
		EventName:      run.Event,
		Environment:    job.Environment,
		HeadRef:        run.HeadBranch,
		BaseRef:        run.BaseBranch,
		IsForkPR:       !scheduler.OIDCAllowed(run),
	}, nil
}

func (a *app) resolve(ctx context.Context, runID, jobID int64) (*model.Run, *model.Job, *model.Repo, error) {
	job, err := a.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("job %d: %w", jobID, err)
	}
	if job.RunID != runID {
		return nil, nil, nil, fmt.Errorf("job %d belongs to run %d, not %d", jobID, job.RunID, runID)
	}
	run, err := a.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("run %d: %w", runID, err)
	}
	repo, err := a.store.GetRepo(ctx, run.RepoID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("repo %d: %w", run.RepoID, err)
	}
	return run, job, repo, nil
}

// refOf reconstructs the full ref a run was triggered on.
//
// A branch name is always prefixed, never passed through: "refs/heads/main" is
// a legal branch NAME, and honouring it as an already-qualified ref would let a
// branch called refs/heads/main claim the default branch's cache scope.
func refOf(run *model.Run) string {
	if run.HeadBranch == "" {
		return "refs/heads/"
	}
	if run.Event == "pull_request" {
		return fmt.Sprintf("refs/pull/%s/merge", run.HeadBranch)
	}
	return "refs/heads/" + run.HeadBranch
}
