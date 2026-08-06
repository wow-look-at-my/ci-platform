package runnerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
)

const testToken = "runner-token"

type fakeScheduler struct {
	assignment *protocol.Assignment
	acquires   int
	completed  []SchedulerResult
	setupAt    []time.Time
	released   []model.CancelReason
	acquireErr error
}

func (f *fakeScheduler) Acquire(ctx context.Context, runnerID string, labels []string, now time.Time) (*protocol.Assignment, error) {
	f.acquires++
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	a := f.assignment
	f.assignment = nil
	return a, nil
}

func (f *fakeScheduler) JobCompleted(ctx context.Context, jobID int64, res SchedulerResult) error {
	f.completed = append(f.completed, res)
	return nil
}

func (f *fakeScheduler) JobSetupCompleted(ctx context.Context, jobID int64, at time.Time) error {
	f.setupAt = append(f.setupAt, at)
	return nil
}

func (f *fakeScheduler) ReleaseJob(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	f.released = append(f.released, reason)
	return nil
}

type fakeLogs struct {
	lines     []model.LogLine
	finalized int
}

func (f *fakeLogs) Append(ctx context.Context, jobID int64, attempt int, lines []model.LogLine) (int64, error) {
	f.lines = append(f.lines, lines...)
	return int64(len(f.lines)), nil
}

func (f *fakeLogs) Finalize(ctx context.Context, jobID int64, attempt int) error {
	f.finalized++
	return nil
}

type harness struct {
	srv   *Server
	sched *fakeScheduler
	logs  *fakeLogs
	st    store.Store
	http  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := mem.New()
	sched := &fakeScheduler{}
	logs := &fakeLogs{}
	srv, err := New(Options{
		Store: st, Scheduler: sched, Logs: logs, Token: testToken,
		LeaseTTL: time.Minute, HeartbeatInterval: 5 * time.Second,
		AcquireWait: 200 * time.Millisecond, PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	h := &harness{srv: srv, sched: sched, logs: logs, st: st, http: httptest.NewServer(srv)}
	t.Cleanup(h.http.Close)
	return h
}

func (h *harness) post(t *testing.T, path string, body any, out any) int {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, h.http.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// An unauthenticated runner endpoint would let anything on the network claim a
// job and read its secrets, so construction refuses without a token.
func TestNew_RequiresItsDependencies(t *testing.T) {
	_, err := New(Options{})
	require.ErrorContains(t, err, "Store is required")

	_, err = New(Options{Store: mem.New()})
	require.ErrorContains(t, err, "Scheduler is required")

	_, err = New(Options{Store: mem.New(), Scheduler: &fakeScheduler{}})
	require.ErrorContains(t, err, "Logs is required")

	_, err = New(Options{Store: mem.New(), Scheduler: &fakeScheduler{}, Logs: &fakeLogs{}})
	require.ErrorContains(t, err, "Token is required")

	// A heartbeat slower than the lease requeues every running job forever.
	_, err = New(Options{
		Store: mem.New(), Scheduler: &fakeScheduler{}, Logs: &fakeLogs{}, Token: "t",
		LeaseTTL: 10 * time.Second, HeartbeatInterval: 30 * time.Second,
	})
	require.ErrorContains(t, err, "must be shorter than lease TTL")
}

func TestAuth(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodPost, h.http.URL+protocol.PathRegister, bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req2, _ := http.NewRequest(http.MethodPost, h.http.URL+protocol.PathRegister, bytes.NewReader([]byte(`{}`)))
	req2.Header.Set("Authorization", "Bearer wrong-token")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

// A runner speaking another version's payloads is refused rather than
// half-understood.
func TestRegister_RejectsAnUnknownProtocolVersion(t *testing.T) {
	h := newHarness(t)
	code := h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: "999", RunnerID: "r1",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, code)

	code = h.post(t, protocol.PathRegister, protocol.RegisterRequest{APIVersion: protocol.APIVersion}, nil)
	assert.Equal(t, http.StatusBadRequest, code, "runner_id is required")
}

func TestRegister_RecordsTheRunnerAndReturnsLeaseTerms(t *testing.T) {
	h := newHarness(t)
	var out protocol.RegisterResponse
	code := h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "r1", Name: "host-1",
		Labels: []string{"linux", "x64"}, Capacity: 2,
	}, &out)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, time.Minute, out.LeaseTTL.D())
	assert.Equal(t, 5*time.Second, out.HeartbeatInterval.D())

	rn, err := h.st.GetRunner(context.Background(), "r1")
	require.NoError(t, err)
	assert.Equal(t, []string{"linux", "x64"}, rn.Labels)
	assert.Equal(t, model.RunnerIdle, rn.State)
}

