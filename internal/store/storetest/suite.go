// Package storetest is the conformance suite every store.Store implementation
// must pass. It is one suite run twice, so "works in memory but not in
// Postgres" is a test failure rather than a production incident.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Scope keys the suite builds by hand. Both stores agree on this shape, and
// ResolveSecrets/ResolveVars read exactly these keys.
func orgKey(owner string) string            { return owner }
func repoKey(owner, repo string) string     { return owner + "/" + repo }
func envKey(owner, repo, env string) string { return owner + "/" + repo + "/" + env }

// now is truncated to microseconds because Postgres timestamptz stores
// microseconds; a nanosecond-precision fixture would fail to round-trip through
// the durable store and pass in memory.
func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

type fixture struct {
	t   *testing.T
	ctx context.Context
	s   store.Store
}

func newFixture(t *testing.T, newStore func(t *testing.T) store.Store) *fixture {
	t.Helper()
	return &fixture{t: t, ctx: context.Background(), s: newStore(t)}
}

func (f *fixture) repo(id int64, owner, name string) *model.Repo {
	f.t.Helper()
	r := &model.Repo{ID: id, Owner: owner, Name: name, DefaultBranch: "main", InstallationID: 7}
	require.NoError(f.t, f.s.UpsertRepo(f.ctx, r))
	return r
}

func (f *fixture) run(repoID int64) *model.Run {
	f.t.Helper()
	r := &model.Run{
		RepoID: repoID, RepoFull: "acme/widget", WorkflowName: "CI",
		WorkflowPath: ".github/workflows/ci.yml", RunNumber: 1, Attempt: 1,
		Event: "push", HeadSHA: "deadbeef", HeadBranch: "main", Actor: "octocat",
		Status: model.StatusQueued, CreatedAt: nowUTC(),
	}
	require.NoError(f.t, f.s.CreateRun(f.ctx, r))
	return r
}

func (f *fixture) job(runID int64, key string, labels []string) *model.Job {
	f.t.Helper()
	j := &model.Job{
		RunID: runID, Key: key, Name: key, Labels: labels, Attempt: 1, MaxAttempts: 3,
		Status: model.StatusWaiting, CreatedAt: nowUTC(),
	}
	require.NoError(f.t, f.s.CreateJob(f.ctx, j))
	return j
}

func (f *fixture) runner(id string, labels []string, state model.RunnerState) *model.Runner {
	f.t.Helper()
	r := &model.Runner{
		ID: id, Name: id, Labels: labels, State: state, Capacity: 1,
		FirstSeenAt: nowUTC(), LastHeartbeat: nowUTC(),
	}
	require.NoError(f.t, f.s.RegisterRunner(f.ctx, r))
	return r
}

// RunSuite runs the whole conformance suite against one implementation.
// newStore must return a migrated, empty store; the caller owns its teardown.
func RunSuite(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, *fixture)
	}{
		{"MigrateIsIdempotent", testMigrateIdempotent},
		{"Repos", testRepos},
		{"Runs", testRuns},
		{"RunFilters", testRunFilters},
		{"RunNumbers", testRunNumbers},
		{"Jobs", testJobs},
		{"ConcurrencyGroup", testConcurrencyGroup},
		{"Steps", testSteps},
		{"Annotations", testAnnotations},
		{"Artifacts", testArtifacts},
		{"Runners", testRunners},
		{"Events", testEvents},
		{"NotFound", testNotFound},
		{"DeepCopyOnReadAndWrite", testDeepCopy},
		{"EnqueueIsIdempotent", testEnqueueIdempotent},
		{"EnqueueRejectsAnAttemptMismatch", testEnqueueAttemptMismatch},
		{"ConcurrentDequeue", testConcurrentDequeue},
		{"LabelMatching", testLabelMatching},
		{"NotBeforeIsHonoured", testNotBefore},
		{"HeartbeatByWrongRunner", testHeartbeatWrongRunner},
		{"LeaseExpiryRequeues", testLeaseExpiryRequeues},
		{"ReleaseLeaseRecordsReason", testReleaseLease},
		{"DispatchIsIdempotent", testDispatchIdempotent},
		{"PriorityOrdering", testPriorityOrdering},
		{"CompletedJobLeavesQueue", testCompletedJobLeavesQueue},
		{"QueueStatsStarvation", testQueueStatsStarvation},
		{"QueueSamples", testQueueSamples},
		{"CacheRestoreKeys", testCacheRestoreKeys},
		{"CacheEviction", testCacheEviction},
		{"CacheEvents", testCacheEvents},
		{"SecretScopePrecedence", testSecretScopes},
		{"VarScopePrecedence", testVarScopes},
		{"RejectsUnexplainedAndMalformed", testValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newFixture(t, newStore))
		})
	}
}

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

