package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func ctx() context.Context { return context.Background() }

func TestStartRunQueuesOnlyRootJobs(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	if h.st.queued(h.job("build").ID) == nil {
		t.Fatal("build should be queued")
	}
	if h.st.queued(h.job("test").ID) != nil {
		t.Fatal("test must not be queued before build finishes")
	}
	if len(h.st.eventsOfKind(EventQueued)) != 1 {
		t.Fatalf("want one queued event, got %d", len(h.st.eventsOfKind(EventQueued)))
	}
}

func TestTickIsIdempotent(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.tick(base)
	h.tick(base)
	if n := len(h.st.eventsOfKind(EventQueued)); n != 1 {
		t.Fatalf("a job was queued %d times", n)
	}
}

func TestDependentRunsAfterSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if h.st.queued(h.job("test").ID) == nil {
		t.Fatal("test should be queued once build succeeded")
	}
}

// A failed dependency skips its dependents, and a skipped job is never
// recorded as success.
func TestFailurePropagatesAsSkipNotSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
		jobIR("deploy", func(j *model.JobIR) { j.Needs = []string{"test"} }),
	))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	h.tick(base.Add(3 * time.Minute))

	h.requireConclusion("test", model.ConclusionSkipped)
	h.requireConclusion("deploy", model.ConclusionSkipped)
	if h.runRow().Conclusion != model.ConclusionFailure {
		t.Fatalf("run concluded %q", h.runRow().Conclusion)
	}
	skips := h.st.eventsOfKind(EventSkipped)
	if len(skips) != 2 {
		t.Fatalf("want two skip events, got %d", len(skips))
	}
	if !strings.Contains(skips[0].Message, "build concluded failure") {
		t.Fatalf("skip event does not say why: %q", skips[0].Message)
	}
}

// GitHub compiles a condition naming no status function as
// "success() && (<condition>)", so a plain if: still does not run after a
// failed dependency.
func TestPlainIfIsWrappedInSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("deploy", func(j *model.JobIR) {
			j.Needs = []string{"build"}
			j.If = model.NewExpr("github.ref == 'refs/heads/feature'")
		}),
	))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	h.requireConclusion("deploy", model.ConclusionSkipped)
}

func TestAlwaysOverridesAFailedDependency(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("report", func(j *model.JobIR) {
			j.Needs = []string{"build"}
			j.If = model.NewExpr("always()")
		}),
	))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if h.st.queued(h.job("report").ID) == nil {
		t.Fatal("always() should run the job after a failure")
	}
}

func TestFailureFunctionRunsOnlyOnFailure(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("oncall", func(j *model.JobIR) {
			j.Needs = []string{"build"}
			j.If = model.NewExpr("failure()")
		}),
	))
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	h.requireConclusion("oncall", model.ConclusionSkipped)
}

func TestNeedsContextCarriesOutputsAndResult(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("deploy", func(j *model.JobIR) {
			j.Needs = []string{"build"}
			j.If = model.NewExpr("needs.build.outputs.version == '1.2.3'")
		}),
	))
	h.tick(base)
	j := h.job("build")
	j.Status = model.StatusInProgress
	if err := h.st.UpdateJob(ctx(), j); err != nil {
		t.Fatal(err)
	}
	if err := h.s.JobCompletedAt(ctx(), j.ID, Result{
		Conclusion: model.ConclusionSuccess,
		Outputs:    map[string]string{"version": "1.2.3"},
	}, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.tick(base.Add(2 * time.Minute))
	if h.st.queued(h.job("deploy").ID) == nil {
		t.Fatal("deploy should have seen needs.build.outputs.version")
	}
}

// A run where every job was skipped must never report success.
func TestAllSkippedRunIsNotSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("a", func(j *model.JobIR) { j.If = model.NewExpr("false") }),
		jobIR("b", func(j *model.JobIR) { j.If = model.NewExpr("false") }),
	))
	h.tick(base)
	got := h.runRow()
	if got.Conclusion != model.ConclusionSkipped {
		t.Fatalf("an all-skipped run concluded %q", got.Conclusion)
	}
	roll, err := h.s.RunRollup(ctx(), h.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(roll.Summary, "success") {
		t.Fatalf("summary claims success: %q", roll.Summary)
	}
}

