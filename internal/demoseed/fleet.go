package demoseed

// The fleet and the cache: a starved label with nobody to take its work, and a cache with a restore-key hit and an eviction that says why.

import (
	"context"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// seedFleet gives the queue page something to be honest about: a label with
// queued work and no runner that can take it, which is incident 4's alarm.
func seedFleet(ctx context.Context, st store.Store) error {
	runners := []*model.Runner{
		{
			ID: "runner-a1", Name: "build-01", Labels: []string{"self-hosted", "linux", "x64"},
			State: model.RunnerIdle, Capacity: 2, Version: "0.4.1", OS: "linux", Arch: "amd64",
			FirstSeenAt: ago(31 * 24 * time.Hour), LastHeartbeat: ago(4 * time.Second),
		},
		{
			ID: "runner-a2", Name: "build-02", Labels: []string{"self-hosted", "linux", "x64"},
			State: model.RunnerBusy, CurrentJobID: 2, Capacity: 2, Version: "0.4.1", OS: "linux", Arch: "amd64",
			FirstSeenAt: ago(31 * 24 * time.Hour), LastHeartbeat: ago(2 * time.Second),
		},
		{
			ID: "runner-c1", Name: "arm-01", Labels: []string{"self-hosted", "linux", "arm64"},
			State: model.RunnerIdle, Capacity: 1, Version: "0.4.1", OS: "linux", Arch: "arm64",
			FirstSeenAt: ago(9 * 24 * time.Hour), LastHeartbeat: ago(6 * time.Second),
		},
		{
			ID: "runner-b2", Name: "build-03", Labels: []string{"self-hosted", "linux", "x64"},
			State: model.RunnerOffline, Capacity: 2, Version: "0.4.0", OS: "linux", Arch: "amd64",
			FirstSeenAt: ago(31 * 24 * time.Hour), LastHeartbeat: ago(4*time.Hour + 52*time.Minute),
		},
	}
	for _, r := range runners {
		if err := st.RegisterRunner(ctx, r); err != nil {
			return err
		}
	}

	// Queued work nobody can take: the gpu label has no online runner, which is
	// what turns "the job just sat there" into a named alarm.
	starved := &model.Run{
		RepoID: repoID, RepoFull: owner + "/" + repo,
		WorkflowName: "Benchmarks", WorkflowPath: ".github/workflows/bench.yml",
		RunNumber: 9, Attempt: 1, Event: "workflow_dispatch",
		HeadSHA: "e58c3a7d10b942f6ac83512d0eb47f9163ca25b8", HeadBranch: "main",
		Actor: "sam", Status: model.StatusQueued,
		CreatedAt: ago(37 * time.Minute),
	}
	if err := st.CreateRun(ctx, starved); err != nil {
		return err
	}
	benchJob := &model.Job{
		RunID: starved.ID, Key: "bench", Name: "bench (cuda)",
		Labels: []string{"self-hosted", "linux", "gpu"}, Attempt: 1, MaxAttempts: 1,
		Status: model.StatusQueued, CreatedAt: ago(37 * time.Minute), QueuedAt: agop(37 * time.Minute),
	}
	if err := st.CreateJob(ctx, benchJob); err != nil {
		return err
	}
	if err := st.Enqueue(ctx, store.QueuedJob{
		JobID: benchJob.ID, RunID: starved.ID, Attempt: 1,
		Labels: benchJob.Labels, QueuedAt: ago(37 * time.Minute),
	}); err != nil {
		return err
	}

	for i := 12; i >= 0; i-- {
		at := ago(time.Duration(i) * 30 * time.Minute)
		depth := 1
		if i%3 == 0 {
			depth = 3
		}
		if err := st.RecordQueueSample(ctx, store.QueueSample{
			At: at, Depth: depth, DepthByLabel: map[string]int{"linux": depth, "gpu": 1},
			Busy: 1 + i%2, Idle: 2,
		}); err != nil {
			return err
		}
	}
	return nil
}

// seedCache gives the cache page a hit rate, a restore-key match, and an
// eviction that says what it removed and why.
func seedCache(ctx context.Context, st store.Store) error {
	entries := []*model.CacheEntry{
		{RepoID: repoID, Key: "go-mod-linux-8f21c0", Version: "v1", Ref: "refs/heads/main",
			SizeBytes: 412_884_004, StorageKey: "cache/1", CreatedAt: ago(3 * 24 * time.Hour), LastAccessed: ago(14 * time.Minute)},
		{RepoID: repoID, Key: "go-build-linux-8f21c0", Version: "v1", Ref: "refs/heads/main",
			SizeBytes: 1_204_991_233, StorageKey: "cache/2", CreatedAt: ago(3 * 24 * time.Hour), LastAccessed: ago(15 * time.Minute)},
		{RepoID: repoID, Key: "node-modules-2a7ff1", Version: "v1", Ref: "refs/heads/fix-timeout-parsing",
			SizeBytes: 318_552_112, StorageKey: "cache/3", CreatedAt: ago(2 * 24 * time.Hour), LastAccessed: ago(2 * time.Hour)},
	}
	for _, e := range entries {
		if err := st.ReserveCache(ctx, e); err != nil {
			return err
		}
		if err := st.FinalizeCache(ctx, e.ID, e.SizeBytes); err != nil {
			return err
		}
	}

	events := []model.CacheEvent{
		{RepoID: repoID, Key: "go-mod-linux-8f21c0", Kind: "hit", MatchedOn: "go-mod-linux-8f21c0",
			SizeBytes: 412_884_004, At: ago(14 * time.Minute)},
		{RepoID: repoID, Key: "go-build-linux-9c04ab", Kind: "hit", MatchedOn: "go-build-linux-",
			Reason:    "no exact match; restored from the newest entry under restore-key \"go-build-linux-\"",
			SizeBytes: 1_204_991_233, At: ago(15 * time.Minute)},
		{RepoID: repoID, Key: "node-modules-91b0e2", Kind: "miss",
			Reason: "no entry matched the key or any restore-key for version v1", At: ago(2 * time.Hour)},
		{RepoID: repoID, Key: "node-modules-2a7ff1", Kind: "store", SizeBytes: 318_552_112, At: ago(119 * time.Minute)},
		{RepoID: repoID, Key: "go-build-linux-71ce93", Kind: "evict",
			Reason:    "evicted to stay under the 10737418240 byte cache quota; it was the least recently used entry",
			SizeBytes: 1_198_233_004, At: ago(9 * time.Hour)},
	}
	for _, e := range events {
		if err := st.RecordCacheEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