func testEnqueueIdempotent(t *testing.T, f *fixture) {
	repo := f.repo(120, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})
	q := store.QueuedJob{JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC()}

	require.NoError(t, f.s.Enqueue(f.ctx, q))
	require.NoError(t, f.s.Enqueue(f.ctx, q), "enqueuing a queued job is a no-op, not an error")
	require.NoError(t, f.s.Enqueue(f.ctx, q))

	stats, err := f.s.QueueStats(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Depth, "three enqueues produce one row")

	leased, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, job.ID, leased.ID)

	require.NoError(t, f.s.Enqueue(f.ctx, q), "enqueuing a leased job is a no-op, not an error")
	held, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusInProgress, held.Status, "the lease survived the re-enqueue")
	require.Equal(t, "runner-a", held.RunnerID)

	_, err = f.s.Dequeue(f.ctx, "runner-b", []string{"linux"}, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound, "a leased job is not handed out again")
}

// Attempt is half of the dispatch idempotency key, so a caller that states an
// attempt the job row disagrees with has to be refused: picking one silently
// would let the same attempt dispatch twice with side effects.
func testEnqueueAttemptMismatch(t *testing.T, f *fixture) {
	repo := f.repo(125, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})

	base := store.QueuedJob{JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC()}

	wrong := base
	wrong.Attempt = job.Attempt + 1
	require.Error(t, f.s.Enqueue(f.ctx, wrong), "an attempt the job row disagrees with must be refused")

	stats, err := f.s.QueueStats(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Equal(t, 0, stats.Depth, "a refused enqueue leaves no row behind")

	// Stating the correct attempt, and omitting it, both work.
	right := base
	right.Attempt = job.Attempt
	require.NoError(t, f.s.Enqueue(f.ctx, right))
	require.NoError(t, f.s.Enqueue(f.ctx, base), "an unstated attempt still means the job's own")

	stats, err = f.s.QueueStats(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Depth)
}

func testConcurrentDequeue(t *testing.T, f *fixture) {
	const jobs = 20
	const runners = 50

	repo := f.repo(130, "acme", "widget")
	run := f.run(repo.ID)
	queuedAt := nowUTC()
	want := map[int64]bool{}
	for i := 0; i < jobs; i++ {
		j := f.job(run.ID, fmt.Sprintf("job-%d", i), []string{"linux"})
		want[j.ID] = true
		require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
			JobID: j.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: queuedAt,
		}))
	}

	var mu sync.Mutex
	got := map[int64]int{}
	errs := make(chan error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runnerID := fmt.Sprintf("runner-%02d", i)
			// Each goroutine claims at most one job. SKIP LOCKED can report an
			// empty queue while another transaction still holds a row, so a
			// miss retries until every job is accounted for.
			for attempt := 0; attempt < 500; attempt++ {
				j, err := f.s.Dequeue(f.ctx, runnerID, []string{"linux", "self-hosted"}, time.Minute)
				if errors.Is(err, store.ErrNotFound) {
					mu.Lock()
					done := len(got) == jobs
					mu.Unlock()
					if done {
						return
					}
					time.Sleep(time.Millisecond)
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				got[j.ID]++
				mu.Unlock()
				if j.RunnerID != runnerID {
					errs <- fmt.Errorf("job %d leased to %q but returned runner %q",
						j.ID, runnerID, j.RunnerID)
				}
				return
			}
			errs <- fmt.Errorf("runner %s gave up before the queue drained", runnerID)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Len(t, got, jobs, "every job was dequeued")
	for id, n := range got {
		require.Equal(t, 1, n, "job %d was dequeued %d times; exactly once is the contract", id, n)
		require.True(t, want[id], "dequeued a job that was never enqueued: %d", id)
	}

	stats, err := f.s.QueueStats(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Equal(t, 0, stats.Depth, "nothing is left waiting")
}

func testLabelMatching(t *testing.T, f *fixture) {
	repo := f.repo(140, "acme", "widget")
	run := f.run(repo.ID)
	strict := f.job(run.ID, "strict", []string{"self-hosted", "linux"})
	loose := f.job(run.ID, "loose", []string{"linux"})
	free := f.job(run.ID, "free", nil)

	at := nowUTC()
	for _, j := range []*model.Job{strict, loose, free} {
		require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
			JobID: j.ID, RunID: run.ID, Labels: j.Labels, QueuedAt: at,
		}))
	}

	// A runner offering only [linux] cannot take [self-hosted, linux].
	got, err := f.s.Dequeue(f.ctx, "linux-only", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, strict.ID, got.ID,
		"a job needing [self-hosted linux] must not go to a runner with only [linux]")

	got2, err := f.s.Dequeue(f.ctx, "linux-only-2", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, strict.ID, got2.ID)

	_, err = f.s.Dequeue(f.ctx, "linux-only-3", []string{"linux"}, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound, "the strict job stays queued")

	// A runner with a superset of the required labels takes it.
	got3, err := f.s.Dequeue(f.ctx, "big", []string{"self-hosted", "linux", "gpu"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, strict.ID, got3.ID)

	// An unlabelled job is eligible for any runner: both earlier dequeues were
	// either it or the loose job.
	require.ElementsMatch(t, []int64{loose.ID, free.ID}, []int64{got.ID, got2.ID})
}