// continue-on-error unblocks dependents without hiding the failure from the
// rollup.
func TestContinueOnErrorUnblocksDependentsButStaysRed(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("flaky", func(j *model.JobIR) { j.ContinueOnError = model.NewExpr("true") }),
		jobIR("after", func(j *model.JobIR) { j.Needs = []string{"flaky"} }),
	))
	h.tick(base)
	h.complete("flaky", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))

	if h.st.queued(h.job("after").ID) == nil {
		t.Fatal("continue-on-error must not block dependents")
	}
	h.requireConclusion("flaky", model.ConclusionFailure)
	h.complete("after", model.ConclusionSuccess, model.ClassNone, base.Add(3*time.Minute))
	h.tick(base.Add(4 * time.Minute))
	if got := h.runRow().Conclusion; got != model.ConclusionFailure {
		t.Fatalf("the rollup swallowed a continue-on-error failure: %q", got)
	}
}

func TestInfraFailureRetriesWithBackoff(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.complete("build", model.ConclusionInfraFailure, model.ClassInfra, base.Add(time.Minute))

	j := h.job("build")
	if j.Status == model.StatusCompleted {
		t.Fatal("an infra failure inside the policy must not complete the job")
	}
	if j.Attempt != 2 || j.InfraRetryCount != 1 {
		t.Fatalf("attempt %d, retries %d", j.Attempt, j.InfraRetryCount)
	}
	q := h.st.queued(j.ID)
	if q == nil {
		t.Fatal("a retried job must go back on the queue")
	}
	want := base.Add(time.Minute).Add(model.DefaultRetryPolicy().Delay(2))
	if !q.NotBefore.Equal(want) {
		t.Fatalf("NotBefore %s, want %s", q.NotBefore, want)
	}
	if len(h.st.eventsOfKind(EventRetried)) != 1 {
		t.Fatal("a retry must be recorded")
	}
}

func TestUserAndConfigFailuresAreNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		class model.FailureClass
		concl model.Conclusion
	}{
		{model.ClassUser, model.ConclusionFailure},
		{model.ClassConfig, model.ConclusionConfigError},
	} {
		h := newHarness(t, wf(jobIR("build")))
		h.tick(base)
		h.complete("build", tc.concl, tc.class, base.Add(time.Minute))
		j := h.job("build")
		if j.Status != model.StatusCompleted || j.Attempt != 1 {
			t.Fatalf("%s failure was retried: status %s attempt %d", tc.class, j.Status, j.Attempt)
		}
	}
}

func TestRetriesExhaustAndTheJobFails(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	at := base
	for i := 0; i < model.DefaultRetryPolicy().Attempts; i++ {
		at = at.Add(time.Minute)
		h.complete("build", model.ConclusionInfraFailure, model.ClassInfra, at)
	}
	j := h.job("build")
	if j.Status != model.StatusCompleted || j.Conclusion != model.ConclusionInfraFailure {
		t.Fatalf("after exhausting retries: status %s conclusion %s", j.Status, j.Conclusion)
	}
	if j.InfraRetryCount != model.DefaultRetryPolicy().Attempts-1 {
		t.Fatalf("retry count %d", j.InfraRetryCount)
	}
}

// A runner that disappears must never fail or lose the job.
func TestLeaseReapRequeuesWithAReason(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	j := h.job("build")
	j.Status = model.StatusInProgress
	j.RunnerID = "runner-9"
	started := base.Add(time.Minute)
	j.StartedAt = &started
	if err := h.st.UpdateJob(ctx(), j); err != nil {
		t.Fatal(err)
	}
	h.st.reap = []*model.Job{j}
	h.tick(base.Add(5 * time.Minute))

	got := h.job("build")
	if got.Status != model.StatusQueued {
		t.Fatalf("a reaped job is queued again, got %s", got.Status)
	}
	if got.Conclusion != "" {
		t.Fatalf("a lost runner must not conclude the job, got %q", got.Conclusion)
	}
	if got.RequeueCount != 1 || got.Attempt != 2 {
		t.Fatalf("requeues %d attempt %d", got.RequeueCount, got.Attempt)
	}
	ev := h.st.eventsOfKind(EventRequeued)
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "runner-9") {
		t.Fatalf("requeue event: %+v", ev)
	}
	if ev[0].Detail["actor"] != string(model.CancelActorRunnerLost) {
		t.Fatalf("requeue actor: %v", ev[0].Detail["actor"])
	}
}