// An idle poll is a normal outcome, not an error: the agent loops quietly.
func TestAcquire_EmptyPollIsNotAnError(t *testing.T) {
	h := newHarness(t)
	var out protocol.AcquireResponse
	code := h.post(t, protocol.PathAcquire, protocol.AcquireRequest{
		RunnerID: "r1", Labels: []string{"linux"}, Wait: protocol.Duration(100 * time.Millisecond),
	}, &out)
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, out.Assignment)
	assert.Greater(t, h.sched.acquires, 1, "the long poll re-checks rather than returning immediately")
}

func TestAcquire_DeliversAnAssignment(t *testing.T) {
	h := newHarness(t)
	h.sched.assignment = &protocol.Assignment{
		RunID: 1, JobID: 2, Attempt: 1, IdempotencyKey: "1/2/1", JobName: "build (linux)",
	}
	var out protocol.AcquireResponse
	code := h.post(t, protocol.PathAcquire, protocol.AcquireRequest{RunnerID: "r1"}, &out)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, out.Assignment)
	assert.Equal(t, "1/2/1", out.Assignment.IdempotencyKey)
}

func TestAcquire_SchedulerErrorSurfaces(t *testing.T) {
	h := newHarness(t)
	h.sched.acquireErr = errors.New("boom")
	code := h.post(t, protocol.PathAcquire, protocol.AcquireRequest{RunnerID: "r1"}, nil)
	assert.Equal(t, http.StatusInternalServerError, code)
}

// The heartbeat is the only channel by which a cancellation reaches a running
// job, and it always carries the reason.
func TestHeartbeat_DeliversAQueuedCancellationWithItsReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	repo, run, job := seedQueuedJob(t, h.st)
	_ = repo
	_ = run
	_, err := h.st.Dequeue(ctx, "r1", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	reason := model.CancelReason{
		Actor:    model.CancelActorUser,
		Sentence: "Alex cancelled this run from the web UI.",
	}
	require.NoError(t, h.srv.RequestCancel(job.ID, reason))

	var out protocol.HeartbeatResponse
	code := h.post(t, protocol.PathHeartbeat, protocol.HeartbeatRequest{
		RunnerID: "r1", JobID: job.ID, Attempt: 1, Phase: "execute",
	}, &out)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, out.Cancel)
	assert.Equal(t, model.CancelActorUser, out.Cancel.Actor)
	assert.Equal(t, reason.Sentence, out.Cancel.Sentence)
	require.NoError(t, out.Cancel.Validate())

	// Delivered once, not repeatedly.
	var again protocol.HeartbeatResponse
	h.post(t, protocol.PathHeartbeat, protocol.HeartbeatRequest{RunnerID: "r1", JobID: job.ID}, &again)
	assert.Nil(t, again.Cancel)
}

func TestRequestCancel_RefusesAnUnexplainedCancellation(t *testing.T) {
	h := newHarness(t)
	require.Error(t, h.srv.RequestCancel(1, model.CancelReason{Actor: model.CancelActorUser}))
	require.Error(t, h.srv.RequestCancel(1, model.CancelReason{Sentence: "x"}))
}

