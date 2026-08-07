package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

var (
	base    = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	repo    = gh.Repo{Owner: "wow-look-at-my", Name: "ci-platform"}
	headSHA = "0123456789abcdef0123456789abcdef01234567"
)

// call is one recorded request against the fake Checks API.
type call struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeAPI records every check-run write and answers with a stable id.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []call
	nextID int64
	// fail, when set, is consulted per call; a non-zero return is the status.
	fail func(n int) int
}

func (f *fakeAPI) served() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newFake(t *testing.T) (*fakeAPI, *gh.Client) {
	t.Helper()
	f := &fakeAPI{nextID: 1000}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{Method: r.Method, Path: r.URL.Path, Body: body})
		n := len(f.calls)
		f.mu.Unlock()
		if f.fail != nil {
			if code := f.fail(n); code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
				return
			}
		}
		id := int64(0)
		if r.Method == http.MethodPost {
			f.mu.Lock()
			f.nextID++
			id = f.nextID
			f.mu.Unlock()
		} else {
			id = 1001
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": body["name"]})
	}))
	t.Cleanup(srv.Close)

	cli, err := gh.NewClient(gh.Options{
		BaseURL:    srv.URL,
		Tokens:     gh.StaticToken("ghs_installation"),
		MaxRetries: -1,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Now:        func() time.Time { return base },
	})
	require.NoError(t, err)
	return f, cli
}

func newReporter(t *testing.T, cli *gh.Client, mutate ...func(*ReporterOptions)) *Reporter {
	t.Helper()
	opts := ReporterOptions{
		MinInterval:   time.Hour, // the ticker never fires inside a test
		DisableTicker: true,
		Now:           func() time.Time { return base },
		Sleep:         func(context.Context, time.Duration) error { return nil },
	}
	for _, m := range mutate {
		m(&opts)
	}
	r := NewReporter(cli, opts)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func progress(jobID int64, name string) Update {
	return Update{
		JobID: jobID, Repo: repo, Name: name, HeadSHA: headSHA,
		ExternalID: fmt.Sprintf("job-%d", jobID),
		DetailsURL: "https://ci.example/jobs/1",
		Status:     model.StatusInProgress,
		StartedAt:  ptr(base),
	}
}

func ptr[T any](v T) *T { return &v }

func TestConclusionToCheckMapping(t *testing.T) {
	cases := []struct {
		in     model.Conclusion
		wire   string
		prefix string
	}{
		{model.ConclusionSuccess, "success", "Success"},
		{model.ConclusionFailure, "failure", "Failed"},
		{model.ConclusionTimedOut, "timed_out", "Timed out"},
		{model.ConclusionCancelled, "cancelled", "Cancelled"},
		{model.ConclusionSkipped, "skipped", "Skipped"},
		{model.ConclusionNeutral, "neutral", "Neutral"},
		{model.ConclusionStale, "stale", "Stale"},
		{model.ConclusionActionRequired, "action_required", "Action required"},
		// The deviations: an infra or config failure must not render as a red X.
		{model.ConclusionInfraFailure, "action_required", "Infrastructure failure"},
		{model.ConclusionConfigError, "action_required", "Workflow configuration error"},
	}
	for _, c := range cases {
		wire, prefix := ConclusionToCheck(c.in)
		assert.Equal(t, c.wire, wire, string(c.in))
		assert.Equal(t, c.prefix, prefix, string(c.in))
	}
	wire, prefix := ConclusionToCheck(model.Conclusion("invented"))
	assert.Equal(t, "neutral", wire)
	assert.Contains(t, prefix, "invented")

	// Every model conclusion is mapped; nothing falls through by accident.
	for _, c := range []model.Conclusion{
		model.ConclusionSuccess, model.ConclusionFailure, model.ConclusionNeutral,
		model.ConclusionCancelled, model.ConclusionTimedOut, model.ConclusionActionRequired,
		model.ConclusionSkipped, model.ConclusionStale, model.ConclusionInfraFailure,
		model.ConclusionConfigError,
	} {
		w, p := ConclusionToCheck(c)
		assert.NotEmpty(t, w)
		assert.NotContains(t, p, "Unknown conclusion")
	}
}

func TestStatusTitle(t *testing.T) {
	assert.Equal(t, "Queued", statusTitle(model.StatusQueued))
	assert.Equal(t, "In progress", statusTitle(model.StatusInProgress))
	assert.Equal(t, "Waiting", statusTitle(model.StatusWaiting))
	assert.Equal(t, "Completed", statusTitle(model.StatusCompleted))
	assert.Equal(t, "weird", statusTitle(model.Status("weird")))
}

func TestCreateThenUpdateSendsExactBodies(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	ctx := context.Background()

	require.NoError(t, r.Report(ctx, progress(7, "build (linux)")))

	calls := f.served()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].Method)
	assert.Equal(t, "/repos/wow-look-at-my/ci-platform/check-runs", calls[0].Path)
	assert.Equal(t, "build (linux)", calls[0].Body["name"])
	assert.Equal(t, headSHA, calls[0].Body["head_sha"])
	assert.Equal(t, "in_progress", calls[0].Body["status"])
	assert.Equal(t, "job-7", calls[0].Body["external_id"])
	assert.Equal(t, "https://ci.example/jobs/1", calls[0].Body["details_url"])
	assert.Equal(t, "2026-08-06T12:00:00Z", calls[0].Body["started_at"])
	assert.Nil(t, calls[0].Body["conclusion"])
	assert.Equal(t, int64(1001), r.CheckRunID(7))

	// A completion PATCHes the same check run, and never repeats head_sha.
	done := progress(7, "build (linux)")
	done.Status = model.StatusCompleted
	done.Conclusion = model.ConclusionSuccess
	done.CompletedAt = ptr(base.Add(90 * time.Second))
	require.NoError(t, r.Report(ctx, done))

	calls = f.served()
	require.Len(t, calls, 2)
	assert.Equal(t, http.MethodPatch, calls[1].Method)
	assert.Equal(t, "/repos/wow-look-at-my/ci-platform/check-runs/1001", calls[1].Path)
	assert.Equal(t, "completed", calls[1].Body["status"])
	assert.Equal(t, "success", calls[1].Body["conclusion"])
	assert.Equal(t, "2026-08-06T12:01:30Z", calls[1].Body["completed_at"])
	assert.Nil(t, calls[1].Body["head_sha"])
}