func TestConcurrencyGroupCancelsInProgress(t *testing.T) {
	group := func(j *model.JobIR) {
		j.Concurrency = &model.Concurrency{Group: model.NewExpr("deploy"), CancelInProgress: model.NewExpr("true")}
	}
	h := newHarness(t, wf(
		jobIR("gate"),
		jobIR("deploy-a", group),
		jobIR("deploy-b", func(j *model.JobIR) { j.Needs = []string{"gate"}; group(j) }),
	))
	h.tick(base)
	if h.st.queued(h.job("deploy-a").ID) == nil {
		t.Fatal("deploy-a should hold the group first")
	}
	// deploy-b becomes ready while deploy-a holds the group.
	h.complete("gate", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))

	a, b := h.job("deploy-a"), h.job("deploy-b")
	if a.Status != model.StatusCompleted {
		t.Fatalf("the older job should have been superseded, got %s", a.Status)
	}
	if a.Conclusion != model.ConclusionCancelled || a.Cancel == nil {
		t.Fatalf("cancelled without a reason: %+v", a)
	}
	if a.Cancel.Actor != model.CancelActorConcurrencyGroup {
		t.Fatalf("actor %q", a.Cancel.Actor)
	}
	if !strings.Contains(a.Cancel.Sentence, "deploy") {
		t.Fatalf("sentence does not name the group: %q", a.Cancel.Sentence)
	}
	if h.st.queued(b.ID) == nil {
		t.Fatal("the superseding job should be queued")
	}
}

func TestConcurrencyGroupWithoutCancelWaits(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("deploy-a", func(j *model.JobIR) {
			j.Concurrency = &model.Concurrency{Group: model.NewExpr("deploy")}
		}),
		jobIR("deploy-b", func(j *model.JobIR) {
			j.Concurrency = &model.Concurrency{Group: model.NewExpr("deploy")}
		}),
	))
	h.tick(base)
	b := h.job("deploy-b")
	if b.Status != model.StatusWaiting {
		t.Fatalf("the second job should be waiting, got %s", b.Status)
	}
	if h.st.queued(b.ID) != nil {
		t.Fatal("a waiting job must not be on the queue")
	}
	ev := h.st.eventsOfKind(EventWaiting)
	if len(ev) != 1 {
		t.Fatalf("want one waiting event, got %d", len(ev))
	}
	// It stays waiting, and does not write an event a tick.
	h.tick(base.Add(time.Minute))
	if len(h.st.eventsOfKind(EventWaiting)) != 1 {
		t.Fatal("waiting must be recorded once, not once a tick")
	}
	// Once the holder finishes, the waiter is admitted.
	h.complete("deploy-a", model.ConclusionSuccess, model.ClassNone, base.Add(2*time.Minute))
	h.tick(base.Add(3 * time.Minute))
	if h.st.queued(h.job("deploy-b").ID) == nil {
		t.Fatal("the waiter should be admitted once the group frees up")
	}
}

func TestFailFastCancelsSiblingLegs(t *testing.T) {
	h := newHarness(t, wf(jobIR("test", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu", "windows", "macos"}},
			Order:      []string{"os"},
		}}
	})))
	h.tick(base)
	h.complete("test (ubuntu)", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))

	for _, name := range []string{"test (windows)", "test (macos)"} {
		j := h.job(name)
		if j.Conclusion != model.ConclusionCancelled {
			t.Fatalf("%s concluded %q, want cancelled", name, j.Conclusion)
		}
		if j.Cancel == nil || !strings.Contains(j.Cancel.Sentence, "fail-fast") {
			t.Fatalf("%s: cancel reason %+v", name, j.Cancel)
		}
	}
}

func TestFailFastFalseLetsSiblingsFinish(t *testing.T) {
	no := false
	h := newHarness(t, wf(jobIR("test", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{FailFast: &no, Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
			Order:      []string{"os"},
		}}
	})))
	h.tick(base)
	h.complete("test (ubuntu)", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if got := h.job("test (windows)").Conclusion; got != "" {
		t.Fatalf("fail-fast: false still cancelled a sibling (%q)", got)
	}
}

