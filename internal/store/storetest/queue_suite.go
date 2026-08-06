// The durable queue and the lease protocol, which is where a lost runner
// becomes a requeue rather than a lost or failed job.
package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

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

// A never-read entry must sort by its creation time, not by a zero
// last_accessed. Otherwise a just-committed entry is the oldest thing in the
// repo and evicts itself on the very commit that created it.
func testFreshCacheEntryIsNotEvictedFirst(t *testing.T, f *fixture) {
	repo := f.repo(126, "acme", "widget")
	base := nowUTC()

	old := &model.CacheEntry{RepoID: repo.ID, Key: "old", Version: "v1", SizeBytes: 100,
		StorageKey: "k-old", CreatedAt: base.Add(-time.Hour), Finalized: true}
	require.NoError(t, f.s.ReserveCache(f.ctx, old))
	require.NoError(t, f.s.FinalizeCache(f.ctx, old.ID, 100))

	// Deliberately left with a zero LastAccessed: never read since creation.
	fresh := &model.CacheEntry{RepoID: repo.ID, Key: "fresh", Version: "v1", SizeBytes: 100,
		StorageKey: "k-fresh", CreatedAt: base, Finalized: true}
	require.NoError(t, f.s.ReserveCache(f.ctx, fresh))
	require.NoError(t, f.s.FinalizeCache(f.ctx, fresh.ID, 100))

	evicted, err := f.s.EvictCaches(f.ctx, repo.ID, 100, base)
	require.NoError(t, err)
	require.Len(t, evicted, 1, "one entry over quota")
	require.Equal(t, "old", evicted[0].Key, "the fresh entry must not evict itself")

	entries, err := f.s.ListCacheEntries(f.ctx, repo.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "fresh", entries[0].Key)
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
	require.Equal(t, "runner-gone", reaped[0].RunnerID,
		"the reaped copy must name the runner it lost, or the requeue cannot say who vanished")
	require.Nil(t, reaped[0].LeaseExpiresAt)

	// The STORED row is unleased and names nobody; only the returned copy keeps
	// the lost runner, so nothing downstream mistakes the job for still-leased.
	stored, err := f.s.GetJob(f.ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusQueued, stored.Status)
	require.Equal(t, 1, stored.RequeueCount)
	require.Empty(t, stored.RunnerID)

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