func TestCoalescingRapidReportsToOneCallPlusFinal(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	ctx := context.Background()

	for i := range 25 {
		u := progress(7, "test")
		u.Summary = fmt.Sprintf("step %d", i)
		require.NoError(t, r.Report(ctx, u))
	}
	assert.Equal(t, 1, f.count(), "25 rapid reports must collapse to the single create")

	final := progress(7, "test")
	final.Status = model.StatusCompleted
	final.Conclusion = model.ConclusionFailure
	final.Summary = "the last word"
	require.NoError(t, r.Report(ctx, final))

	calls := f.served()
	require.Len(t, calls, 2, "one call plus the final update")
	out := calls[1].Body["output"].(map[string]any)
	assert.Contains(t, out["summary"], "the last word")
	assert.Equal(t, "failure", calls[1].Body["conclusion"])
}

func TestMinIntervalLetsALaterUpdateThrough(t *testing.T) {
	f, cli := newFake(t)
	now := base
	r := newReporter(t, cli, func(o *ReporterOptions) {
		o.MinInterval = 2 * time.Second
		o.Now = func() time.Time { return now }
	})
	ctx := context.Background()

	require.NoError(t, r.Report(ctx, progress(7, "test")))
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	assert.Equal(t, 1, f.count())

	now = now.Add(3 * time.Second)
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	assert.Equal(t, 2, f.count())
}

func TestFlushForcesAPendingUpdate(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	ctx := context.Background()

	require.NoError(t, r.Report(ctx, progress(7, "test")))
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	assert.Equal(t, 1, f.count())

	require.NoError(t, r.Flush(ctx, 7))
	assert.Equal(t, 2, f.count())

	// Flushing with nothing pending, and for an unknown job, are no-ops.
	require.NoError(t, r.Flush(ctx, 7))
	require.NoError(t, r.Flush(ctx, 999))
	assert.Equal(t, 2, f.count())
}