func TestMaxParallelLimitsLegs(t *testing.T) {
	h := newHarness(t, wf(jobIR("test", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{
			MaxParallel: model.NewExpr("1"),
			Matrix: &model.Matrix{
				Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
				Order:      []string{"os"},
			},
		}
	})))
	h.tick(base)
	if h.st.queued(h.job("test (windows)").ID) != nil {
		t.Fatal("max-parallel 1 must admit only one leg")
	}
	h.complete("test (ubuntu)", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if h.st.queued(h.job("test (windows)").ID) == nil {
		t.Fatal("the second leg should run once the first finished")
	}
}

func TestJobTimeoutIsTimedOutAndNotRetried(t *testing.T) {
	h := newHarness(t, wf(jobIR("build", func(j *model.JobIR) {
		j.TimeoutMinutes = model.NewExpr("5")
	})))
	h.tick(base)
	j := h.job("build")
	started := base.Add(time.Minute)
	j.Status = model.StatusInProgress
	j.StartedAt = &started
	j.SetupCompletedAt = &started
	if err := h.st.UpdateJob(ctx(), j); err != nil {
		t.Fatal(err)
	}
	h.tick(started.Add(6 * time.Minute))

	got := h.job("build")
	if got.Conclusion != model.ConclusionTimedOut {
		t.Fatalf("conclusion %q", got.Conclusion)
	}
	if got.Cancel == nil || got.Cancel.Actor != model.CancelActorTimeout {
		t.Fatalf("cancel reason %+v", got.Cancel)
	}
	if got.Attempt != 1 {
		t.Fatal("a timeout is the user's and must not retry")
	}
}

// A job stuck in setup ran no user command, so it is the platform's fault and
// it retries.
func TestSetupTimeoutIsInfraAndRetries(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")), func(o *Options) { o.SetupTimeout = 2 * time.Minute })
	h.tick(base)
	j := h.job("build")
	started := base.Add(time.Minute)
	j.Status = model.StatusInProgress
	j.StartedAt = &started
	if err := h.st.UpdateJob(ctx(), j); err != nil {
		t.Fatal(err)
	}
	h.tick(started.Add(3 * time.Minute))

	got := h.job("build")
	if got.Status == model.StatusCompleted {
		t.Fatalf("a setup timeout inside the retry policy must retry, got %q", got.Conclusion)
	}
	if got.Attempt != 2 || got.InfraRetryCount != 1 {
		t.Fatalf("attempt %d retries %d", got.Attempt, got.InfraRetryCount)
	}
	ev := h.st.eventsOfKind(EventRetried)
	if len(ev) != 1 || ev[0].Detail["class"] != string(model.ClassInfra) {
		t.Fatalf("retry event %+v", ev)
	}
}

func TestStepTimeoutStopsTheJob(t *testing.T) {
	h := newHarness(t, wf(jobIR("build", func(j *model.JobIR) {
		j.Steps = []*model.StepIR{{
			Number: 1, Name: model.NewExpr("slow"), Run: model.NewExpr("sleep"),
			TimeoutMinutes: model.NewExpr("2"),
		}}
	})))
	h.tick(base)
	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if a.Steps[0].TimeoutMinutes != 2 {
		t.Fatalf("step timeout not resolved: %+v", a.Steps[0])
	}
	started := base.Add(2 * time.Minute)
	j := h.job("build")
	j.SetupCompletedAt = &started
	if err := h.st.UpdateJob(ctx(), j); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertStep(ctx(), &model.Step{
		JobID: j.ID, Number: 1, Name: "slow", Attempt: j.Attempt,
		Status: model.StatusInProgress, StartedAt: &started,
	}); err != nil {
		t.Fatal(err)
	}
	h.tick(started.Add(3 * time.Minute))

	got := h.job("build")
	if got.Conclusion != model.ConclusionTimedOut {
		t.Fatalf("conclusion %q", got.Conclusion)
	}
	if got.Cancel == nil || !strings.Contains(got.Cancel.Sentence, "step 1") {
		t.Fatalf("cancel reason %+v", got.Cancel)
	}
}

