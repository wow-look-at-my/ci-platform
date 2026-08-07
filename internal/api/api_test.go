package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func ptime(t time.Time) *time.Time { return &t }

// fixture builds a store with one repo, three runs, and a three-job DAG on the
// first run: build -> test -> deploy, where test failed for infra reasons.
func fixture() *fakeStore {
	f := newFakeStore()
	f.repos = []*model.Repo{{ID: 7, Owner: "wow-look-at-my", Name: "ci-platform", DefaultBranch: "master"}}
	f.usage[7] = 4096
	base := testNow.Add(-10 * time.Minute)

	f.runs = []*model.Run{
		{
			ID: 100, RepoID: 7, RepoFull: "wow-look-at-my/ci-platform",
			WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			RunNumber: 42, Attempt: 2, Event: "push",
			HeadSHA: "deadbeef", HeadBranch: "master", Actor: "pazer",
			Status: model.StatusCompleted, Conclusion: model.ConclusionInfraFailure,
			CreatedAt: base, StartedAt: ptime(base.Add(30 * time.Second)), CompletedAt: ptime(base.Add(5 * time.Minute)),
		},
		{
			ID: 101, RepoID: 7, RepoFull: "wow-look-at-my/ci-platform",
			WorkflowName: "Release", WorkflowPath: ".github/workflows/release.yml",
			RunNumber: 9, Attempt: 1, Event: "workflow_dispatch",
			HeadSHA: "cafebabe", HeadBranch: "feature/x", Actor: "someone",
			Status:    model.StatusInProgress,
			CreatedAt: base.Add(time.Minute), StartedAt: ptime(base.Add(2 * time.Minute)),
		},
		{
			ID: 102, RepoID: 7, RepoFull: "wow-look-at-my/ci-platform",
			WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
			RunNumber: 43, Attempt: 1, Event: "pull_request",
			HeadSHA: "0badcafe", HeadBranch: "feature/x", Actor: "pazer",
			Status: model.StatusQueued, CreatedAt: base.Add(3 * time.Minute),
		},
	}

	f.jobs = []*model.Job{
		{
			ID: 200, RunID: 100, Key: "build", Name: "build", Labels: []string{"linux"},
			Attempt: 1, MaxAttempts: 3, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
			CreatedAt: base, QueuedAt: ptime(base), StartedAt: ptime(base.Add(10 * time.Second)),
			SetupCompletedAt: ptime(base.Add(20 * time.Second)), CompletedAt: ptime(base.Add(80 * time.Second)),
		},
		{
			ID: 201, RunID: 100, Key: "test", Name: "test (ubuntu)", Needs: []string{"build"},
			Labels: []string{"linux"}, Attempt: 2, MaxAttempts: 3, RequeueCount: 1, InfraRetryCount: 1,
			Status: model.StatusCompleted, Conclusion: model.ConclusionInfraFailure, Class: model.ClassInfra,
			FailureExplained:  "The registry returned HTTP 524 while pulling the base image; this is not your code.",
			ClassificationLog: []string{"classified infra via rule \"registry-5xx\""},
			RunnerID:          "runner-a",
			CreatedAt:         base, QueuedAt: ptime(base.Add(80 * time.Second)),
			StartedAt: ptime(base.Add(90 * time.Second)), SetupCompletedAt: ptime(base.Add(100 * time.Second)),
			CompletedAt: ptime(base.Add(200 * time.Second)),
		},
		{
			ID: 202, RunID: 100, Key: "deploy", Name: "deploy", Needs: []string{"test", "build"},
			Labels: []string{"linux"}, Attempt: 1, MaxAttempts: 1,
			Status: model.StatusCompleted, Conclusion: model.ConclusionSkipped, CreatedAt: base,
		},
	}

	f.steps[201] = []*model.Step{
		{ID: 1, JobID: 201, Number: 1, Name: "Set up job", Attempt: 2, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess, LogStart: 1, LogEnd: 5},
		{ID: 2, JobID: 201, Number: 2, Name: "Run tests", Attempt: 2, Status: model.StatusCompleted, Conclusion: model.ConclusionInfraFailure, Class: model.ClassInfra, ExitCode: 1, LogStart: 6, LogEnd: 12},
	}
	f.anns[201] = []model.Annotation{{ID: 1, JobID: 201, Path: "main.go", StartLine: 4, EndLine: 4, Level: model.AnnotationFailure, Message: "boom"}}
	f.events = []store.Event{
		{ID: 1, RunID: 100, Kind: "run.created", Message: "run created", At: base},
		{ID: 2, JobID: 201, Kind: "job.classified", Message: "classified infra", At: base.Add(200 * time.Second)},
	}

	f.runners = []*model.Runner{
		{ID: "runner-a", Name: "agent-1", Labels: []string{"linux", "x64"}, State: model.RunnerBusy, CurrentJobID: 201, Capacity: 2, LastHeartbeat: testNow.Add(-5 * time.Second)},
		{ID: "runner-b", Name: "agent-2", Labels: []string{"linux"}, State: model.RunnerIdle, Capacity: 1, LastHeartbeat: testNow.Add(-2 * time.Minute)},
		{ID: "runner-c", Name: "agent-3", Labels: []string{"macos"}, State: model.RunnerOffline, Capacity: 1, LastHeartbeat: testNow.Add(-1 * time.Hour)},
	}

	f.arts = []*model.Artifact{
		{ID: 300, RunID: 100, JobID: 200, Name: "binary.tar.gz", SizeBytes: 1024, Digest: "sha256:abc", Finalized: true, CreatedAt: base, ExpiresAt: testNow.Add(24 * time.Hour)},
		{ID: 301, RunID: 100, Name: "in-flight.zip", Finalized: false, CreatedAt: base},
	}

	f.cacheEvs[7] = []model.CacheEvent{
		{ID: 1, RepoID: 7, Key: "go-mod", Kind: "store", SizeBytes: 2048, At: base},
		{ID: 2, RepoID: 7, Key: "go-mod", Kind: "hit", MatchedOn: "go-mod", At: base.Add(time.Minute)},
		{ID: 3, RepoID: 7, Key: "go-mod", Kind: "hit", At: base.Add(2 * time.Minute)},
		{ID: 4, RepoID: 7, Key: "npm", Kind: "miss", At: base.Add(3 * time.Minute)},
		{ID: 5, RepoID: 7, Key: "stale", Kind: "store", SizeBytes: 10, At: base},
		{ID: 6, RepoID: 7, Key: "stale", Kind: "evict", Reason: "quota", At: base.Add(4 * time.Minute)},
	}

	// The store lists live entries directly; the evicted "stale" key is simply
	// absent rather than something the reader reconciles against the events.
	f.cacheEntries = []*model.CacheEntry{
		{ID: 1, RepoID: 7, Key: "go-mod", Version: "v1", SizeBytes: 2048,
			CreatedAt: base, LastAccessed: base.Add(2 * time.Minute), Finalized: true},
	}

	f.qstats = &store.QueueStats{
		Depth: 3, DepthByLabel: map[string]int{"linux": 2, "macos": 1},
		OldestWaiting: 90 * time.Second, OldestJobID: 202,
		RunnersByLabel: map[string]int{"linux": 2}, IdleByLabel: map[string]int{"linux": 1},
		StarvedLabels: []string{"macos"}, At: testNow,
	}
	f.qhist = []store.QueueSample{
		{At: testNow.Add(-30 * time.Minute), Depth: 1, Busy: 1, Idle: 1},
		{At: testNow.Add(-5 * time.Minute), Depth: 3, Busy: 2, Idle: 0},
		{At: testNow.Add(-2 * time.Hour), Depth: 9, Busy: 0, Idle: 2},
	}
	return f
}