func testNotBefore(t *testing.T, f *fixture) {
	repo := f.repo(150, "acme", "widget")
	run := f.run(repo.ID)
	backedOff := f.job(run.ID, "retry", []string{"linux"})
	ready := f.job(run.ID, "ready", []string{"linux"})

	at := nowUTC()
	const backoff = 400 * time.Millisecond
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: backedOff.ID, RunID: run.ID, Labels: []string{"linux"},
		QueuedAt: at.Add(-time.Hour), NotBefore: at.Add(backoff), Priority: 10,
	}))
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: ready.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: at,
	}))

	// The backed-off job sorts first on both priority and queued_at, so if
	// not_before were ignored it would be the one handed out.
	got, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, ready.ID, got.ID, "a backed-off retry is not dispatched early")

	_, err = f.s.Dequeue(f.ctx, "runner-b", []string{"linux"}, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound)

	time.Sleep(backoff + 200*time.Millisecond)
	late, err := f.s.Dequeue(f.ctx, "runner-c", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, backedOff.ID, late.ID, "once not_before passes the retry is dispatchable")
}

func testHeartbeatWrongRunner(t *testing.T, f *fixture) {
	repo := f.repo(160, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC(),
	}))

	leased, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased.LeaseExpiresAt)

	require.NoError(t, f.s.Heartbeat(f.ctx, "runner-a", job.ID, time.Minute))
	extended, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.True(t, extended.LeaseExpiresAt.After(*leased.LeaseExpiresAt), "the lease was extended")

	require.ErrorIs(t, f.s.Heartbeat(f.ctx, "runner-b", job.ID, time.Minute), store.ErrLeaseLost,
		"only the holder may extend a lease")
	require.ErrorIs(t, f.s.Heartbeat(f.ctx, "runner-a", 999999, time.Minute), store.ErrLeaseLost,
		"a job with no lease cannot be heartbeated")

	unchanged, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.True(t, extended.LeaseExpiresAt.Equal(*unchanged.LeaseExpiresAt),
		"a rejected heartbeat changes nothing")

	require.Error(t, f.s.Heartbeat(f.ctx, "runner-a", job.ID, 0), "a non-positive ttl is rejected")
}

