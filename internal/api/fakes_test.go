package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

var errUnused = errors.New("fake store: method not used by the API")

// fakeStore is an in-memory store.Store good enough to exercise every API
// handler. Methods the API never calls return errUnused so a future handler
// silently depending on one fails loudly instead of seeing a zero value.
type fakeStore struct {
	mu sync.Mutex

	durable  bool
	listErr  error // ListRepos failure, for the /healthz down path
	repos    []*model.Repo
	runs     []*model.Run
	jobs     []*model.Job
	steps    map[int64][]*model.Step
	anns     map[int64][]model.Annotation
	events   []store.Event
	runners  []*model.Runner
	arts     []*model.Artifact
	cacheEvs map[int64][]model.CacheEvent
	usage    map[int64]int64
	qstats   *store.QueueStats
	qhist    []store.QueueSample
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		durable:  true,
		steps:    map[int64][]*model.Step{},
		anns:     map[int64][]model.Annotation{},
		cacheEvs: map[int64][]model.CacheEvent{},
		usage:    map[int64]int64{},
	}
}

func (f *fakeStore) Durable() bool                     { return f.durable }
func (f *fakeStore) Migrate(context.Context) error     { return nil }
func (f *fakeStore) Close() error                      { return nil }

func (f *fakeStore) UpsertRepo(context.Context, *model.Repo) error { return errUnused }