func TestCloseFlushesPending(t *testing.T) {
	f, cli := newFake(t)
	r := NewReporter(cli, ReporterOptions{
		MinInterval: time.Hour, DisableTicker: true,
		Now: func() time.Time { return base }, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	ctx := context.Background()
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	assert.Equal(t, 1, f.count())

	require.NoError(t, r.Close(ctx))
	assert.Equal(t, 2, f.count())
	require.NoError(t, r.Close(ctx), "Close is idempotent")
}

func TestBackgroundTickerFlushes(t *testing.T) {
	f, cli := newFake(t)
	r := NewReporter(cli, ReporterOptions{
		MinInterval: 10 * time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	defer func() { _ = r.Close(context.Background()) }()
	ctx := context.Background()

	require.NoError(t, r.Report(ctx, progress(7, "test")))
	require.NoError(t, r.Report(ctx, progress(7, "test")))
	assert.Eventually(t, func() bool { return f.count() >= 2 }, 2*time.Second, 5*time.Millisecond)
}

func TestFinalUpdateIsRetriedAndReportedWhenLost(t *testing.T) {
	f, cli := newFake(t)
	f.fail = func(n int) int {
		if n == 1 {
			return 0 // the create succeeds
		}
		return http.StatusInternalServerError
	}
	var slept []time.Duration
	r := newReporter(t, cli, func(o *ReporterOptions) {
		o.FinalAttempts = 3
		o.FinalBackoff = 100 * time.Millisecond
		o.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	})
	ctx := context.Background()
	require.NoError(t, r.Report(ctx, progress(7, "test")))

	final := progress(7, "test")
	final.Status = model.StatusCompleted
	final.Conclusion = model.ConclusionInfraFailure
	err := r.Report(ctx, final)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "job 7")
	assert.Equal(t, 4, f.count(), "create plus three completion attempts")
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, slept)

	// The lost update stays queued rather than disappearing.
	f.fail = nil
	require.NoError(t, r.Flush(ctx, 7))
	calls := f.served()
	assert.Equal(t, "action_required", calls[len(calls)-1].Body["conclusion"])
}

func TestFinalUpdateRetrySucceedsOnSecondAttempt(t *testing.T) {
	f, cli := newFake(t)
	f.fail = func(n int) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r := newReporter(t, cli, func(o *ReporterOptions) { o.FinalAttempts = 3 })
	final := progress(7, "test")
	final.Status = model.StatusCompleted
	final.Conclusion = model.ConclusionSuccess
	require.NoError(t, r.Report(context.Background(), final))
	assert.Equal(t, 2, f.count())
}

func TestAnnotationChunkingAtFifty(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)

	anns := make([]model.Annotation, 0, 120)
	for i := range 120 {
		anns = append(anns, model.Annotation{
			Path: "main.go", StartLine: i + 1, EndLine: i + 1, StartCol: 3, EndCol: 9,
			Level: model.AnnotationWarning, Message: fmt.Sprintf("note %d", i), Title: "vet",
		})
	}
	u := progress(7, "vet")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionFailure
	u.Annotations = anns
	require.NoError(t, r.Report(context.Background(), u))

	calls := f.served()
	require.Len(t, calls, 3, "50 + 50 + 20 across one create and two follow-up PATCHes")
	assert.Equal(t, http.MethodPost, calls[0].Method)
	assert.Equal(t, http.MethodPatch, calls[1].Method)
	assert.Equal(t, http.MethodPatch, calls[2].Method)

	counts := []int{}
	for _, c := range calls {
		out := c.Body["output"].(map[string]any)
		counts = append(counts, len(out["annotations"].([]any)))
		assert.NotEmpty(t, out["title"], "every annotation request must carry a title")
		assert.NotEmpty(t, out["summary"], "every annotation request must carry a summary")
	}
	assert.Equal(t, []int{50, 50, 20}, counts)

	first := calls[0].Body["output"].(map[string]any)["annotations"].([]any)[0].(map[string]any)
	assert.Equal(t, "main.go", first["path"])
	assert.EqualValues(t, 1, first["start_line"])
	assert.EqualValues(t, 3, first["start_column"])
	assert.Equal(t, "warning", first["annotation_level"])
}