type harness struct {
	srv  *Server
	st   *fakeStore
	ctrl *fakeController
	logs *fakeLogs
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	st := fixture()
	ctrl := &fakeController{}
	logs := &fakeLogs{}
	cfg := Config{
		Store: st, Controller: ctrl, Logs: logs,
		Now: func() time.Time { return testNow }, SSEHeartbeat: 20 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return &harness{srv: New(cfg), st: st, ctrl: ctrl, logs: logs}
}

func (h *harness) do(t *testing.T, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v), "body: %s", w.Body.String())
	return v
}

func TestListRunsShape(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runs", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	// The exact key names a gh-alike client parses.
	raw := decode[map[string]any](t, w)
	assert.Contains(t, raw, "total_count")
	assert.Contains(t, raw, "workflow_runs")
	runs := raw["workflow_runs"].([]any)
	require.Len(t, runs, 3)
	first := runs[0].(map[string]any)
	for _, k := range []string{
		"id", "name", "workflow_name", "run_number", "run_attempt", "attempt", "status",
		"conclusion", "head_sha", "head_branch", "created_at", "started_at", "completed_at",
		"event", "actor", "failure_class", "timing", "repository",
	} {
		assert.Contains(t, first, k, "workflow_runs[0] is missing %q", k)
	}
	timing := first["timing"].(map[string]any)
	for _, k := range []string{"queued_for", "setup_for", "execute_for", "total_for"} {
		assert.Contains(t, timing, k)
	}
	assert.Equal(t, "infra", first["failure_class"])
	assert.InDelta(t, 30.0, timing["queued_for"], 0.001)
}