func testLeaseExpiryRequeues(t *testing.T, f *fixture) {
	repo := f.repo(170, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC(),
	}))

	leased, err := f.s.Dequeue(f.ctx, "runner-gone", []string{"linux"}, time.Second)
	require.NoError(t, err)
	require.Equal(t, model.StatusInProgress, leased.Status)
	require.Equal(t, 0, leased.RequeueCount)

	// Nothing has expired yet.
	nothing, err := f.s.ReapExpiredLeases(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Empty(t, nothing)

	after := leased.LeaseExpiresAt.Add(time.Millisecond)
	reaped, err := f.s.ReapExpiredLeases(f.ctx, after)
	require.NoError(t, err)
	require.Len(t, reaped, 1)
	require.Equal(t, job.ID, reaped[0].ID)
	require.Equal(t, model.StatusQueued, reaped[0].Status,
		"a lost runner is a requeue, never a failure")
	require.Empty(t, reaped[0].Conclusion, "the job has no conclusion: it did not finish")
	require.Equal(t, 1, reaped[0].RequeueCount)
	require.Nil(t, reaped[0].LeaseExpiresAt)
	require.Empty(t, reaped[0].RunnerID)

	stored, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusQueued, stored.Status)
	require.Equal(t, 1, stored.RequeueCount)

	events, err := f.s.ListEvents(f.ctx, 0, job.ID)
	require.NoError(t, err)
	var lost *store.Event
	for i := range events {
		if events[i].Kind == "runner_lost" {
			lost = &events[i]
		}
	}
	require.NotNil(t, lost, "the requeue is explained in the event log")
	require.Contains(t, lost.Message, "runner-gone")
	require.NotEmpty(t, lost.Message, "the event carries a human sentence, not a code")
	require.Equal(t, "runner_lost", lost.Detail["actor"])

	// Reaping again must not double-count: the lease is already gone.
	twice, err := f.s.ReapExpiredLeases(f.ctx, after)
	require.NoError(t, err)
	require.Empty(t, twice)
	stored, err = f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.RequeueCount)

	// The requeued job is dispatchable again, at the same attempt.
	redelivered, err := f.s.Dequeue(f.ctx, "runner-new", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, job.ID, redelivered.ID)
	require.Equal(t, 1, redelivered.Attempt)
}

func testReleaseLease(t *testing.T, f *fixture) {
	repo := f.repo(180, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC(),
	}))
	_, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	// A requeue with no explanation is rejected before it touches storage.
	err = f.s.ReleaseLease(f.ctx, "runner-a", job.ID, model.CancelReason{Actor: model.CancelActorShutdown})
	require.ErrorContains(t, err, "no explanation sentence")
	err = f.s.ReleaseLease(f.ctx, "runner-a", job.ID, model.CancelReason{Sentence: "because"})
	require.ErrorContains(t, err, "unknown actor")

	still, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusInProgress, still.Status, "a rejected release changes nothing")

	reason := model.CancelReason{
		Actor:    model.CancelActorShutdown,
		Sentence: "The control plane is shutting down, so this job went back on the queue untouched.",
	}
	require.ErrorIs(t, f.s.ReleaseLease(f.ctx, "runner-b", job.ID, reason), store.ErrLeaseLost,
		"only the holder may release a lease")
	require.NoError(t, f.s.ReleaseLease(f.ctx, "runner-a", job.ID, reason))

	back, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusQueued, back.Status)
	require.Equal(t, 1, back.RequeueCount)

	events, err := f.s.ListEvents(f.ctx, 0, job.ID)
	require.NoError(t, err)
	var requeued *store.Event
	for i := range events {
		if events[i].Kind == "requeued" {
			requeued = &events[i]
		}
	}
	require.NotNil(t, requeued)
	require.Equal(t, reason.Sentence, requeued.Message)
	require.Equal(t, string(model.CancelActorShutdown), requeued.Detail["actor"])
}

func testDispatchIdempotent(t *testing.T, f *fixture) {
	repo := f.repo(190, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "deploy", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC(),
	}))

	_, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	reason := model.CancelReason{
		Actor:    model.CancelActorRunnerLost,
		Sentence: "The runner went away mid-job, so the job went back on the queue.",
	}
	require.NoError(t, f.s.ReleaseLease(f.ctx, "runner-a", job.ID, reason))

	// Same (run, job, attempt), handed to a different runner.
	again, err := f.s.Dequeue(f.ctx, "runner-b", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, job.ID, again.ID)
	require.Equal(t, 1, again.Attempt)

	events, err := f.s.ListEvents(f.ctx, 0, job.ID)
	require.NoError(t, err)
	var dispatched, redispatched int
	for _, e := range events {
		switch e.Kind {
		case "dispatched":
			dispatched++
		case "redispatched":
			redispatched++
		}
	}
	require.Equal(t, 1, dispatched,
		"(run, job, attempt) is dispatched exactly once, however many times it is redelivered")
	require.Equal(t, 1, redispatched, "the redelivery is recorded, not hidden")
}