func TestRunTimeoutStopsEverything(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")), func(o *Options) { o.RunTimeout = 10 * time.Minute })
	h.tick(base)
	h.tick(base.Add(11 * time.Minute))
	got := h.job("build")
	if got.Conclusion != model.ConclusionTimedOut || got.Cancel == nil {
		t.Fatalf("job %+v", got)
	}
	if h.runRow().Status != model.StatusCompleted {
		t.Fatal("the run should be closed")
	}
}

func TestCancelRunRecordsAReasonOnEveryJob(t *testing.T) {
	h := newHarness(t, wf(jobIR("a"), jobIR("b")))
	h.tick(base)
	reason := model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "someone pressed cancel on the run page.",
		TriggeredBy: "someone",
	}
	if err := h.s.CancelAt(ctx(), h.run.ID, reason, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, j := range h.jobs() {
		if j.Conclusion != model.ConclusionCancelled {
			t.Fatalf("%s concluded %q", j.Name, j.Conclusion)
		}
		if j.Cancel == nil || j.Cancel.Sentence == "" {
			t.Fatalf("%s cancelled with no sentence", j.Name)
		}
	}
	if got := h.runRow().Conclusion; got != model.ConclusionCancelled {
		t.Fatalf("run concluded %q", got)
	}
}

func TestCancellationWithoutASentenceIsRejected(t *testing.T) {
	h := newHarness(t, wf(jobIR("a")))
	h.tick(base)
	err := h.s.CancelAt(ctx(), h.run.ID, model.CancelReason{Actor: model.CancelActorUser}, base)
	if err == nil || !strings.Contains(err.Error(), "explanation") {
		t.Fatalf("want a missing-sentence error, got %v", err)
	}
	err = h.s.CancelJobAt(ctx(), h.job("a").ID, model.CancelReason{Sentence: "no actor"}, base)
	if err == nil || !strings.Contains(err.Error(), "actor") {
		t.Fatalf("want a missing-actor error, got %v", err)
	}
	if h.job("a").Conclusion != "" {
		t.Fatal("a rejected cancellation must not have changed the job")
	}
}