func (f *fakeStore) GetRepo(_ context.Context, id int64) (*model.Repo, error) {
	for _, r := range f.repos {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetRepoByName(_ context.Context, owner, name string) (*model.Repo, error) {
	for _, r := range f.repos {
		if r.Owner == owner && r.Name == name {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListRepos(context.Context) ([]*model.Repo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.repos, nil
}

func (f *fakeStore) CreateRun(context.Context, *model.Run) error { return errUnused }
func (f *fakeStore) UpdateRun(context.Context, *model.Run) error { return errUnused }

func (f *fakeStore) GetRun(_ context.Context, id int64) (*model.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) matches(r *model.Run, fl store.RunFilter) bool {
	switch {
	case fl.RepoID != 0 && r.RepoID != fl.RepoID:
		return false
	case fl.Branch != "" && r.HeadBranch != fl.Branch:
		return false
	case fl.Actor != "" && r.Actor != fl.Actor:
		return false
	case fl.Event != "" && r.Event != fl.Event:
		return false
	case fl.Status != "" && r.Status != fl.Status:
		return false
	case fl.Conclusion != "" && r.Conclusion != fl.Conclusion:
		return false
	case fl.Workflow != "" && r.WorkflowName != fl.Workflow && r.WorkflowPath != fl.Workflow:
		return false
	case fl.Search != "" && !strings.Contains(strings.ToLower(r.WorkflowName+" "+r.HeadBranch+" "+r.HeadSHA), strings.ToLower(fl.Search)):
		return false
	}
	return true
}

func (f *fakeStore) ListRuns(_ context.Context, fl store.RunFilter) ([]*model.Run, error) {
	var out []*model.Run
	for _, r := range f.runs {
		if f.matches(r, fl) {
			out = append(out, r)
		}
	}
	if fl.Offset >= len(out) {
		return nil, nil
	}
	out = out[fl.Offset:]
	if fl.Limit > 0 && len(out) > fl.Limit {
		out = out[:fl.Limit]
	}
	return out, nil
}

func (f *fakeStore) CountRuns(_ context.Context, fl store.RunFilter) (int, error) {
	n := 0
	for _, r := range f.runs {
		if f.matches(r, fl) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) NextRunNumber(context.Context, int64, string) (int64, error) { return 0, errUnused }
func (f *fakeStore) ListRunsForSHA(context.Context, int64, string) ([]*model.Run, error) {
	return nil, errUnused
}

func (f *fakeStore) CreateJob(context.Context, *model.Job) error { return errUnused }
func (f *fakeStore) UpdateJob(context.Context, *model.Job) error { return errUnused }

func (f *fakeStore) GetJob(_ context.Context, id int64) (*model.Job, error) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListJobsForRun(_ context.Context, runID int64) ([]*model.Job, error) {
	var out []*model.Job
	for _, j := range f.jobs {
		if j.RunID == runID {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeStore) ListJobsInConcurrencyGroup(context.Context, string) ([]*model.Job, error) {
	return nil, errUnused
}

func (f *fakeStore) UpsertStep(context.Context, *model.Step) error { return errUnused }

func (f *fakeStore) ListSteps(_ context.Context, jobID int64, attempt int) ([]*model.Step, error) {
	var out []*model.Step
	for _, s := range f.steps[jobID] {
		if s.Attempt == attempt {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (f *fakeStore) Enqueue(context.Context, store.QueuedJob) error { return errUnused }
func (f *fakeStore) Dequeue(context.Context, string, []string, time.Duration) (*model.Job, error) {
	return nil, errUnused
}
func (f *fakeStore) Heartbeat(context.Context, string, int64, time.Duration) error { return errUnused }
func (f *fakeStore) ReleaseLease(context.Context, string, int64, model.CancelReason) error {
	return errUnused
}
func (f *fakeStore) ReapExpiredLeases(context.Context, time.Time) ([]*model.Job, error) {
	return nil, errUnused
}

func (f *fakeStore) QueueStats(context.Context, time.Time) (*store.QueueStats, error) {
	if f.qstats == nil {
		return nil, errors.New("no queue stats configured")
	}
	return f.qstats, nil
}

func (f *fakeStore) QueueDepthHistory(_ context.Context, since time.Time) ([]store.QueueSample, error) {
	var out []store.QueueSample
	for _, s := range f.qhist {
		if !s.At.Before(since) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) RecordQueueSample(context.Context, store.QueueSample) error { return errUnused }

func (f *fakeStore) RegisterRunner(context.Context, *model.Runner) error   { return errUnused }
func (f *fakeStore) RunnerHeartbeat(context.Context, string, time.Time) error { return errUnused }
func (f *fakeStore) GetRunner(context.Context, string) (*model.Runner, error) { return nil, errUnused }
func (f *fakeStore) ListRunners(context.Context) ([]*model.Runner, error)     { return f.runners, nil }
func (f *fakeStore) MarkOfflineRunners(context.Context, time.Time) ([]*model.Runner, error) {
	return nil, errUnused
}

func (f *fakeStore) AddAnnotations(context.Context, int64, []model.Annotation) error { return errUnused }
func (f *fakeStore) ListAnnotations(_ context.Context, jobID int64) ([]model.Annotation, error) {
	return f.anns[jobID], nil
}

func (f *fakeStore) CreateArtifact(context.Context, *model.Artifact) error       { return errUnused }
func (f *fakeStore) FinalizeArtifact(context.Context, int64, int64, string) error { return errUnused }

func (f *fakeStore) GetArtifact(_ context.Context, id int64) (*model.Artifact, error) {
	for _, a := range f.arts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) FindArtifact(context.Context, int64, string) (*model.Artifact, error) {
	return nil, errUnused
}

func (f *fakeStore) ListArtifacts(_ context.Context, runID int64) ([]*model.Artifact, error) {
	var out []*model.Artifact
	for _, a := range f.arts {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteExpiredArtifacts(context.Context, time.Time) ([]*model.Artifact, error) {
	return nil, errUnused
}

func (f *fakeStore) ReserveCache(context.Context, *model.CacheEntry) error { return errUnused }
func (f *fakeStore) FinalizeCache(context.Context, int64, int64) error     { return errUnused }
func (f *fakeStore) LookupCache(context.Context, int64, string, []string, string, string) (*model.CacheEntry, string, error) {
	return nil, "", errUnused
}
func (f *fakeStore) GetCache(context.Context, int64) (*model.CacheEntry, error) { return nil, errUnused }
func (f *fakeStore) TouchCache(context.Context, int64, time.Time) error         { return errUnused }
func (f *fakeStore) RecordCacheEvent(context.Context, model.CacheEvent) error   { return errUnused }

func (f *fakeStore) ListCacheEvents(_ context.Context, repoID int64, limit int) ([]model.CacheEvent, error) {
	out := f.cacheEvs[repoID]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) EvictCaches(context.Context, int64, int64, time.Time) ([]*model.CacheEntry, error) {
	return nil, errUnused
}
func (f *fakeStore) CacheUsage(_ context.Context, repoID int64) (int64, error) {
	return f.usage[repoID], nil
}

func (f *fakeStore) PutSecret(context.Context, string, string, string, []byte) error { return errUnused }
func (f *fakeStore) ResolveSecrets(context.Context, string, string, string) (map[string][]byte, error) {
	return nil, errUnused
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

func (f *fakeStore) RecordEvent(context.Context, store.Event) error { return errUnused }

func (f *fakeStore) ListEvents(_ context.Context, runID, jobID int64) ([]store.Event, error) {
	var out []store.Event
	for _, e := range f.events {
		if runID != 0 && e.RunID == runID {
			out = append(out, e)
			continue
		}
		if jobID != 0 && e.JobID == jobID {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakeController records what the API asked for.
type fakeController struct {
	mu       sync.Mutex
	cancels  []model.CancelReason
	jobIDs   []int64
	runIDs   []int64
	actions  []string
	actors   []string
	err      error
}

func (c *fakeController) record(action string, runID, jobID int64, actor string, reason *model.CancelReason) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, action)
	c.runIDs = append(c.runIDs, runID)
	c.jobIDs = append(c.jobIDs, jobID)
	c.actors = append(c.actors, actor)
	if reason != nil {
		c.cancels = append(c.cancels, *reason)
	}
	return c.err
}

func (c *fakeController) Cancel(_ context.Context, runID int64, r model.CancelReason) error {
	return c.record("cancel-run", runID, 0, r.TriggeredBy, &r)
}
func (c *fakeController) CancelJob(_ context.Context, jobID int64, r model.CancelReason) error {
	return c.record("cancel-job", 0, jobID, r.TriggeredBy, &r)
}
func (c *fakeController) Rerun(_ context.Context, runID int64, actor string) error {
	return c.record("rerun", runID, 0, actor, nil)
}
func (c *fakeController) RerunFailed(_ context.Context, runID int64, actor string) error {
	return c.record("rerun-failed", runID, 0, actor, nil)
}
func (c *fakeController) RerunJob(_ context.Context, jobID int64, actor string) error {
	return c.record("rerun-job", 0, jobID, actor, nil)
}

// fakeLogs serves a fixed slice and can hold a subscription open so the SSE
// test can drive heartbeats and resume.
type fakeLogs struct {
	mu    sync.Mutex
	lines []model.LogLine
	live  chan model.LogLine
	err   error
}

func (l *fakeLogs) Read(_ context.Context, _ int64, _ int, fromSeq int64, limit int) ([]model.LogLine, error) {
	if l.err != nil {
		return nil, l.err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []model.LogLine
	for _, ln := range l.lines {
		if ln.Seq >= fromSeq {
			out = append(out, ln)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (l *fakeLogs) Subscribe(_ context.Context, _ int64, _ int, fromSeq int64) (<-chan model.LogLine, error) {
	if l.err != nil {
		return nil, l.err
	}
	ch := make(chan model.LogLine, 64)
	l.mu.Lock()
	for _, ln := range l.lines {
		if ln.Seq >= fromSeq {
			ch <- ln
		}
	}
	live := l.live
	l.mu.Unlock()
	if live == nil {
		close(ch)
		return ch, nil
	}
	go func() {
		defer close(ch)
		for ln := range live {
			ch <- ln
		}
	}()
	return ch, nil
}

// fakeBlobs hands back fixed bytes.
type fakeBlobs struct {
	data []byte
	err  error
}

func (b *fakeBlobs) Open(context.Context, *model.Artifact) (io.ReadCloser, error) {
	if b.err != nil {
		return nil, b.err
	}
	return io.NopCloser(bytes.NewReader(b.data)), nil
}
