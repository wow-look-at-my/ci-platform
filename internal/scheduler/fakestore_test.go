package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// errUnused marks a store method these tests never exercise. Returning an error
// rather than a zero value means a test that starts using one finds out.
var errUnused = errors.New("fakeStore: method not implemented for scheduler tests")

// fakeStore is an in-package stand-in for store.Store. internal/store/mem does
// not exist yet, and this package may not create files under internal/store.
type fakeStore struct {
	repos map[int64]*model.Repo
	runs  map[int64]*model.Run
	jobs  map[int64]*model.Job
	steps map[int64][]*model.Step

	queue   []store.QueuedJob
	events  []store.Event
	secrets map[string][]byte

	nextJobID int64
	// reap is what the next ReapExpiredLeases call returns; the real store
	// decides this from lease timestamps.
	reap []*model.Job
	// failEnqueue makes Enqueue fail, to prove errors are not swallowed.
	failEnqueue bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		repos:   map[int64]*model.Repo{},
		runs:    map[int64]*model.Run{},
		jobs:    map[int64]*model.Job{},
		steps:   map[int64][]*model.Step{},
		secrets: map[string][]byte{},
	}
}

func (f *fakeStore) eventsOfKind(kind string) []store.Event {
	var out []store.Event
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeStore) queued(jobID int64) *store.QueuedJob {
	for i := range f.queue {
		if f.queue[i].JobID == jobID {
			return &f.queue[i]
		}
	}
	return nil
}

// --- Repos ---

func (f *fakeStore) UpsertRepo(_ context.Context, r *model.Repo) error {
	f.repos[r.ID] = r
	return nil
}

func (f *fakeStore) GetRepo(_ context.Context, id int64) (*model.Repo, error) {
	r, ok := f.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) GetRepoByName(context.Context, string, string) (*model.Repo, error) {
	return nil, errUnused
}
func (f *fakeStore) ListRepos(context.Context) ([]*model.Repo, error) { return nil, errUnused }

// --- Runs ---

func (f *fakeStore) CreateRun(_ context.Context, r *model.Run) error {
	f.runs[r.ID] = r
	return nil
}

func (f *fakeStore) GetRun(_ context.Context, id int64) (*model.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) UpdateRun(_ context.Context, r *model.Run) error {
	if _, ok := f.runs[r.ID]; !ok {
		return store.ErrNotFound
	}
	f.runs[r.ID] = r
	return nil
}

func (f *fakeStore) ListRuns(context.Context, store.RunFilter) ([]*model.Run, error) {
	return nil, errUnused
}
func (f *fakeStore) CountRuns(context.Context, store.RunFilter) (int, error) { return 0, errUnused }
func (f *fakeStore) NextRunNumber(context.Context, int64, string) (int64, error) {
	return 0, errUnused
}
func (f *fakeStore) ListRunsForSHA(context.Context, int64, string) ([]*model.Run, error) {
	return nil, errUnused
}

// --- Jobs ---

func (f *fakeStore) CreateJob(_ context.Context, j *model.Job) error {
	f.nextJobID++
	j.ID = f.nextJobID
	f.jobs[j.ID] = j
	return nil
}

func (f *fakeStore) GetJob(_ context.Context, id int64) (*model.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return j, nil
}

func (f *fakeStore) UpdateJob(_ context.Context, j *model.Job) error {
	if _, ok := f.jobs[j.ID]; !ok {
		return store.ErrNotFound
	}
	f.jobs[j.ID] = j
	return nil
}

