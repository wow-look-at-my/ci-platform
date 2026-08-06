package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAnnotationsAreNotResent(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	ctx := context.Background()

	u := progress(7, "vet")
	u.Annotations = []model.Annotation{{Path: "a.go", StartLine: 1, EndLine: 1, Message: "one"}}
	require.NoError(t, r.Report(ctx, u))

	u2 := progress(7, "vet")
	u2.Status = model.StatusCompleted
	u2.Conclusion = model.ConclusionFailure
	u2.Annotations = []model.Annotation{
		{Path: "a.go", StartLine: 1, EndLine: 1, Message: "one"},
		{Path: "b.go", StartLine: 2, EndLine: 2, Message: "two"},
	}
	require.NoError(t, r.Report(ctx, u2))

	calls := f.served()
	require.Len(t, calls, 2)
	second := calls[1].Body["output"].(map[string]any)["annotations"].([]any)
	require.Len(t, second, 1)
	assert.Equal(t, "b.go", second[0].(map[string]any)["path"])
}

func TestAnnotationTruncationAndDefaults(t *testing.T) {
	long := strings.Repeat("x", MaxAnnotationMessage+500)
	got := annotationFor(model.Annotation{
		Path: "a.go", Message: long, Title: strings.Repeat("t", 400), RawDetail: long,
		StartLine: 0, EndLine: 0, StartCol: 2, EndCol: 4,
	})
	assert.Len(t, got.Message, MaxAnnotationMessage)
	assert.Len(t, got.Title, MaxAnnotationTitle)
	assert.Len(t, got.RawDetails, MaxAnnotationMessage)
	assert.Equal(t, "failure", got.AnnotationLevel, "an unset level defaults to failure, never to notice")
	assert.Equal(t, 1, got.StartLine)
	assert.Equal(t, 1, got.EndLine)
	assert.Equal(t, 2, got.StartColumn)

	// Columns are dropped on a multi-line span, which the API rejects.
	multi := annotationFor(model.Annotation{Path: "a.go", StartLine: 4, EndLine: 9, StartCol: 1, EndCol: 2})
	assert.Zero(t, multi.StartColumn)
	assert.Zero(t, multi.EndColumn)
}

func TestOutputRendersStepTableAndContext(t *testing.T) {
	u := progress(7, "build")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionInfraFailure
	u.Attempt = 2
	u.MaxAttempts = 3
	u.Class = model.ClassInfra
	u.ClassReason = "the container registry returned a 5xx"
	u.Timing = &model.Timing{QueuedFor: 3 * time.Second, SetupFor: 12 * time.Second,
		ExecuteFor: 64 * time.Second, TotalFor: 79 * time.Second}
	u.Steps = []model.Step{
		{Number: 1, Name: "Set up job", Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
			StartedAt: ptr(base), CompletedAt: ptr(base.Add(2 * time.Second))},
		{Number: 2, Name: "docker push | tee log", Status: model.StatusCompleted, Conclusion: model.ConclusionInfraFailure,
			StartedAt: ptr(base), CompletedAt: ptr(base.Add(70 * time.Second))},
		{Number: 3, Name: "Upload", Status: model.StatusQueued},
	}

	out := Render(u, base)
	assert.Equal(t, "Infrastructure failure", out.Title)
	assert.Contains(t, out.Summary, "https://ci.example/jobs/1")
	assert.Contains(t, out.Summary, "Attempt 2 of 3")
	assert.Contains(t, out.Summary, "Classified as **infrastructure**: the container registry returned a 5xx")
	assert.Contains(t, out.Summary, "not of the code under test")
	assert.Contains(t, out.Summary, "Queued 3.0s, setup 12.0s, execute 1m04s (total 1m19s)")

	assert.Contains(t, out.Text, "| Step | Result | Duration |")
	assert.Contains(t, out.Text, "| Set up job | Success | 2.0s |")
	assert.Contains(t, out.Text, `| docker push \| tee log | Infrastructure failure | 1m10s |`)
	assert.Contains(t, out.Text, "| Upload | Queued | - |")
	assert.Contains(t, out.Text, "renders its collapsible per-step view only for its own Actions runner")
}