// A runner whose lease was taken must stop without reporting a result, or two
// runners both complete the same job.
func TestHeartbeat_ReportsALostLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)
	_, err := h.st.Dequeue(ctx, "r1", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	var out protocol.HeartbeatResponse
	code := h.post(t, protocol.PathHeartbeat, protocol.HeartbeatRequest{
		RunnerID: "someone-else", JobID: job.ID,
	}, &out)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, out.LeaseLost)
	assert.Nil(t, out.Cancel)
}

func TestLogsAppend(t *testing.T) {
	h := newHarness(t)
	var out map[string]int64
	code := h.post(t, protocol.PathLogs, protocol.LogBatch{
		JobID: 1, Attempt: 1,
		Lines: []model.LogLine{{Seq: 1, Text: "hello"}, {Seq: 2, Text: "world"}},
	}, &out)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, int64(2), out["last_seq"])
	assert.Len(t, h.logs.lines, 2)

	code = h.post(t, protocol.PathLogs, protocol.LogBatch{JobID: 1, Attempt: 1}, &out)
	assert.Equal(t, http.StatusOK, code, "an empty batch is a no-op")
}

func TestStepLifecycleRecordsTheClassification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)

	code := h.post(t, protocol.PathStepStart, protocol.StepStartRequest{
		JobID: job.ID, Attempt: 1, Number: 1, Name: "build", LogStart: 0,
	}, nil)
	require.Equal(t, http.StatusOK, code)

	code = h.post(t, protocol.PathStepEnd, protocol.StepEndRequest{
		JobID: job.ID, Attempt: 1, Number: 1,
		Conclusion:  model.ConclusionInfraFailure,
		Class:       model.ClassInfra,
		ClassReason: `classified infra via rule "cloudflare-524": the remote returned HTTP 524`,
		ExitCode:    1, LogEnd: 42,
	}, nil)
	require.Equal(t, http.StatusOK, code)

	steps, err := h.st.ListSteps(ctx, job.ID, 1)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, model.ConclusionInfraFailure, steps[0].Conclusion)
	assert.Equal(t, int64(42), steps[0].LogEnd)

	// The decision is recorded so an operator can see WHY it was called infra.
	events, err := h.st.ListEvents(ctx, 0, job.ID)
	require.NoError(t, err)
	found := false
	for _, e := range events {
		if e.Kind == "classified" {
			found = true
			assert.Contains(t, e.Message, "cloudflare-524")
		}
	}
	assert.True(t, found, "the classification must be recorded as an event")
}

// Setup timing is measured and recorded, not inferred from adjacent timestamps.
func TestSetupPhaseIsRecordedWithItsBreakdown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)

	require.Equal(t, http.StatusOK, h.post(t, protocol.PathSetup, protocol.SetupRequest{
		JobID: job.ID, Attempt: 1, Phase: "started",
	}, nil))

	require.Equal(t, http.StatusOK, h.post(t, protocol.PathSetup, protocol.SetupRequest{
		JobID: job.ID, Attempt: 1, Phase: "completed",
		Breakdown: map[string]protocol.Duration{
			"container_create": protocol.Duration(2 * time.Second),
			"image_pull":       protocol.Duration(5 * time.Minute),
		},
		CacheWarm: false,
	}, nil))

	require.Len(t, h.sched.setupAt, 1)
	events, err := h.st.ListEvents(ctx, 0, job.ID)
	require.NoError(t, err)
	var msg string
	for _, e := range events {
		if e.Kind == "setup_completed" {
			msg = e.Message
			assert.Equal(t, "5m0s", e.Detail["image_pull"])
			assert.Equal(t, false, e.Detail["cache_warm"])
		}
	}
	assert.Contains(t, msg, "cold image cache")
	assert.Contains(t, msg, "5m2s")
}

