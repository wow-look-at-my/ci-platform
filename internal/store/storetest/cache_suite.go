// Cache lookup and eviction, secret and var scoping, and the validation both
// stores must enforce identically.
package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

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
