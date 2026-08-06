// Entity round-trips: the CRUD every implementation has to agree on.
package storetest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func testMigrateIdempotent(t *testing.T, f *fixture) {
	// The store handed to the suite is already migrated; migrating again must
	// be a no-op rather than an error or a duplicate-object failure.
	require.NoError(t, f.s.Migrate(f.ctx))
	require.NoError(t, f.s.Migrate(f.ctx))
}

func testRepos(t *testing.T, f *fixture) {
	r := f.repo(10, "acme", "widget")

	got, err := f.s.GetRepo(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, got)

	got, err = f.s.GetRepoByName(f.ctx, "acme", "widget")
	require.NoError(t, err)
	require.Equal(t, r, got)

	r.DefaultBranch = "trunk"
	r.Private = true
	require.NoError(t, f.s.UpsertRepo(f.ctx, r))
	got, err = f.s.GetRepo(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, "trunk", got.DefaultBranch)
	require.True(t, got.Private)

	f.repo(11, "acme", "gadget")
	all, err := f.s.ListRepos(f.ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "gadget", all[0].Name, "ListRepos sorts by owner then name")

	require.ErrorContains(t, f.s.UpsertRepo(f.ctx, &model.Repo{Owner: "a", Name: "b"}), "no id")
}

func testRuns(t *testing.T, f *fixture) {
	repo := f.repo(20, "acme", "widget")
	created := nowUTC()
	r := &model.Run{
		RepoID: repo.ID, RepoFull: repo.FullName(), WorkflowName: "CI",
		WorkflowPath: ".github/workflows/ci.yml", RunNumber: 4, Attempt: 2,
		Event: "pull_request", HeadSHA: "cafe1234", HeadBranch: "feature", BaseBranch: "main",
		Actor: "octocat", IsForkPR: true, CheckSuiteID: 99,
		Status:       model.StatusInProgress,
		EventPayload: []byte(`{"action":"opened"}`),
		Inputs:       map[string]any{"level": "debug", "count": float64(3)},
		CreatedAt:    created,
	}
	require.NoError(t, f.s.CreateRun(f.ctx, r))
	require.NotZero(t, r.ID, "CreateRun fills in the allocated id")

	got, err := f.s.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r.HeadSHA, got.HeadSHA)
	require.Equal(t, r.Inputs, got.Inputs)
	require.True(t, got.IsForkPR)
	require.JSONEq(t, string(r.EventPayload), string(got.EventPayload))
	require.True(t, created.Equal(got.CreatedAt), "created_at round-trips: %s vs %s", created, got.CreatedAt)

	done := nowUTC()
	got.Status = model.StatusCompleted
	got.Conclusion = model.ConclusionCancelled
	got.Cancel = &model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "octocat cancelled this run from the run page.",
		TriggeredBy: "octocat",
	}
	got.CompletedAt = &done
	require.NoError(t, f.s.UpdateRun(f.ctx, got))

	after, err := f.s.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, model.ConclusionCancelled, after.Conclusion)
	require.NotNil(t, after.Cancel)
	require.Equal(t, "octocat cancelled this run from the run page.", after.Cancel.Sentence)
	require.NotNil(t, after.CompletedAt)
	require.True(t, done.Equal(*after.CompletedAt))

	forSHA, err := f.s.ListRunsForSHA(f.ctx, repo.ID, "cafe1234")
	require.NoError(t, err)
	require.Len(t, forSHA, 1)
	require.Equal(t, r.ID, forSHA[0].ID)

	none, err := f.s.ListRunsForSHA(f.ctx, repo.ID, "nosuchsha")
	require.NoError(t, err)
	require.Empty(t, none)
}