func TestComplete_RequiresAReasonWhenCancelled(t *testing.T) {
	h := newHarness(t)
	code := h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		JobID: 1, Attempt: 1, Conclusion: model.ConclusionCancelled,
	}, nil)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, h.sched.completed)

	// A malformed reason is refused too.
	code = h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		JobID: 1, Attempt: 1, Conclusion: model.ConclusionCancelled,
		Cancel: &model.CancelReason{Actor: model.CancelActorUser},
	}, nil)
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestComplete_ForwardsTheClassification(t *testing.T) {
	h := newHarness(t)
	_, _, job := seedQueuedJob(t, h.st)

	var out protocol.CompleteResponse
	code := h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		JobID: job.ID, Attempt: 1,
		Conclusion:        model.ConclusionInfraFailure,
		Class:             model.ClassInfra,
		ClassReason:       "registry responded 524",
		ClassificationLog: []string{"step 2: infra"},
	}, &out)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, h.sched.completed, 1)
	assert.Equal(t, model.ClassInfra, h.sched.completed[0].Class)
	assert.Equal(t, "registry responded 524", h.sched.completed[0].ClassReason)
	assert.Equal(t, 1, h.logs.finalized)
}

// A job reappearing in the queue must say why it did.
func TestRelease_RequiresAReason(t *testing.T) {
	h := newHarness(t)
	code := h.post(t, protocol.PathRelease, protocol.ReleaseRequest{JobID: 1, RunnerID: "r1"}, nil)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, h.sched.released)

	code = h.post(t, protocol.PathRelease, protocol.ReleaseRequest{
		JobID: 1, RunnerID: "r1",
		Reason: model.CancelReason{
			Actor:    model.CancelActorShutdown,
			Sentence: "The runner agent received SIGTERM and released this job back to the queue.",
		},
	}, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, h.sched.released, 1)
	assert.Equal(t, model.CancelActorShutdown, h.sched.released[0].Actor)
}

func TestAnnotate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)

	code := h.post(t, protocol.PathAnnotate, protocol.AnnotateRequest{
		JobID: job.ID, Attempt: 1,
		Annotations: []model.Annotation{{
			Path: "main.go", StartLine: 3, EndLine: 3,
			Level: model.AnnotationFailure, Message: "undefined: foo",
		}},
	}, nil)
	require.Equal(t, http.StatusOK, code)

	as, err := h.st.ListAnnotations(ctx, job.ID)
	require.NoError(t, err)
	require.Len(t, as, 1)
	assert.Equal(t, "main.go", as[0].Path)

	code = h.post(t, protocol.PathAnnotate, protocol.AnnotateRequest{JobID: job.ID}, nil)
	assert.Equal(t, http.StatusOK, code, "an empty batch is a no-op")
}

func TestMalformedBodyIsRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodPost, h.http.URL+protocol.PathRegister, bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSubtleCompare(t *testing.T) {
	assert.Equal(t, 1, subtleCompare("abc", "abc"))
	assert.Equal(t, 0, subtleCompare("abc", "abd"))
	assert.Equal(t, 0, subtleCompare("abc", "abcd"))
}

func seedQueuedJob(t *testing.T, st store.Store) (*model.Repo, *model.Run, *model.Job) {
	t.Helper()
	ctx := context.Background()
	// Repo ids come from GitHub, so the store requires the caller to supply one.
	repo := &model.Repo{ID: 4242, Owner: "acme", Name: "widget", DefaultBranch: "main"}
	require.NoError(t, st.UpsertRepo(ctx, repo))

	run := &model.Run{RepoID: repo.ID, WorkflowPath: ".github/workflows/ci.yml", Attempt: 1,
		Status: model.StatusQueued, HeadSHA: "abc", CreatedAt: time.Now().UTC()}
	require.NoError(t, st.CreateRun(ctx, run))

	job := &model.Job{RunID: run.ID, Key: "build", Name: "build", Attempt: 1, MaxAttempts: 3,
		Labels: []string{"linux"}, Status: model.StatusQueued, CreatedAt: time.Now().UTC()}
	require.NoError(t, st.CreateJob(ctx, job))
	require.NoError(t, st.Enqueue(ctx, store.QueuedJob{
		JobID: job.ID, RunID: run.ID, Attempt: job.Attempt,
		Labels: []string{"linux"}, QueuedAt: time.Now().UTC(),
	}))
	return repo, run, job
}