func TestListRunsFilters(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		query string
		want  int
	}{
		{"?branch=feature/x", 2},
		{"?actor=pazer", 2},
		{"?event=push", 1},
		{"?status=queued", 1},
		{"?conclusion=infra_failure", 1},
		{"?workflow=CI", 2},
		{"?q=cafebabe", 1},
		{"?repo=wow-look-at-my/ci-platform", 3},
		{"?branch=nope", 0},
	}
	for _, c := range cases {
		w := h.do(t, "GET", "/api/v1/runs"+c.query, "")
		require.Equal(t, http.StatusOK, w.Code, c.query)
		got := decode[RunListDTO](t, w)
		assert.Equal(t, c.want, got.TotalCount, c.query)
		assert.Len(t, got.WorkflowRuns, c.want, c.query)
	}
}

func TestListRunsPaging(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runs?per_page=2&page=1", "")
	got := decode[RunListDTO](t, w)
	assert.Equal(t, 3, got.TotalCount)
	require.Len(t, got.WorkflowRuns, 2)
	assert.Equal(t, int64(100), got.WorkflowRuns[0].ID)

	w = h.do(t, "GET", "/api/v1/runs?per_page=2&page=2", "")
	got = decode[RunListDTO](t, w)
	assert.Equal(t, 3, got.TotalCount)
	require.Len(t, got.WorkflowRuns, 1)
	assert.Equal(t, int64(102), got.WorkflowRuns[0].ID)
	assert.Equal(t, 2, got.Page)
	assert.Equal(t, 2, got.PerPage)
}

func TestListRunsRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"?status=nonsense", "?conclusion=nonsense", "?page=0", "?per_page=9999", "?page=abc", "?repo=notaslash"} {
		w := h.do(t, "GET", "/api/v1/runs"+q, "")
		assert.Equal(t, http.StatusBadRequest, w.Code, q)
		assert.NotEmpty(t, decode[errorBody](t, w).Message, q)
	}
	w := h.do(t, "GET", "/api/v1/runs?repo=nobody/nothing", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetRunIncludesGraphAndAttempts(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runs/100", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[RunDetailDTO](t, w)

	require.Len(t, got.Jobs, 3)
	require.Len(t, got.Graph.Nodes, 3)
	depth := map[string]int{}
	for _, n := range got.Graph.Nodes {
		depth[n.Key] = n.Depth
	}
	assert.Equal(t, 0, depth["build"])
	assert.Equal(t, 1, depth["test"])
	assert.Equal(t, 2, depth["deploy"])
	assert.ElementsMatch(t, []GraphEdge{{From: "build", To: "test"}, {From: "test", To: "deploy"}, {From: "build", To: "deploy"}}, got.Graph.Edges)

	require.Len(t, got.Attempts, 2)
	assert.False(t, got.Attempts[0].Current)
	assert.True(t, got.Attempts[1].Current)
	assert.Len(t, got.Events, 1)
}

func TestGetRunNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runs/999", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "not_found", decode[errorBody](t, w).Error)

	w = h.do(t, "GET", "/api/v1/runs/abc", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListRunJobs(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runs/100/jobs", "")
	require.Equal(t, http.StatusOK, w.Code)
	raw := decode[map[string]any](t, w)
	assert.Contains(t, raw, "total_count")
	assert.Contains(t, raw, "jobs")
	got := decode[JobListDTO](t, w)
	assert.Equal(t, 3, got.TotalCount)

	w = h.do(t, "GET", "/api/v1/runs/999/jobs", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetJobDetail(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/jobs/201", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[JobDetailDTO](t, w)

	assert.Equal(t, "infra", got.FailureClass)
	assert.Equal(t, "infra_failure", got.Conclusion)
	assert.Contains(t, got.ClassificationReason, "HTTP 524")
	assert.Equal(t, 2, got.Attempt)
	assert.Equal(t, 3, got.MaxAttempts)
	assert.Equal(t, 1, got.RequeueCount)
	require.Len(t, got.Steps, 2)
	assert.Equal(t, "Run tests", got.Steps[1].Name)
	assert.Equal(t, "infra", got.Steps[1].FailureClass)
	assert.Len(t, got.Annotations, 1)
	assert.Len(t, got.Events, 1)
	assert.Equal(t, "wow-look-at-my/ci-platform", got.RepoFull)
	assert.InDelta(t, 10.0, got.Timing.SetupFor, 0.001)
	assert.InDelta(t, 100.0, got.Timing.ExecuteFor, 0.001)

	// Timing keys, verbatim.
	raw := decode[map[string]any](t, w)
	assert.Contains(t, raw, "classification_reason")
	assert.Contains(t, raw, "failure_class")
	assert.Contains(t, raw, "max_attempts")
	assert.Contains(t, raw, "requeue_count")
}

func TestGetJobUsesRequestedAttempt(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/jobs/201?attempt=1", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[JobDetailDTO](t, w)
	assert.Equal(t, 1, got.Attempt)
	assert.Empty(t, got.Steps, "attempt 1 has no recorded steps in the fixture")

	w = h.do(t, "GET", "/api/v1/jobs/201?attempt=0", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCancelRequiresASentence(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{``, `{}`, `{"reason":""}`, `{"reason":"   "}`} {
		w := h.do(t, "POST", "/api/v1/runs/100/cancel", body)
		require.Equal(t, http.StatusBadRequest, w.Code, "body %q", body)
		assert.Equal(t, "missing_reason", decode[errorBody](t, w).Error)
	}
	assert.Empty(t, h.ctrl.cancels, "no cancellation may be recorded without a reason")

	w := h.do(t, "POST", "/api/v1/runs/100/cancel", `{"not":"json"`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCancelRecordsUserActorAndNamesThem(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("POST", "/api/v1/runs/100/cancel", strings.NewReader(`{"reason":"superseded by a newer branch"}`))
	r.Header.Set("X-CI-Actor", "pazer")
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Len(t, h.ctrl.cancels, 1)
	got := h.ctrl.cancels[0]
	assert.Equal(t, model.CancelActorUser, got.Actor)
	assert.Equal(t, "pazer", got.TriggeredBy)
	assert.Contains(t, got.Sentence, "pazer", "the sentence must name the actor")
	assert.Contains(t, got.Sentence, "superseded by a newer branch")
	require.NoError(t, got.Validate())

	body := decode[actionResponse](t, w)
	assert.True(t, body.OK)
	assert.Equal(t, "user", body.Cancel.Actor)
}

func TestCancelJob(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/v1/jobs/201/cancel", `{"reason":"stuck","actor":"ops"}`)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Len(t, h.ctrl.cancels, 1)
	assert.Equal(t, "ops", h.ctrl.cancels[0].TriggeredBy)
	assert.Equal(t, "cancel-job", h.ctrl.actions[0])
	assert.Equal(t, int64(201), h.ctrl.jobIDs[0])
}

func TestRerunEndpoints(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		target string
		action string
	}{
		{"/api/v1/runs/100/rerun", "rerun"},
		{"/api/v1/runs/100/rerun-failed", "rerun-failed"},
		{"/api/v1/jobs/201/rerun", "rerun-job"},
	}
	for i, c := range cases {
		w := h.do(t, "POST", c.target, "")
		require.Equal(t, http.StatusAccepted, w.Code, c.target)
		assert.Equal(t, c.action, h.ctrl.actions[i])
		assert.Equal(t, "operator", h.ctrl.actors[i])
	}
}

func TestActionsWithoutAControllerFailLoud(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Controller = nil })
	for _, target := range []string{"/api/v1/runs/100/cancel", "/api/v1/runs/100/rerun", "/api/v1/jobs/201/rerun"} {
		w := h.do(t, "POST", target, `{"reason":"x"}`)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, target)
		assert.Equal(t, "no_controller", decode[errorBody](t, w).Error)
	}
}

func TestControllerErrorSurfaces(t *testing.T) {
	h := newHarness(t)
	h.ctrl.err = store.ErrNotFound
	w := h.do(t, "POST", "/api/v1/runs/100/cancel", `{"reason":"x"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)

	h.ctrl.err = errUnused
	w = h.do(t, "POST", "/api/v1/runs/100/rerun", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "controller_error", decode[errorBody](t, w).Error)
}

func TestRunnersFleet(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/runners", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[RunnerListDTO](t, w)
	assert.Equal(t, 3, got.TotalCount)
	assert.Equal(t, 1, got.Busy)
	assert.Equal(t, 1, got.Idle)
	assert.Equal(t, 1, got.Offline)
	assert.Equal(t, 4, got.Capacity)
	assert.InDelta(t, 5.0, got.Runners[0].HeartbeatAge, 0.001)
	assert.InDelta(t, 3600.0, got.Runners[2].HeartbeatAge, 0.001)
}

func TestQueueAndHistory(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/queue", "")
	require.Equal(t, http.StatusOK, w.Code)
	raw := decode[map[string]any](t, w)
	for _, k := range []string{"depth", "depth_by_label", "oldest_waiting", "starved_labels", "runners_by_label", "idle_by_label"} {
		assert.Contains(t, raw, k)
	}

	w = h.do(t, "GET", "/api/v1/queue/history?since=1h", "")
	require.Equal(t, http.StatusOK, w.Code)
	hist := decode[QueueHistoryDTO](t, w)
	assert.Equal(t, 2, hist.Count, "the 2h-old sample is outside the window")
	assert.True(t, hist.Samples[0].At.Before(hist.Samples[1].At), "samples must be ordered oldest first")

	w = h.do(t, "GET", "/api/v1/queue/history?since="+testNow.Add(-3*time.Hour).Format(time.RFC3339), "")
	assert.Equal(t, 3, decode[QueueHistoryDTO](t, w).Count)

	w = h.do(t, "GET", "/api/v1/queue/history?since=whenever", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestQueueStoreFailureIsNotAnEmptyQueue(t *testing.T) {
	h := newHarness(t)
	h.st.qstats = nil
	w := h.do(t, "GET", "/api/v1/queue", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, decode[errorBody](t, w).Message, "queue stats")
}

func TestArtifacts(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Blobs = &fakeBlobs{data: []byte("payload")} })
	w := h.do(t, "GET", "/api/v1/runs/100/artifacts", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[ArtifactListDTO](t, w)
	assert.Equal(t, 2, got.TotalCount)
	assert.Equal(t, int64(1024), got.SizeBytes)
	assert.Equal(t, "/api/v1/artifacts/300/download", got.Artifacts[0].DownloadURL)
	assert.False(t, got.Artifacts[0].Expired)

	w = h.do(t, "GET", "/api/v1/artifacts/300/download", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "payload", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Disposition"), "binary.tar.gz")
	assert.Equal(t, "sha256:abc", w.Header().Get("X-Artifact-Digest"))

	w = h.do(t, "GET", "/api/v1/artifacts/301/download", "")
	assert.Equal(t, http.StatusConflict, w.Code, "an unfinalized artifact must not download as empty bytes")

	w = h.do(t, "GET", "/api/v1/artifacts/999/download", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestArtifactDownloadWithoutBlobStore(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/artifacts/300/download", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "no_blob_store", decode[errorBody](t, w).Error)
}

func TestRepoCache(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/repos/wow-look-at-my/ci-platform/cache", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[CacheDTO](t, w)

	assert.Equal(t, "wow-look-at-my/ci-platform", got.Repository)
	assert.Equal(t, int64(4096), got.UsageBytes)
	assert.Equal(t, 2, got.Stats.Hits)
	assert.Equal(t, 1, got.Stats.Misses)
	assert.Equal(t, 1, got.Stats.Evictions)
	assert.InDelta(t, 2.0/3.0, got.Stats.HitRate, 0.0001)

	// Entries come from the store, so an evicted key is simply gone rather
	// than something the reader has to reconcile against the event log.
	require.Len(t, got.Entries, 1, "the evicted key must not appear as live")
	assert.Equal(t, "go-mod", got.Entries[0].Key)

	byKey := map[string]CacheKeyStats{}
	for _, k := range got.ByKey {
		byKey[k.Key] = k
	}
	assert.Equal(t, 2, byKey["go-mod"].Hits)
	assert.Equal(t, 1, byKey["npm"].Misses)

	w = h.do(t, "GET", "/api/v1/repos/nobody/nothing/cache", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnknownRouteIs404(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/nonsense", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMethodMismatch(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/v1/runs", "")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