func TestOutputCancellationAndConfigError(t *testing.T) {
	u := progress(7, "build")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionCancelled
	u.Cancel = &model.CancelReason{
		Actor:       model.CancelActorConcurrencyGroup,
		Sentence:    "a newer run for the same branch took the concurrency slot",
		TriggeredBy: "run 918",
	}
	out := Render(u, base)
	assert.Equal(t, "Cancelled", out.Title)
	assert.Contains(t, out.Summary, "Cancelled by **concurrency_group**")
	assert.Contains(t, out.Summary, "took the concurrency slot")
	assert.Contains(t, out.Summary, "(triggered by run 918)")

	cfg := progress(7, "build")
	cfg.Status = model.StatusCompleted
	cfg.Conclusion = model.ConclusionConfigError
	cfg.Class = model.ClassConfig
	assert.Contains(t, Summary(cfg, base), "retrying cannot help")
	assert.Contains(t, Summary(cfg, base), "workflow configuration")
}

func TestOutputSingleAttemptAndNoDetails(t *testing.T) {
	u := Update{JobID: 1, Repo: repo, Name: "n", HeadSHA: headSHA, Status: model.StatusQueued}
	assert.Equal(t, "Queued", Title(u))
	assert.Equal(t, "Queued", Summary(u, base), "an empty summary falls back to the title, never to blank")
	assert.Equal(t, "", Text(u, base))

	u.Attempt = 2
	assert.Contains(t, Summary(u, base), "Attempt 2.")

	u.Attempt, u.Summary = 0, "custom line"
	assert.Contains(t, Summary(u, base), "custom line")
}

func TestOutputTruncationIsAnnounced(t *testing.T) {
	u := progress(7, "build")
	u.Summary = strings.Repeat("y", MaxOutputSummary+1000)
	s := Summary(u, base)
	assert.LessOrEqual(t, len(s), MaxOutputSummary)
	assert.Contains(t, s, "[truncated: output exceeded GitHub's limit]")

	steps := make([]model.Step, 4000)
	for i := range steps {
		steps[i] = model.Step{Number: i, Name: strings.Repeat("n", 40), Status: model.StatusCompleted,
			Conclusion: model.ConclusionSuccess}
	}
	u2 := progress(7, "build")
	u2.Steps = steps
	txt := Text(u2, base)
	assert.LessOrEqual(t, len(txt), MaxOutputText)
	assert.Contains(t, txt, "[truncated")

	assert.Len(t, Title(Update{Status: model.Status(strings.Repeat("z", 400))}), MaxOutputTitle)
}

func TestStepTableEdgeCases(t *testing.T) {
	assert.Equal(t, "", StepTable(nil, base))
	tbl := StepTable([]model.Step{
		{Number: 4, Status: model.StatusCompleted},
		{Number: 5, Name: "flaky", Status: model.StatusCompleted, Conclusion: model.ConclusionFailure, ContinueOnError: true},
		{Number: 6, Name: strings.Repeat("w", 400), Status: model.StatusInProgress, StartedAt: ptr(base.Add(-time.Hour))},
	}, base)
	assert.Contains(t, tbl, "| step 4 | completed | - |")
	assert.Contains(t, tbl, "Failed (continue-on-error)")
	assert.Contains(t, tbl, "1h00m")
	assert.Contains(t, tbl, strings.Repeat("w", 197)+"...")
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "-", formatDuration(0))
	assert.Equal(t, "-", formatDuration(-time.Second))
	assert.Equal(t, "250ms", formatDuration(250*time.Millisecond))
	assert.Equal(t, "3.5s", formatDuration(3500*time.Millisecond))
	assert.Equal(t, "2m05s", formatDuration(125*time.Second))
	assert.Equal(t, "2h03m", formatDuration(123*time.Minute))
}

func TestTruncateRespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 10) // two bytes each
	got := truncate(s, 5)
	assert.Equal(t, 4, len(got))
	assert.True(t, strings.HasPrefix(s, got))
	assert.Equal(t, "abc", truncate("abc", 10))
}

func TestUpdateValidation(t *testing.T) {
	ok := progress(7, "build")
	require.NoError(t, ok.Validate())

	bad := ok
	bad.JobID = 0
	require.ErrorContains(t, bad.Validate(), "no JobID")

	bad = ok
	bad.Repo = gh.Repo{}
	require.ErrorContains(t, bad.Validate(), "repo owner/name")

	bad = ok
	bad.Name = ""
	require.ErrorContains(t, bad.Validate(), "no Name")

	bad = ok
	bad.HeadSHA = ""
	require.ErrorContains(t, bad.Validate(), "no HeadSHA")

	bad = ok
	bad.Status = model.Status("nope")
	require.ErrorContains(t, bad.Validate(), "invalid status")

	bad = ok
	bad.Status = model.StatusCompleted
	require.ErrorContains(t, bad.Validate(), "no conclusion")

	bad.Conclusion = model.Conclusion("weird")
	require.ErrorContains(t, bad.Validate(), "invalid conclusion")

	// A cancellation with no recorded reason is refused outright.
	bad.Conclusion = model.ConclusionCancelled
	require.ErrorContains(t, bad.Validate(), "cancelled with no CancelReason")

	bad.Cancel = &model.CancelReason{Actor: model.CancelActorUser}
	require.ErrorContains(t, bad.Validate(), "no explanation sentence")

	bad.Cancel = &model.CancelReason{Actor: model.CancelActorUser, Sentence: "a maintainer cancelled it"}
	require.NoError(t, bad.Validate())
}

func TestActionValidation(t *testing.T) {
	for _, a := range DefaultActions() {
		require.NoError(t, a.Validate())
		assert.LessOrEqual(t, len(a.Label), MaxActionLabel)
		assert.LessOrEqual(t, len(a.Description), MaxActionDescription)
	}
	require.ErrorContains(t, Action{Label: "x"}.Validate(), "no identifier")
	require.ErrorContains(t, Action{Identifier: strings.Repeat("i", 21), Label: "x"}.Validate(), "identifier")
	require.ErrorContains(t, Action{Identifier: "i"}.Validate(), "no label")
	require.ErrorContains(t, Action{Identifier: "i", Label: strings.Repeat("l", 21)}.Validate(), "label")
	require.ErrorContains(t, Action{Identifier: "i", Label: "l", Description: strings.Repeat("d", 41)}.Validate(), "description")

	u := progress(7, "b")
	u.Actions = []Action{{Identifier: "a", Label: "A"}, {Identifier: "b", Label: "B"},
		{Identifier: "c", Label: "C"}, {Identifier: "d", Label: "D"}}
	require.ErrorContains(t, u.Validate(), "limit 3")
}

func TestCompletedUpdateCarriesDefaultActions(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	u := progress(7, "build")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionFailure
	require.NoError(t, r.Report(context.Background(), u))

	acts := f.served()[0].Body["actions"].([]any)
	require.Len(t, acts, 2)
	assert.Equal(t, "Re-run job", acts[0].(map[string]any)["label"])
	assert.Equal(t, ActionRerunFailedJobs, acts[1].(map[string]any)["identifier"])
}

func TestExplicitEmptyActionsSuppressesButtons(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	u := progress(7, "build")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionSuccess
	u.Actions = []Action{}
	require.NoError(t, r.Report(context.Background(), u))
	assert.Nil(t, f.served()[0].Body["actions"])
}

