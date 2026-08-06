// Package storetest is the conformance suite every store.Store implementation
// must pass. It is one suite run twice, so "works in memory but not in
// Postgres" is a test failure rather than a production incident.
package storetest

import (
	"context"
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
		{"FreshCacheEntryIsNotEvictedFirst", testFreshCacheEntryIsNotEvictedFirst},
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
