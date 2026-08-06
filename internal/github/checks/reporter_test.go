// Reporter coalescing, annotation chunking, and the actions buttons.
package checks

import (
	"context"
	"fmt"
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