func TestReporterRejectsInvalidUpdateBeforeAnyCall(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	err := r.Report(context.Background(), Update{Name: "x"})
	require.Error(t, err)
	assert.Zero(t, f.count())
}

func TestReporterWithoutClientFailsLoudly(t *testing.T) {
	r := NewReporter(nil, ReporterOptions{DisableTicker: true})
	err := r.Report(context.Background(), progress(1, "x"))
	require.ErrorContains(t, err, "no GitHub client")
}

func TestExistingCheckRunIDSkipsTheCreate(t *testing.T) {
	f, cli := newFake(t)
	r := newReporter(t, cli)
	u := progress(7, "build")
	u.CheckRunID = 555
	require.NoError(t, r.Report(context.Background(), u))
	calls := f.served()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPatch, calls[0].Method)
	assert.Equal(t, "/repos/wow-look-at-my/ci-platform/check-runs/555", calls[0].Path)
}

func TestOnCheckRunIDCallback(t *testing.T) {
	_, cli := newFake(t)
	var gotJob, gotRun int64
	r := newReporter(t, cli, func(o *ReporterOptions) {
		o.OnCheckRunID = func(jobID, checkRunID int64) { gotJob, gotRun = jobID, checkRunID }
	})
	require.NoError(t, r.Report(context.Background(), progress(7, "build")))
	assert.Equal(t, int64(7), gotJob)
	assert.Equal(t, int64(1001), gotRun)
	assert.Zero(t, r.CheckRunID(404))
}

func TestCreateWithoutAnIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cli, err := gh.NewClient(gh.Options{BaseURL: srv.URL, Tokens: gh.StaticToken("t"), MaxRetries: -1})
	require.NoError(t, err)
	r := newReporter(t, cli)
	require.ErrorContains(t, r.Report(context.Background(), progress(7, "build")), "no id")
}

func TestAnnotationFollowUpFailureIsReported(t *testing.T) {
	f, cli := newFake(t)
	f.fail = func(n int) int {
		if n == 2 {
			return http.StatusInternalServerError
		}
		return 0
	}
	r := newReporter(t, cli, func(o *ReporterOptions) { o.FinalAttempts = 1 })

	anns := make([]model.Annotation, 60)
	for i := range anns {
		anns[i] = model.Annotation{Path: "a.go", StartLine: i + 1, EndLine: i + 1, Message: "m"}
	}
	u := progress(7, "vet")
	u.Status = model.StatusCompleted
	u.Conclusion = model.ConclusionFailure
	u.Annotations = anns
	err := r.Report(context.Background(), u)
	require.ErrorContains(t, err, "delivering annotations")
}

func TestConcurrentReportsAreSafe(t *testing.T) {
	_, cli := newFake(t)
	r := newReporter(t, cli, func(o *ReporterOptions) { o.MinInterval = time.Millisecond })
	var wg sync.WaitGroup
	for j := range 8 {
		for range 20 {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				assert.NoError(t, r.Report(context.Background(), progress(int64(j+1), fmt.Sprintf("job-%d", j))))
			}(j)
		}
	}
	wg.Wait()
	for j := range 8 {
		assert.NotZero(t, r.CheckRunID(int64(j+1)))
	}
}

func TestMergeAnnotationsKeepsTheLongerList(t *testing.T) {
	a := []model.Annotation{{Path: "a"}}
	b := []model.Annotation{{Path: "a"}, {Path: "b"}}
	assert.Len(t, mergeAnnotations(b, a), 2)
	assert.Len(t, mergeAnnotations(a, b), 2)
}

func TestChunkAnnotationsEmpty(t *testing.T) {
	head, rest := chunkAnnotations(nil)
	assert.Nil(t, head)
	assert.Nil(t, rest)
}

func TestSleepCtxHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, sleepCtx(ctx, time.Minute))
	require.NoError(t, sleepCtx(ctx, 0))
}