func TestCancelJobSkipsItsDependents(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	if err := h.s.CancelJobAt(ctx(), h.job("build").ID, model.CancelReason{
		Actor: model.CancelActorUser, Sentence: "cancelled by hand.",
	}, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.tick(base.Add(2 * time.Minute))
	h.requireConclusion("test", model.ConclusionSkipped)
	if got := h.runRow().Conclusion; got != model.ConclusionCancelled {
		t.Fatalf("run concluded %q", got)
	}
}

func TestAssignmentCarriesIdempotencyKeyAndToken(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	j := h.job("build")
	want := "1/" + itoa(j.ID) + "/1"
	if a.IdempotencyKey != want {
		t.Fatalf("idempotency key %q, want %q", a.IdempotencyKey, want)
	}
	if a.JobToken != "tok" || a.ServerURL != "https://ci.example.com" {
		t.Fatalf("assignment %+v", a)
	}
	if a.Retry.Attempts != model.DefaultRetryPolicy().Attempts {
		t.Fatal("the resolved retry policy must travel with the assignment")
	}
	if j.Status != model.StatusInProgress || j.RunnerID != "runner-1" || j.LeaseExpiresAt == nil {
		t.Fatalf("job after dispatch: %+v", j)
	}
	if len(h.st.eventsOfKind(EventDispatched)) != 1 {
		t.Fatal("dispatch must be recorded")
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// A fork pull request is attacker-controlled, so it gets no secrets and no
// OIDC, and the restriction is recorded rather than silent.
func TestForkPullRequestGetsNoSecrets(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	if err := h.st.PutSecret(ctx(), "repo", "wow-look-at-my/ci-platform", "DEPLOY_KEY", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	h.tick(base)

	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if a.Secrets["DEPLOY_KEY"] != "s3cret" {
		t.Fatal("a same-repo run should receive its secrets")
	}

	// Now the same workflow as a fork PR.
	fh := newHarness(t, wf(jobIR("build")))
	fh.run.IsForkPR = true
	if err := fh.st.UpdateRun(ctx(), fh.run); err != nil {
		t.Fatal(err)
	}
	if err := fh.st.PutSecret(ctx(), "repo", "wow-look-at-my/ci-platform", "DEPLOY_KEY", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	fh.tick(base)
	fa, err := fh.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.Secrets) != 0 {
		t.Fatalf("a fork PR received secrets: %v", fa.Secrets)
	}
	if _, ok := fa.Contexts["secrets"]; ok {
		t.Fatal("a fork PR must not get a secrets context either")
	}
	if OIDCAllowed(fh.run) || SecretsAllowed(fh.run) {
		t.Fatal("fork PRs must be refused secrets and OIDC")
	}
	if len(fh.st.eventsOfKind(EventRestricted)) != 1 {
		t.Fatal("the restriction must be recorded, not silent")
	}
}

func TestForkApprovalHoldsJobs(t *testing.T) {
	h := &harness{t: t, st: newFakeStore()}
	h.s = New(h.st, Options{
		NewEval: fakeFactory, ServerURL: "https://ci.example.com",
		MintJobToken:        func(int64, int64, int) (string, error) { return "tok", nil },
		RequireForkApproval: true,
	})
	repo := &model.Repo{ID: 7, Owner: "o", Name: "r", DefaultBranch: "master"}
	if err := h.st.UpsertRepo(ctx(), repo); err != nil {
		t.Fatal(err)
	}
	h.run = &model.Run{ID: 1, RepoID: 7, IsForkPR: true, Status: model.StatusQueued, CreatedAt: base, HeadBranch: "pr"}
	if err := h.st.CreateRun(ctx(), h.run); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(wf(jobIR("build")), plan.Input{Run: h.run, NewEval: fakeFactory})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.StartRun(ctx(), h.run, p); err != nil {
		t.Fatal(err)
	}
	h.tick(base)
	if h.st.queued(h.job("build").ID) != nil {
		t.Fatal("an unapproved fork PR must not dispatch")
	}
	if err := h.s.Approve(ctx(), h.run.ID, "maintainer", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.tick(base.Add(2 * time.Minute))
	if h.st.queued(h.job("build").ID) == nil {
		t.Fatal("approval should release the jobs")
	}
	if err := h.s.Approve(ctx(), h.run.ID, "", base); err == nil {
		t.Fatal("approving without an approver must be rejected")
	}
}

func TestRollupSummaryAndDefaultBranchAlarm(t *testing.T) {
	once := model.RetryPolicy{Attempts: 1, On: []model.FailureClass{model.ClassInfra}}
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("publish", func(j *model.JobIR) { j.Retry = &once }),
	))
	h.run.HeadBranch = "master"
	if err := h.st.UpdateRun(ctx(), h.run); err != nil {
		t.Fatal(err)
	}
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.complete("publish", model.ConclusionInfraFailure, model.ClassInfra, base.Add(2*time.Minute))
	h.tick(base.Add(3 * time.Minute))

	roll, err := h.s.RunRollup(ctx(), h.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if roll.Conclusion != model.ConclusionInfraFailure {
		t.Fatalf("rollup concluded %q", roll.Conclusion)
	}
	if roll.ByClass[model.ClassInfra] != 1 || roll.ByConclusion[model.ConclusionSuccess] != 1 {
		t.Fatalf("counts %+v %+v", roll.ByClass, roll.ByConclusion)
	}
	if !strings.Contains(roll.Summary, "not your code's") {
		t.Fatalf("summary %q", roll.Summary)
	}
	if len(h.notes) != 1 {
		t.Fatalf("want one default-branch notification, got %d", len(h.notes))
	}
	if h.notes[0].Kind != NotifyDefaultBranchNotSuccess || h.notes[0].Branch != "master" {
		t.Fatalf("notification %+v", h.notes[0])
	}
}

func TestNoAlarmOnASuccessfulDefaultBranchRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.run.HeadBranch = "master"
	if err := h.st.UpdateRun(ctx(), h.run); err != nil {
		t.Fatal(err)
	}
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if len(h.notes) != 0 {
		t.Fatalf("a green run raised an alarm: %+v", h.notes)
	}
}

func TestNoAlarmOffTheDefaultBranch(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	if len(h.notes) != 0 {
		t.Fatalf("a feature branch raised the default-branch alarm: %+v", h.notes)
	}
}

func TestRerunFailedResetsFailuresAndDownstream(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
		jobIR("lint"),
	))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.complete("lint", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	h.requireConclusion("test", model.ConclusionSkipped)

	// A finished run's plan is dropped, so a re-run re-registers it.
	if err := h.s.RerunFailed(ctx(), h.run.ID, "someone"); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("want ErrNoPlan before the plan is re-registered, got %v", err)
	}
	if err := h.s.RegisterPlan(h.run.ID, h.plan); err != nil {
		t.Fatal(err)
	}
	if err := h.s.RerunFailed(ctx(), h.run.ID, "someone"); err != nil {
		t.Fatal(err)
	}
	if got := h.job("build"); got.Conclusion != "" || got.Attempt != 2 {
		t.Fatalf("build after re-run: %+v", got)
	}
	if got := h.job("test"); got.Conclusion != "" {
		t.Fatal("a downstream job must be re-run with its dependency")
	}
	if got := h.job("lint"); got.Conclusion != model.ConclusionSuccess || got.Attempt != 1 {
		t.Fatal("a successful, unrelated job must keep its result")
	}
	if h.runRow().Attempt != 2 {
		t.Fatalf("run attempt %d", h.runRow().Attempt)
	}
	h.tick(base.Add(3 * time.Minute))
	if h.st.queued(h.job("build").ID) == nil {
		t.Fatal("the re-run job should be queued again")
	}
}

func TestRerunJobResetsOnlyItAndDownstream(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	h.complete("test", model.ConclusionSuccess, model.ClassNone, base.Add(3*time.Minute))
	h.tick(base.Add(4 * time.Minute))

	if err := h.s.RegisterPlan(h.run.ID, h.plan); err != nil {
		t.Fatal(err)
	}
	if err := h.s.RerunJob(ctx(), h.job("build").ID, "someone"); err != nil {
		t.Fatal(err)
	}
	if h.job("build").Conclusion != "" || h.job("test").Conclusion != "" {
		t.Fatal("re-running a job re-runs what depends on it")
	}
	if err := h.s.RerunFailed(ctx(), h.run.ID, ""); err == nil {
		t.Fatal("a re-run without an actor must be rejected")
	}
}

func TestSchedulerWithoutAPlanFailsLoudly(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	id := h.job("build").ID
	h.s.forgetPlan(h.run.ID)
	err := h.s.JobCompletedAt(ctx(), id, Result{Conclusion: model.ConclusionSuccess}, base)
	if !errors.Is(err, ErrNoPlan) {
		t.Fatalf("want ErrNoPlan, got %v", err)
	}
}

func TestAcquireSkipsAJobCancelledWhileQueued(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	if err := h.s.CancelJobAt(ctx(), h.job("build").ID, model.CancelReason{
		Actor: model.CancelActorUser, Sentence: "cancelled before a runner took it.",
	}, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(2*time.Minute))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want nothing to run, got %v", err)
	}
}