func testRunFilters(t *testing.T, f *fixture) {
	repo := f.repo(30, "acme", "widget")
	base := nowUTC()
	mk := func(branch, actor, event string, status model.Status, offset time.Duration) *model.Run {
		r := &model.Run{
			RepoID: repo.ID, WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			Event: event, HeadSHA: "sha-" + branch, HeadBranch: branch, Actor: actor,
			Status: status, CreatedAt: base.Add(offset),
		}
		require.NoError(t, f.s.CreateRun(f.ctx, r))
		return r
	}
	newest := mk("main", "octocat", "push", model.StatusCompleted, 3*time.Second)
	mk("feature", "hubot", "pull_request", model.StatusQueued, 2*time.Second)
	mk("main", "hubot", "push", model.StatusQueued, time.Second)

	all, err := f.s.ListRuns(f.ctx, store.RunFilter{RepoID: repo.ID})
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, newest.ID, all[0].ID, "newest first")

	n, err := f.s.CountRuns(f.ctx, store.RunFilter{RepoID: repo.ID})
	require.NoError(t, err)
	require.Equal(t, 3, n)

	onMain, err := f.s.ListRuns(f.ctx, store.RunFilter{RepoID: repo.ID, Branch: "main"})
	require.NoError(t, err)
	require.Len(t, onMain, 2)

	byActor, err := f.s.ListRuns(f.ctx, store.RunFilter{Actor: "hubot"})
	require.NoError(t, err)
	require.Len(t, byActor, 2)

	byEvent, err := f.s.ListRuns(f.ctx, store.RunFilter{Event: "pull_request"})
	require.NoError(t, err)
	require.Len(t, byEvent, 1)

	byStatus, err := f.s.ListRuns(f.ctx, store.RunFilter{Status: model.StatusQueued})
	require.NoError(t, err)
	require.Len(t, byStatus, 2)

	byWorkflow, err := f.s.ListRuns(f.ctx, store.RunFilter{Workflow: ".github/workflows/ci.yml"})
	require.NoError(t, err)
	require.Len(t, byWorkflow, 3)

	bySearch, err := f.s.ListRuns(f.ctx, store.RunFilter{Search: "featur"})
	require.NoError(t, err)
	require.Len(t, bySearch, 1)

	page, err := f.s.ListRuns(f.ctx, store.RunFilter{RepoID: repo.ID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)

	page2, err := f.s.ListRuns(f.ctx, store.RunFilter{RepoID: repo.ID, Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, page2, 1)

	nCompleted, err := f.s.CountRuns(f.ctx, store.RunFilter{Conclusion: model.ConclusionSuccess})
	require.NoError(t, err)
	require.Equal(t, 0, nCompleted)
}

func testRunNumbers(t *testing.T, f *fixture) {
	repo := f.repo(40, "acme", "widget")
	for want := int64(1); want <= 3; want++ {
		got, err := f.s.NextRunNumber(f.ctx, repo.ID, "ci.yml")
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	other, err := f.s.NextRunNumber(f.ctx, repo.ID, "release.yml")
	require.NoError(t, err)
	require.Equal(t, int64(1), other, "run numbers are per workflow path")

	// Concurrent allocation must never hand out the same number twice.
	const n = 20
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := f.s.NextRunNumber(f.ctx, repo.ID, "ci.yml")
			errs[i] = err
			mu.Lock()
			defer mu.Unlock()
			if seen[v] {
				errs[i] = fmt.Errorf("run number %d handed out twice", v)
			}
			seen[v] = true
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, seen, n)
}

func testJobs(t *testing.T, f *fixture) {
	repo := f.repo(50, "acme", "widget")
	run := f.run(repo.ID)

	created := nowUTC()
	j := &model.Job{
		RunID: run.ID, Key: "build", Name: "build (linux, 1.24)",
		MatrixKey: "linux-1.24", Matrix: map[string]any{"os": "linux", "go": "1.24"},
		Needs: []string{"setup"}, Labels: []string{"self-hosted", "linux"},
		Attempt: 1, MaxAttempts: 3, Status: model.StatusWaiting,
		TimeoutMinutes: 30, Environment: "staging", CheckRunID: 1234,
		Outputs: map[string]string{"digest": "sha256:abc"}, CreatedAt: created,
		ClassificationLog: []string{"matched infra pattern: connection reset"},
	}
	require.NoError(t, f.s.CreateJob(f.ctx, j))
	require.NotZero(t, j.ID)

	got, err := f.s.GetJob(f.ctx, j.ID)
	require.NoError(t, err)
	require.Equal(t, j.Matrix, got.Matrix)
	require.Equal(t, j.Needs, got.Needs)
	require.Equal(t, j.Labels, got.Labels)
	require.Equal(t, j.Outputs, got.Outputs)
	require.Equal(t, j.ClassificationLog, got.ClassificationLog)
	require.Equal(t, "staging", got.Environment)

	done := nowUTC()
	got.Status = model.StatusCompleted
	got.Conclusion = model.ConclusionInfraFailure
	got.Class = model.ClassInfra
	got.FailureExplained = "The sandbox host lost its network before the first step ran."
	got.CompletedAt = &done
	got.InfraRetryCount = 2
	require.NoError(t, f.s.UpdateJob(f.ctx, got))

	after, err := f.s.GetJob(f.ctx, j.ID)
	require.NoError(t, err)
	require.Equal(t, model.ConclusionInfraFailure, after.Conclusion)
	require.Equal(t, model.ClassInfra, after.Class)
	require.Equal(t, 2, after.InfraRetryCount)

	f.job(run.ID, "test", []string{"linux"})
	list, err := f.s.ListJobsForRun(f.ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, j.ID, list[0].ID, "ListJobsForRun is id-ordered")
}

func testConcurrencyGroup(t *testing.T, f *fixture) {
	repo := f.repo(60, "acme", "widget")
	run := f.run(repo.ID)
	base := nowUTC()

	mk := func(key string, offset time.Duration, status model.Status) *model.Job {
		j := &model.Job{
			RunID: run.ID, Key: key, Name: key, Attempt: 1, Status: status,
			ConcurrencyGroup: "deploy-prod", CancelInProgress: true,
			CreatedAt: base.Add(offset),
		}
		require.NoError(t, f.s.CreateJob(f.ctx, j))
		return j
	}
	oldest := mk("a", 0, model.StatusInProgress)
	mk("b", time.Second, model.StatusQueued)
	finished := mk("c", 2*time.Second, model.StatusCompleted)

	live, err := f.s.ListJobsInConcurrencyGroup(f.ctx, "deploy-prod")
	require.NoError(t, err)
	require.Len(t, live, 2, "completed jobs are not live members of the group")
	require.Equal(t, oldest.ID, live[0].ID, "oldest first")
	for _, j := range live {
		require.NotEqual(t, finished.ID, j.ID)
	}

	_, err = f.s.ListJobsInConcurrencyGroup(f.ctx, "")
	require.Error(t, err, "an empty group is a programming error, not a match-everything query")
}

func testSteps(t *testing.T, f *fixture) {
	repo := f.repo(70, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", nil)

	started := nowUTC()
	s1 := &model.Step{
		JobID: job.ID, Number: 1, Name: "checkout", StepID: "co", Attempt: 1,
		Status: model.StatusInProgress, StartedAt: &started, LogStart: 0, LogEnd: 40,
	}
	require.NoError(t, f.s.UpsertStep(f.ctx, s1))
	require.NotZero(t, s1.ID)
	firstID := s1.ID

	s1.Status = model.StatusCompleted
	s1.Conclusion = model.ConclusionSuccess
	s1.Outputs = map[string]string{"ref": "main"}
	require.NoError(t, f.s.UpsertStep(f.ctx, s1))
	require.Equal(t, firstID, s1.ID, "upsert keys on (job, attempt, number)")

	s2 := &model.Step{JobID: job.ID, Number: 2, Name: "build", Attempt: 1, Status: model.StatusQueued}
	require.NoError(t, f.s.UpsertStep(f.ctx, s2))

	// A second attempt keeps its own steps.
	retry := &model.Step{JobID: job.ID, Number: 1, Name: "checkout", Attempt: 2, Status: model.StatusQueued}
	require.NoError(t, f.s.UpsertStep(f.ctx, retry))

	steps, err := f.s.ListSteps(f.ctx, job.ID, 1)
	require.NoError(t, err)
	require.Len(t, steps, 2)
	require.Equal(t, 1, steps[0].Number)
	require.Equal(t, model.ConclusionSuccess, steps[0].Conclusion)
	require.Equal(t, map[string]string{"ref": "main"}, steps[0].Outputs)

	second, err := f.s.ListSteps(f.ctx, job.ID, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)

	require.ErrorContains(t, f.s.UpsertStep(f.ctx, &model.Step{Number: 1, Status: model.StatusQueued}),
		"no job id")
}

func testAnnotations(t *testing.T, f *fixture) {
	repo := f.repo(80, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "lint", nil)

	require.NoError(t, f.s.AddAnnotations(f.ctx, job.ID, nil), "an empty batch is a no-op")

	in := []model.Annotation{
		{Path: "main.go", StartLine: 10, EndLine: 10, StartCol: 2, EndCol: 8,
			Level: model.AnnotationFailure, Message: "undefined: foo", Title: "compile error"},
		{Path: "util.go", StartLine: 3, EndLine: 4,
			Level: model.AnnotationWarning, Message: "shadowed variable", RawDetail: "vet"},
	}
	require.NoError(t, f.s.AddAnnotations(f.ctx, job.ID, in))

	got, err := f.s.ListAnnotations(f.ctx, job.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "main.go", got[0].Path)
	require.Equal(t, model.AnnotationFailure, got[0].Level)
	require.Equal(t, job.ID, got[0].JobID)
	require.NotZero(t, got[0].ID)
	require.Equal(t, "vet", got[1].RawDetail)

	err = f.s.AddAnnotations(f.ctx, job.ID, []model.Annotation{{Level: "catastrophe"}})
	require.ErrorContains(t, err, "invalid level")
}

func testArtifacts(t *testing.T, f *fixture) {
	repo := f.repo(90, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", nil)
	created := nowUTC()

	a := &model.Artifact{
		RunID: run.ID, JobID: job.ID, Name: "binaries", StorageKey: "blob/1",
		CreatedAt: created, ExpiresAt: created.Add(90 * 24 * time.Hour),
	}
	require.NoError(t, f.s.CreateArtifact(f.ctx, a))
	require.NotZero(t, a.ID)

	got, err := f.s.GetArtifact(f.ctx, a.ID)
	require.NoError(t, err)
	require.False(t, got.Finalized)
	require.Zero(t, got.SizeBytes)

	require.NoError(t, f.s.FinalizeArtifact(f.ctx, a.ID, 4096, "sha256:beef"))
	got, err = f.s.GetArtifact(f.ctx, a.ID)
	require.NoError(t, err)
	require.True(t, got.Finalized)
	require.Equal(t, int64(4096), got.SizeBytes)
	require.Equal(t, "sha256:beef", got.Digest)
	require.NotNil(t, got.FinalizedAt)

	found, err := f.s.FindArtifact(f.ctx, run.ID, "binaries")
	require.NoError(t, err)
	require.Equal(t, a.ID, found.ID)

	_, err = f.s.FindArtifact(f.ctx, run.ID, "nope")
	require.ErrorIs(t, err, store.ErrNotFound)

	expired := &model.Artifact{
		RunID: run.ID, Name: "logs", CreatedAt: created.Add(-time.Hour),
		ExpiresAt: created.Add(-time.Minute),
	}
	require.NoError(t, f.s.CreateArtifact(f.ctx, expired))

	all, err := f.s.ListArtifacts(f.ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, all, 2)

	gone, err := f.s.DeleteExpiredArtifacts(f.ctx, created)
	require.NoError(t, err)
	require.Len(t, gone, 1)
	require.Equal(t, expired.ID, gone[0].ID, "expiry returns what it removed so the blobs can follow")

	all, err = f.s.ListArtifacts(f.ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	require.ErrorContains(t, f.s.CreateArtifact(f.ctx, &model.Artifact{RunID: run.ID}), "no name")
}

func testRunners(t *testing.T, f *fixture) {
	first := nowUTC()
	r := &model.Runner{
		ID: "runner-a", Name: "alpha", Labels: []string{"self-hosted", "linux"},
		Group: "default", State: model.RunnerIdle, Capacity: 2, Version: "1.0",
		OS: "linux", Arch: "amd64", FirstSeenAt: first, LastHeartbeat: first,
	}
	require.NoError(t, f.s.RegisterRunner(f.ctx, r))

	got, err := f.s.GetRunner(f.ctx, "runner-a")
	require.NoError(t, err)
	require.Equal(t, []string{"self-hosted", "linux"}, got.Labels)
	require.Equal(t, model.RunnerIdle, got.State)
	require.True(t, first.Equal(got.FirstSeenAt))

	later := first.Add(time.Minute)
	require.NoError(t, f.s.RunnerHeartbeat(f.ctx, "runner-a", later))
	got, err = f.s.GetRunner(f.ctx, "runner-a")
	require.NoError(t, err)
	require.True(t, later.Equal(got.LastHeartbeat))

	f.runner("runner-b", []string{"macos"}, model.RunnerIdle)
	all, err := f.s.ListRunners(f.ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	// runner-a beat at first+1m; runner-b has not beaten since registration.
	deadline := first.Add(30 * time.Second)
	offline, err := f.s.MarkOfflineRunners(f.ctx, deadline)
	require.NoError(t, err)
	require.Len(t, offline, 1, "only the stale runner goes offline")
	require.Equal(t, "runner-b", offline[0].ID)
	require.Equal(t, model.RunnerOffline, offline[0].State)

	again, err := f.s.MarkOfflineRunners(f.ctx, deadline)
	require.NoError(t, err)
	require.Empty(t, again, "an already-offline runner is not reported twice")

	// A heartbeat is proof a runner is not offline.
	require.NoError(t, f.s.RunnerHeartbeat(f.ctx, "runner-b", later.Add(time.Minute)))
	back, err := f.s.GetRunner(f.ctx, "runner-b")
	require.NoError(t, err)
	require.Equal(t, model.RunnerIdle, back.State)

	require.ErrorContains(t, f.s.RegisterRunner(f.ctx, &model.Runner{ID: "x", State: "asleep"}),
		"invalid state")
}

func testEvents(t *testing.T, f *fixture) {
	repo := f.repo(100, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", nil)
	at := nowUTC()

	require.NoError(t, f.s.RecordEvent(f.ctx, store.Event{
		RunID: run.ID, JobID: job.ID, Kind: "classified",
		Message: "Classified as an infrastructure failure: the registry returned 502.",
		Detail:  map[string]any{"class": "infra", "matcher": "registry-5xx"},
		At:      at,
	}))
	require.NoError(t, f.s.RecordEvent(f.ctx, store.Event{
		RunID: run.ID, Kind: "run_started", Message: "Run started.", At: at.Add(time.Second),
	}))

	forJob, err := f.s.ListEvents(f.ctx, 0, job.ID)
	require.NoError(t, err)
	require.Len(t, forJob, 1)
	require.Equal(t, "classified", forJob[0].Kind)
	require.Equal(t, map[string]any{"class": "infra", "matcher": "registry-5xx"}, forJob[0].Detail)
	require.NotZero(t, forJob[0].ID)
	require.True(t, at.Equal(forJob[0].At))

	forRun, err := f.s.ListEvents(f.ctx, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, forRun, 2)
	require.Equal(t, "classified", forRun[0].Kind, "oldest first")

	require.ErrorContains(t, f.s.RecordEvent(f.ctx, store.Event{RunID: run.ID}), "no kind")
}

func testNotFound(t *testing.T, f *fixture) {
	_, err := f.s.GetRepo(f.ctx, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetRepoByName(f.ctx, "nobody", "nothing")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetRun(f.ctx, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetJob(f.ctx, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetRunner(f.ctx, "ghost")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetArtifact(f.ctx, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetCache(f.ctx, 999)
	require.ErrorIs(t, err, store.ErrNotFound)

	require.ErrorIs(t, f.s.RunnerHeartbeat(f.ctx, "ghost", nowUTC()), store.ErrNotFound)
	require.ErrorIs(t, f.s.FinalizeArtifact(f.ctx, 999, 1, "d"), store.ErrNotFound)
	require.ErrorIs(t, f.s.FinalizeCache(f.ctx, 999, 1), store.ErrNotFound)
	require.ErrorIs(t, f.s.TouchCache(f.ctx, 999, nowUTC()), store.ErrNotFound)
	require.ErrorIs(t, f.s.DeleteSecret(f.ctx, "repo", "acme/widget", "ghost"), store.ErrNotFound)
	require.ErrorIs(t, f.s.DeleteVar(f.ctx, "repo", "acme/widget", "ghost"), store.ErrNotFound)

	require.ErrorIs(t, f.s.UpdateRun(f.ctx, &model.Run{ID: 999, Status: model.StatusQueued}),
		store.ErrNotFound)
	require.ErrorIs(t, f.s.UpdateJob(f.ctx, &model.Job{ID: 999, Status: model.StatusQueued}),
		store.ErrNotFound)

	_, _, err = f.s.LookupCache(f.ctx, 999, "k", nil, "v1", "refs/heads/main")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.ErrorIs(t, f.s.Enqueue(f.ctx, store.QueuedJob{JobID: 999}), store.ErrNotFound)
}

func testDeepCopy(t *testing.T, f *fixture) {
	repo := f.repo(110, "acme", "widget")
	run := f.run(repo.ID)
	labels := []string{"linux"}
	matrix := map[string]any{"os": "linux"}
	j := &model.Job{
		RunID: run.ID, Key: "build", Name: "build", Labels: labels, Matrix: matrix,
		Attempt: 1, Status: model.StatusWaiting, CreatedAt: nowUTC(),
	}
	require.NoError(t, f.s.CreateJob(f.ctx, j))

	// Mutating the caller's own struct after the write must not reach storage.
	labels[0] = "mutated-on-write"
	matrix["os"] = "mutated-on-write"

	got, err := f.s.GetJob(f.ctx, j.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"linux"}, got.Labels)
	require.Equal(t, map[string]any{"os": "linux"}, got.Matrix)

	// Mutating a returned value must not reach storage either.
	got.Labels[0] = "mutated-on-read"
	got.Matrix["os"] = "mutated-on-read"
	got.Name = "mutated-on-read"

	again, err := f.s.GetJob(f.ctx, j.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"linux"}, again.Labels)
	require.Equal(t, map[string]any{"os": "linux"}, again.Matrix)
	require.Equal(t, "build", again.Name)
}