func (f *fakeStore) ListJobsForRun(_ context.Context, runID int64) ([]*model.Job, error) {
	var out []*model.Job
	for _, j := range f.jobs {
		if j.RunID == runID {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

func (f *fakeStore) ListJobsInConcurrencyGroup(_ context.Context, group string) ([]*model.Job, error) {
	var out []*model.Job
	for _, j := range f.jobs {
		if j.ConcurrencyGroup == group && j.Status != model.StatusCompleted {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

// --- Steps ---

func (f *fakeStore) UpsertStep(_ context.Context, s *model.Step) error {
	f.steps[s.JobID] = append(f.steps[s.JobID], s)
	return nil
}

func (f *fakeStore) ListSteps(_ context.Context, jobID int64, attempt int) ([]*model.Step, error) {
	var out []*model.Step
	for _, s := range f.steps[jobID] {
		if s.Attempt == attempt {
			out = append(out, s)
		}
	}
	return out, nil
}

// --- Queue ---

func (f *fakeStore) Enqueue(_ context.Context, q store.QueuedJob) error {
	if f.failEnqueue {
		return errors.New("fakeStore: enqueue failed")
	}
	for i := range f.queue {
		if f.queue[i].JobID == q.JobID {
			f.queue[i] = q
			return nil
		}
	}
	f.queue = append(f.queue, q)
	return nil
}

func (f *fakeStore) Dequeue(_ context.Context, runnerID string, labels []string, ttl time.Duration) (*model.Job, error) {
	for i, q := range f.queue {
		j := f.jobs[q.JobID]
		if j == nil {
			continue
		}
		if !hasAll(labels, j.Labels) {
			continue
		}
		f.queue = append(f.queue[:i], f.queue[i+1:]...)
		return j, nil
	}
	return nil, store.ErrNotFound
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (f *fakeStore) Heartbeat(context.Context, string, int64, time.Duration) error { return errUnused }

func (f *fakeStore) ReleaseLease(_ context.Context, _ string, jobID int64, _ model.CancelReason) error {
	j, ok := f.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	j.RunnerID = ""
	j.LeaseExpiresAt = nil
	return nil
}

func (f *fakeStore) ReapExpiredLeases(_ context.Context, _ time.Time) ([]*model.Job, error) {
	out := f.reap
	f.reap = nil
	return out, nil
}

func (f *fakeStore) QueueStats(context.Context, time.Time) (*store.QueueStats, error) {
	return nil, errUnused
}
func (f *fakeStore) QueueDepthHistory(context.Context, time.Time) ([]store.QueueSample, error) {
	return nil, errUnused
}
func (f *fakeStore) RecordQueueSample(context.Context, store.QueueSample) error { return errUnused }

// --- Runners ---

func (f *fakeStore) RegisterRunner(context.Context, *model.Runner) error      { return errUnused }
func (f *fakeStore) RunnerHeartbeat(context.Context, string, time.Time) error { return errUnused }
func (f *fakeStore) GetRunner(context.Context, string) (*model.Runner, error) {
	return nil, errUnused
}
func (f *fakeStore) ListRunners(context.Context) ([]*model.Runner, error) { return nil, errUnused }
func (f *fakeStore) MarkOfflineRunners(context.Context, time.Time) ([]*model.Runner, error) {
	return nil, errUnused
}

// --- Annotations, artifacts, caches ---

func (f *fakeStore) AddAnnotations(context.Context, int64, []model.Annotation) error {
	return errUnused
}
func (f *fakeStore) ListAnnotations(context.Context, int64) ([]model.Annotation, error) {
	return nil, errUnused
}
func (f *fakeStore) CreateArtifact(context.Context, *model.Artifact) error { return errUnused }
func (f *fakeStore) FinalizeArtifact(context.Context, int64, int64, string) error {
	return errUnused
}
func (f *fakeStore) GetArtifact(context.Context, int64) (*model.Artifact, error) {
	return nil, errUnused
}
func (f *fakeStore) FindArtifact(context.Context, int64, string) (*model.Artifact, error) {
	return nil, errUnused
}
func (f *fakeStore) ListArtifacts(context.Context, int64) ([]*model.Artifact, error) {
	return nil, errUnused
}
func (f *fakeStore) DeleteExpiredArtifacts(context.Context, time.Time) ([]*model.Artifact, error) {
	return nil, errUnused
}
func (f *fakeStore) ReserveCache(context.Context, *model.CacheEntry) error { return errUnused }
func (f *fakeStore) FinalizeCache(context.Context, int64, int64) error     { return errUnused }
func (f *fakeStore) ArtifactUsage(context.Context, int64) (int64, error)   { return 0, nil }

func (f *fakeStore) ListCacheEntries(context.Context, int64) ([]*model.CacheEntry, error) {
	return nil, nil
}

func (f *fakeStore) GetCache(context.Context, int64) (*model.CacheEntry, error) {
	return nil, errUnused
}
func (f *fakeStore) LookupCache(context.Context, int64, string, []string, string, string) (*model.CacheEntry, string, error) {
	return nil, "", errUnused
}
func (f *fakeStore) TouchCache(context.Context, int64, time.Time) error       { return errUnused }
func (f *fakeStore) RecordCacheEvent(context.Context, model.CacheEvent) error { return errUnused }
func (f *fakeStore) ListCacheEvents(context.Context, int64, int) ([]model.CacheEvent, error) {
	return nil, errUnused
}
func (f *fakeStore) EvictCaches(context.Context, int64, int64, time.Time) ([]*model.CacheEntry, error) {
	return nil, errUnused
}
func (f *fakeStore) CacheUsage(context.Context, int64) (int64, error) { return 0, errUnused }

// --- Secrets ---

func (f *fakeStore) PutSecret(_ context.Context, _, _, name string, ciphertext []byte) error {
	f.secrets[name] = ciphertext
	return nil
}

func (f *fakeStore) ResolveSecrets(_ context.Context, _, _, _ string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for k, v := range f.secrets {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) DeleteSecret(context.Context, string, string, string) error { return errUnused }
func (f *fakeStore) ListSecretNames(context.Context, string, string) ([]string, error) {
	return nil, errUnused
}
func (f *fakeStore) PutVar(context.Context, string, string, string, string) error { return errUnused }
func (f *fakeStore) ResolveVars(context.Context, string, string, string) (map[string]string, error) {
	return nil, errUnused
}
func (f *fakeStore) DeleteVar(context.Context, string, string, string) error { return errUnused }

// --- Events ---

func (f *fakeStore) RecordEvent(_ context.Context, e store.Event) error {
	if e.Kind == "" {
		return fmt.Errorf("fakeStore: event without a kind")
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) ListEvents(_ context.Context, runID, jobID int64) ([]store.Event, error) {
	var out []store.Event
	for _, e := range f.events {
		if (runID == 0 || e.RunID == runID) && (jobID == 0 || e.JobID == jobID) {
			out = append(out, e)
		}
	}
	return out, nil
}

// --- Store ---

func (f *fakeStore) Durable() bool                 { return false }
func (f *fakeStore) Migrate(context.Context) error { return nil }
func (f *fakeStore) Close() error                  { return nil }

var _ store.Store = (*fakeStore)(nil)