func TestAcquireNeedsARunnerID(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	if _, err := h.s.Acquire(ctx(), "", nil, base); err == nil {
		t.Fatal("want an error for an anonymous acquire")
	}
}

func TestResultValidationRejectsDishonestOutcomes(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	id := h.job("build").ID
	for name, res := range map[string]Result{
		"no conclusion":         {},
		"unknown conclusion":    {Conclusion: model.Conclusion("green-ish")},
		"cancel with no why":    {Conclusion: model.ConclusionCancelled},
		"failure with no class": {Conclusion: model.ConclusionFailure},
	} {
		if err := h.s.JobCompletedAt(ctx(), id, res, base); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestStartRunRejectsAnEmptyPlan(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	err := h.s.StartRun(ctx(), h.run, &plan.Plan{})
	if err == nil || !strings.Contains(err.Error(), "no jobs") {
		t.Fatalf("want a no-jobs error, got %v", err)
	}
	if err := h.s.StartRun(ctx(), nil, h.plan); err == nil {
		t.Fatal("want an error for a nil run")
	}
	if err := h.s.StartRun(ctx(), h.run, nil); err == nil {
		t.Fatal("want an error for a nil plan")
	}
}

func TestNewRejectsMissingWiring(t *testing.T) {
	for name, f := range map[string]func(){
		"no store":     func() { New(nil, Options{NewEval: fakeFactory}) },
		"no evaluator": func() { New(newFakeStore(), Options{}) },
		"no minter":    func() { New(newFakeStore(), Options{NewEval: fakeFactory}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: want a panic naming the missing wiring", name)
				}
			}()
			f()
		}()
	}
}

func TestEnqueueFailureIsNotSwallowed(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.st.failEnqueue = true
	if err := h.s.Tick(ctx(), base); err == nil {
		t.Fatal("a queue write that failed must surface")
	}
}

func TestRunConcurrencySupersedesTheOlderRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)

	// A second run of the same workflow, same group, cancel-in-progress.
	w2 := wf(jobIR("build"))
	w2.Concurrency = &model.Concurrency{Group: model.NewExpr("ci-main"), CancelInProgress: model.NewExpr("true")}
	p1, err := plan.Build(w2, plan.Input{Run: h.run, NewEval: fakeFactory})
	if err != nil {
		t.Fatal(err)
	}
	h.s.registerPlan(h.run.ID, p1)

	run2 := &model.Run{ID: 2, RepoID: 7, Status: model.StatusQueued, CreatedAt: base.Add(time.Minute), HeadBranch: "feature", HeadSHA: "cafe"}
	if err := h.st.CreateRun(ctx(), run2); err != nil {
		t.Fatal(err)
	}
	p2, err := plan.Build(w2, plan.Input{Run: run2, NewEval: fakeFactory})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.StartRun(ctx(), run2, p2); err != nil {
		t.Fatal(err)
	}
	old := h.runRow()
	if old.Conclusion != model.ConclusionCancelled {
		t.Fatalf("the older run concluded %q", old.Conclusion)
	}
	if old.Cancel == nil || old.Cancel.Actor != model.CancelActorSupersededByRun {
		t.Fatalf("cancel reason %+v", old.Cancel)
	}
}