func testPriorityOrdering(t *testing.T, f *fixture) {
	repo := f.repo(200, "acme", "widget")
	run := f.run(repo.ID)
	at := nowUTC()

	low := f.job(run.ID, "low", []string{"linux"})
	high := f.job(run.ID, "high", []string{"linux"})
	older := f.job(run.ID, "older", []string{"linux"})

	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: older.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: at.Add(-time.Hour)}))
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: low.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: at}))
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: high.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: at, Priority: 5}))

	first, err := f.s.Dequeue(f.ctx, "r1", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, high.ID, first.ID, "priority beats age")

	second, err := f.s.Dequeue(f.ctx, "r2", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, older.ID, second.ID, "within a priority, oldest first")
}

func testCompletedJobLeavesQueue(t *testing.T, f *fixture) {
	repo := f.repo(210, "acme", "widget")
	run := f.run(repo.ID)
	job := f.job(run.ID, "build", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Labels: []string{"linux"}, QueuedAt: nowUTC(),
	}))
	leased, err := f.s.Dequeue(f.ctx, "runner-a", []string{"linux"}, time.Second)
	require.NoError(t, err)

	done := nowUTC()
	leased.Status = model.StatusCompleted
	leased.Conclusion = model.ConclusionSuccess
	leased.CompletedAt = &done
	require.NoError(t, f.s.UpdateJob(f.ctx, leased))

	// A finished job must not come back from the reaper, whatever its lease said.
	reaped, err := f.s.ReapExpiredLeases(f.ctx, done.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, reaped, "a completed job is no longer queue state")

	stats, err := f.s.QueueStats(f.ctx, nowUTC())
	require.NoError(t, err)
	require.Equal(t, 0, stats.Depth)
}

func testQueueStatsStarvation(t *testing.T, f *fixture) {
	repo := f.repo(220, "acme", "widget")
	run := f.run(repo.ID)
	at := nowUTC()

	gpu := f.job(run.ID, "train", []string{"self-hosted", "gpu"})
	linux := f.job(run.ID, "build", []string{"linux"})
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: gpu.ID, RunID: run.ID, Labels: gpu.Labels, QueuedAt: at.Add(-5 * time.Minute)}))
	require.NoError(t, f.s.Enqueue(f.ctx, store.QueuedJob{
		JobID: linux.ID, RunID: run.ID, Labels: linux.Labels, QueuedAt: at.Add(-time.Minute)}))

	f.runner("idle-linux", []string{"linux", "self-hosted"}, model.RunnerIdle)
	f.runner("busy-linux", []string{"linux"}, model.RunnerBusy)
	f.runner("offline-gpu", []string{"gpu"}, model.RunnerOffline)
	f.runner("drained-gpu", []string{"gpu"}, model.RunnerDrained)

	stats, err := f.s.QueueStats(f.ctx, at)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Depth)
	require.Equal(t, map[string]int{"self-hosted": 1, "gpu": 1, "linux": 1}, stats.DepthByLabel)
	require.Equal(t, gpu.ID, stats.OldestJobID)
	require.Equal(t, 5*time.Minute, stats.OldestWaiting)
	require.Equal(t, map[string]int{"linux": 2, "self-hosted": 1}, stats.RunnersByLabel,
		"offline and drained runners cannot take new work, so they are not counted")
	require.Equal(t, map[string]int{"linux": 1, "self-hosted": 1}, stats.IdleByLabel)
	require.Equal(t, []string{"gpu"}, stats.StarvedLabels,
		"gpu has queued work and no runner that will ever take it")
	require.Equal(t, at, stats.At)
}

func testQueueSamples(t *testing.T, f *fixture) {
	at := nowUTC()
	require.NoError(t, f.s.RecordQueueSample(f.ctx, store.QueueSample{
		At: at.Add(-2 * time.Hour), Depth: 9, Busy: 3, Idle: 1,
		DepthByLabel: map[string]int{"linux": 9},
	}))
	require.NoError(t, f.s.RecordQueueSample(f.ctx, store.QueueSample{
		At: at, Depth: 2, Busy: 1, Idle: 4,
	}))

	recent, err := f.s.QueueDepthHistory(f.ctx, at.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, 2, recent[0].Depth)

	all, err := f.s.QueueDepthHistory(f.ctx, at.Add(-3*time.Hour))
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, 9, all[0].Depth, "oldest first")
	require.Equal(t, map[string]int{"linux": 9}, all[0].DepthByLabel)
}