func TestRunConcurrencyWithoutCancelHoldsTheNewerRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	w2 := wf(jobIR("build"))
	w2.Concurrency = &model.Concurrency{Group: model.NewExpr("ci-main")}
	p1, err := plan.Build(w2, plan.Input{Run: h.run, NewEval: fakeFactory})
	if err != nil {
		t.Fatal(err)
	}
	h.s.registerPlan(h.run.ID, p1)
	h.tick(base)

	run2 := &model.Run{ID: 2, RepoID: 7, Status: model.StatusQueued, CreatedAt: base.Add(time.Minute), HeadBranch: "feature"}
	if err := h.st.CreateRun(ctx(), run2); err != nil {
		t.Fatal(err)
	}
	p2, err := plan.Build(w2, plan.Input{Run: run2, NewEval: fakeFactory})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.StartRun(ctx(), run2, p2); err != nil {
		t.Fatal(err)
	}
	h.tick(base.Add(2 * time.Minute))

	jobs, err := h.st.ListJobsForRun(ctx(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if h.st.queued(jobs[0].ID) != nil {
		t.Fatal("the newer run must wait while the older one holds the group")
	}
}

func TestJobSetupCompletedRecordsTheBoundary(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	if _, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	at := base.Add(3 * time.Minute)
	if err := h.s.JobSetupCompleted(ctx(), h.job("build").ID, at); err != nil {
		t.Fatal(err)
	}
	if h.job("build").SetupCompletedAt == nil {
		t.Fatal("setup boundary not recorded")
	}
	ev := h.st.eventsOfKind(EventStarted)
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "setup finished in 2m0s") {
		t.Fatalf("started event %+v", ev)
	}
	// Idempotent.
	if err := h.s.JobSetupCompleted(ctx(), h.job("build").ID, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(h.st.eventsOfKind(EventStarted)) != 1 {
		t.Fatal("a repeated setup report must not write a second event")
	}
}

func TestReleaseJobRequiresAReason(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	id := h.job("build").ID
	if err := h.s.ReleaseJob(ctx(), "runner-1", id, model.CancelReason{Actor: model.CancelActorShutdown}); err == nil {
		t.Fatal("a release with no sentence must be rejected")
	}
	if err := h.s.ReleaseJob(ctx(), "runner-1", id, model.CancelReason{
		Actor: model.CancelActorShutdown, Sentence: "the agent is shutting down, so the job goes back on the queue.",
	}); err != nil {
		t.Fatal(err)
	}
}