func testCacheRestoreKeys(t *testing.T, f *fixture) {
	repo := f.repo(230, "acme", "widget")
	base := nowUTC()

	mk := func(key string, created time.Duration, finalized bool) *model.CacheEntry {
		e := &model.CacheEntry{
			RepoID: repo.ID, Key: key, Version: "v1", Ref: "refs/heads/main",
			StorageKey: "blob/" + key, CreatedAt: base.Add(created), LastAccessed: base.Add(created),
		}
		require.NoError(t, f.s.ReserveCache(f.ctx, e))
		if finalized {
			require.NoError(t, f.s.FinalizeCache(f.ctx, e.ID, 100))
		}
		return e
	}
	mk("deps-old", -3*time.Hour, true)
	newestPrefix := mk("deps-new", -time.Hour, true)
	exact := mk("deps-exact", -2*time.Hour, true)
	mk("deps-pending", time.Hour, false) // newest of all, but never finalized
	mk("build-old", -4*time.Hour, true)

	// Exact key beats every prefix match, even a newer one.
	got, matched, err := f.s.LookupCache(f.ctx, repo.ID, "deps-exact", []string{"deps-"}, "v1", "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, exact.ID, got.ID)
	require.Equal(t, "deps-exact", matched)

	// No exact hit: newest prefix match wins, and the unfinalized entry is
	// invisible however new it is.
	got, matched, err = f.s.LookupCache(f.ctx, repo.ID, "deps-missing", []string{"deps-"}, "v1", "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, newestPrefix.ID, got.ID)
	require.Equal(t, "deps-", matched)

	// Restore keys are tried in declaration order, not by recency.
	got, matched, err = f.s.LookupCache(f.ctx, repo.ID, "nope", []string{"build-", "deps-"}, "v1", "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, "build-", matched)
	require.Equal(t, "build-old", got.Key)

	// Version is part of the identity.
	_, _, err = f.s.LookupCache(f.ctx, repo.ID, "deps-exact", []string{"deps-"}, "v2", "refs/heads/main")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Another repo's caches are not visible.
	other := f.repo(231, "acme", "other")
	_, _, err = f.s.LookupCache(f.ctx, other.ID, "deps-exact", []string{"deps-"}, "v1", "refs/heads/main")
	require.ErrorIs(t, err, store.ErrNotFound)

	// A restore key matching nothing is a miss, not an error-free wrong answer.
	_, _, err = f.s.LookupCache(f.ctx, repo.ID, "nope", []string{"unrelated-"}, "v1", "refs/heads/main")
	require.ErrorIs(t, err, store.ErrNotFound)

	touched := base.Add(time.Hour)
	require.NoError(t, f.s.TouchCache(f.ctx, exact.ID, touched))
	reread, err := f.s.GetCache(f.ctx, exact.ID)
	require.NoError(t, err)
	require.True(t, touched.Equal(reread.LastAccessed))
}

func testCacheEviction(t *testing.T, f *fixture) {
	repo := f.repo(240, "acme", "widget")
	base := nowUTC()

	mk := func(key string, accessed time.Duration, size int64) *model.CacheEntry {
		e := &model.CacheEntry{
			RepoID: repo.ID, Key: key, Version: "v1",
			CreatedAt: base.Add(-24 * time.Hour), LastAccessed: base.Add(accessed),
		}
		require.NoError(t, f.s.ReserveCache(f.ctx, e))
		require.NoError(t, f.s.FinalizeCache(f.ctx, e.ID, size))
		return e
	}
	coldest := mk("a", -3*time.Hour, 100)
	colder := mk("b", -2*time.Hour, 100)
	warm := mk("c", -time.Hour, 100)

	usage, err := f.s.CacheUsage(f.ctx, repo.ID)
	require.NoError(t, err)
	require.Equal(t, int64(300), usage)

	none, err := f.s.EvictCaches(f.ctx, repo.ID, 1000, base)
	require.NoError(t, err)
	require.Empty(t, none, "under quota evicts nothing")

	evicted, err := f.s.EvictCaches(f.ctx, repo.ID, 150, base)
	require.NoError(t, err)
	require.Len(t, evicted, 2, "eviction stops as soon as the repo is under quota")
	require.Equal(t, coldest.ID, evicted[0].ID, "least recently accessed goes first")
	require.Equal(t, colder.ID, evicted[1].ID)
	require.Equal(t, int64(100), evicted[0].SizeBytes, "the returned entry carries what was lost")
	require.Equal(t, "a", evicted[0].Key)

	usage, err = f.s.CacheUsage(f.ctx, repo.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100), usage)

	_, err = f.s.GetCache(f.ctx, coldest.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	survivor, err := f.s.GetCache(f.ctx, warm.ID)
	require.NoError(t, err)
	require.Equal(t, "c", survivor.Key)

	// Every eviction is on the record with what was dropped.
	events, err := f.s.ListCacheEvents(f.ctx, repo.ID, 0)
	require.NoError(t, err)
	var evictions int
	for _, e := range events {
		if e.Kind == "evict" {
			evictions++
			require.NotEmpty(t, e.Reason, "an eviction without a reason is a silent eviction")
			require.Equal(t, int64(100), e.SizeBytes)
		}
	}
	require.Equal(t, 2, evictions)

	_, err = f.s.EvictCaches(f.ctx, repo.ID, -1, base)
	require.Error(t, err, "a negative quota is a bug, not an instruction to delete everything")
}

func testCacheEvents(t *testing.T, f *fixture) {
	repo := f.repo(250, "acme", "widget")
	at := nowUTC()
	for i, kind := range []string{"miss", "store", "hit"} {
		require.NoError(t, f.s.RecordCacheEvent(f.ctx, model.CacheEvent{
			RepoID: repo.ID, Key: "deps-abc", Kind: kind, MatchedOn: "deps-",
			SizeBytes: 10, At: at.Add(time.Duration(i) * time.Second),
		}))
	}
	require.NoError(t, f.s.RecordCacheEvent(f.ctx, model.CacheEvent{
		RepoID: 999, Key: "other", Kind: "hit", At: at,
	}))

	all, err := f.s.ListCacheEvents(f.ctx, repo.ID, 0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "hit", all[0].Kind, "newest first")
	require.Equal(t, "deps-", all[0].MatchedOn)

	limited, err := f.s.ListCacheEvents(f.ctx, repo.ID, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)

	require.ErrorContains(t,
		f.s.RecordCacheEvent(f.ctx, model.CacheEvent{RepoID: repo.ID, Kind: "vanished"}),
		"unknown kind")
}

func testSecretScopes(t *testing.T, f *fixture) {
	const owner, repo, env = "acme", "widget", "production"

	require.NoError(t, f.s.PutSecret(f.ctx, "org", orgKey(owner), "SHARED", []byte("org-value")))
	require.NoError(t, f.s.PutSecret(f.ctx, "org", orgKey(owner), "ORG_ONLY", []byte("org-only")))
	require.NoError(t, f.s.PutSecret(f.ctx, "repo", repoKey(owner, repo), "SHARED", []byte("repo-value")))
	require.NoError(t, f.s.PutSecret(f.ctx, "repo", repoKey(owner, repo), "REPO_ONLY", []byte("repo-only")))
	require.NoError(t, f.s.PutSecret(f.ctx, "environment", envKey(owner, repo, env), "SHARED", []byte("env-value")))
	require.NoError(t, f.s.PutSecret(f.ctx, "org", orgKey("other"), "SHARED", []byte("someone-else")))

	withEnv, err := f.s.ResolveSecrets(f.ctx, owner, repo, env)
	require.NoError(t, err)
	require.Equal(t, []byte("env-value"), withEnv["SHARED"], "environment beats repo beats org")
	require.Equal(t, []byte("org-only"), withEnv["ORG_ONLY"])
	require.Equal(t, []byte("repo-only"), withEnv["REPO_ONLY"])
	require.Len(t, withEnv, 3)

	noEnv, err := f.s.ResolveSecrets(f.ctx, owner, repo, "")
	require.NoError(t, err)
	require.Equal(t, []byte("repo-value"), noEnv["SHARED"], "with no environment, repo wins")

	names, err := f.s.ListSecretNames(f.ctx, "repo", repoKey(owner, repo))
	require.NoError(t, err)
	require.Equal(t, []string{"REPO_ONLY", "SHARED"}, names)

	require.NoError(t, f.s.DeleteSecret(f.ctx, "environment", envKey(owner, repo, env), "SHARED"))
	afterDelete, err := f.s.ResolveSecrets(f.ctx, owner, repo, env)
	require.NoError(t, err)
	require.Equal(t, []byte("repo-value"), afterDelete["SHARED"], "the repo value is uncovered again")

	require.ErrorContains(t, f.s.PutSecret(f.ctx, "galaxy", "k", "N", []byte("v")), "unknown scope")
	_, err = f.s.ListSecretNames(f.ctx, "galaxy", "k")
	require.ErrorContains(t, err, "unknown scope")
}

func testVarScopes(t *testing.T, f *fixture) {
	const owner, repo, env = "acme", "widget", "production"

	require.NoError(t, f.s.PutVar(f.ctx, "org", orgKey(owner), "REGION", "us-east"))
	require.NoError(t, f.s.PutVar(f.ctx, "repo", repoKey(owner, repo), "REGION", "eu-west"))
	require.NoError(t, f.s.PutVar(f.ctx, "repo", repoKey(owner, repo), "TIER", "standard"))
	require.NoError(t, f.s.PutVar(f.ctx, "environment", envKey(owner, repo, env), "REGION", "ap-south"))

	withEnv, err := f.s.ResolveVars(f.ctx, owner, repo, env)
	require.NoError(t, err)
	require.Equal(t, "ap-south", withEnv["REGION"])
	require.Equal(t, "standard", withEnv["TIER"])

	noEnv, err := f.s.ResolveVars(f.ctx, owner, repo, "")
	require.NoError(t, err)
	require.Equal(t, "eu-west", noEnv["REGION"])

	require.NoError(t, f.s.DeleteVar(f.ctx, "repo", repoKey(owner, repo), "REGION"))
	after, err := f.s.ResolveVars(f.ctx, owner, repo, "")
	require.NoError(t, err)
	require.Equal(t, "us-east", after["REGION"])

	require.ErrorContains(t, f.s.PutVar(f.ctx, "galaxy", "k", "N", "v"), "unknown scope")
	require.ErrorContains(t, f.s.DeleteVar(f.ctx, "galaxy", "k", "N"), "unknown scope")
}

func testValidation(t *testing.T, f *fixture) {
	repo := f.repo(260, "acme", "widget")
	run := f.run(repo.ID)

	require.Error(t, f.s.CreateRun(f.ctx, nil))
	require.Error(t, f.s.CreateJob(f.ctx, nil))
	require.Error(t, f.s.UpsertRepo(f.ctx, nil))

	require.ErrorContains(t,
		f.s.CreateRun(f.ctx, &model.Run{RepoID: repo.ID, Status: "sideways", CreatedAt: nowUTC()}),
		"invalid status")
	require.ErrorContains(t,
		f.s.CreateJob(f.ctx, &model.Job{RunID: run.ID, Status: model.StatusQueued, Class: "gremlins"}),
		"invalid failure class")
	require.ErrorContains(t,
		f.s.CreateJob(f.ctx, &model.Job{RunID: run.ID, Status: model.StatusQueued, Conclusion: "vibes"}),
		"invalid conclusion")

	// The store refuses to record a cancellation nobody can explain.
	require.ErrorContains(t, f.s.CreateRun(f.ctx, &model.Run{
		RepoID: repo.ID, Status: model.StatusCompleted, CreatedAt: nowUTC(),
		Conclusion: model.ConclusionCancelled,
		Cancel:     &model.CancelReason{Actor: model.CancelActorTimeout},
	}), "no explanation sentence")

	// Ids are allocated by the store, never supplied.
	require.ErrorContains(t,
		f.s.CreateRun(f.ctx, &model.Run{ID: 5, RepoID: repo.ID, Status: model.StatusQueued, CreatedAt: nowUTC()}),
		"store allocates ids")
	require.ErrorContains(t,
		f.s.CreateJob(f.ctx, &model.Job{ID: 5, RunID: run.ID, Status: model.StatusQueued}),
		"store allocates ids")

	require.ErrorContains(t, f.s.Enqueue(f.ctx, store.QueuedJob{}), "no job id")
	_, err := f.s.Dequeue(f.ctx, "", []string{"linux"}, time.Minute)
	require.ErrorContains(t, err, "empty runner id")
	_, err = f.s.Dequeue(f.ctx, "r", []string{"linux"}, 0)
	require.ErrorContains(t, err, "ttl must be positive")
}
