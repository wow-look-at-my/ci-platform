package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func ctx() context.Context { return context.Background() }

func TestStartRunQueuesOnlyRootJobs(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	require.NotNil(t, h.st.queued(h.job("build").ID))

	require.Nil(t, h.st.queued(h.job("test").ID))

	require.Equal(t, 1, len(h.st.eventsOfKind(EventQueued)))

}

func TestTickIsIdempotent(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.tick(base)
	h.tick(base)
	n := len(h.st.eventsOfKind(EventQueued))
	require.Equal(t, 1, n)

}

func TestDependentRunsAfterSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("test").ID))

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
	require.Equal(t, model.ConclusionFailure, h.runRow().Conclusion)

	skips := h.st.eventsOfKind(EventSkipped)
	require.Equal(t, 2, len(skips))

	require.Contains(t, skips[0].Message, "build concluded failure")

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
	require.NotNil(t, h.st.queued(h.job("report").ID))

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
	require.NoError(t, h.st.UpdateJob(ctx(), j))

	require.NoError(t, h.s.JobCompletedAt(ctx(), j.ID, Result{Conclusion: model.ConclusionSuccess, Outputs: map[string]string{"version": "1.2.3"}}, base.Add(time.Minute)))

	h.tick(base.Add(2 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("deploy").ID))

}

// A run where every job was skipped must never report success.
func TestAllSkippedRunIsNotSuccess(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("a", func(j *model.JobIR) { j.If = model.NewExpr("false") }),
		jobIR("b", func(j *model.JobIR) { j.If = model.NewExpr("false") }),
	))
	h.tick(base)
	got := h.runRow()
	require.Equal(t, model.ConclusionSkipped, got.Conclusion)

	roll, err := h.s.RunRollup(ctx(), h.run.ID)
	require.Nil(t, err)

	require.NotContains(t, roll.Summary, "success")

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

	require.NotNil(t, h.st.queued(h.job("after").ID))

	h.requireConclusion("flaky", model.ConclusionFailure)
	h.complete("after", model.ConclusionSuccess, model.ClassNone, base.Add(3*time.Minute))
	h.tick(base.Add(4 * time.Minute))
	got := h.runRow().Conclusion
	require.Equal(t, model.ConclusionFailure, got)

}

func TestInfraFailureRetriesWithBackoff(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.complete("build", model.ConclusionInfraFailure, model.ClassInfra, base.Add(time.Minute))

	j := h.job("build")
	require.NotEqual(t, model.StatusCompleted, j.Status)

	require.False(t, j.Attempt != 2 || j.InfraRetryCount != 1)

	q := h.st.queued(j.ID)
	require.NotNil(t, q)

	want := base.Add(time.Minute).Add(model.DefaultRetryPolicy().Delay(2))
	require.True(t, q.NotBefore.Equal(want))

	require.Equal(t, 1, len(h.st.eventsOfKind(EventRetried)))

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
		require.False(t, j.Status != model.StatusCompleted || j.Attempt != 1)

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
	require.False(t, j.Status != model.StatusCompleted || j.Conclusion != model.ConclusionInfraFailure)

	require.Equal(t, model.DefaultRetryPolicy().Attempts-1, j.InfraRetryCount)

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
	require.NoError(t, h.st.UpdateJob(ctx(), j))

	h.st.reap = []*model.Job{j}
	h.tick(base.Add(5 * time.Minute))

	got := h.job("build")
	require.Equal(t, model.StatusQueued, got.Status)

	require.Equal(t, model.Conclusion(""), got.Conclusion)

	require.False(t, got.RequeueCount != 1 || got.Attempt != 2)

	ev := h.st.eventsOfKind(EventRequeued)
	require.False(t, len(ev) != 1 || !strings.Contains(ev[0].Message, "runner-9"))

	require.Equal(t, string(model.CancelActorRunnerLost), ev[0].Detail["actor"])

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
	require.NotNil(t, h.st.queued(h.job("deploy-a").ID))

	// deploy-b becomes ready while deploy-a holds the group.
	h.complete("gate", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))

	a, b := h.job("deploy-a"), h.job("deploy-b")
	require.Equal(t, model.StatusCompleted, a.Status)

	require.False(t, a.Conclusion != model.ConclusionCancelled || a.Cancel == nil)

	require.Equal(t, model.CancelActorConcurrencyGroup, a.Cancel.Actor)

	require.Contains(t, a.Cancel.Sentence, "deploy")

	require.NotNil(t, h.st.queued(b.ID))

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
	require.Equal(t, model.StatusWaiting, b.Status)

	require.Nil(t, h.st.queued(b.ID))

	ev := h.st.eventsOfKind(EventWaiting)
	require.Equal(t, 1, len(ev))

	// It stays waiting, and does not write an event a tick.
	h.tick(base.Add(time.Minute))
	require.Equal(t, 1, len(h.st.eventsOfKind(EventWaiting)))

	// Once the holder finishes, the waiter is admitted.
	h.complete("deploy-a", model.ConclusionSuccess, model.ClassNone, base.Add(2*time.Minute))
	h.tick(base.Add(3 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("deploy-b").ID))

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
		require.Equal(t, model.ConclusionCancelled, j.Conclusion)

		require.False(t, j.Cancel == nil || !strings.Contains(j.Cancel.Sentence, "fail-fast"))

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
	got := h.job("test (windows)").Conclusion
	require.Equal(t, model.Conclusion(""), got)

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
	require.Nil(t, h.st.queued(h.job("test (windows)").ID))

	h.complete("test (ubuntu)", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("test (windows)").ID))

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
	require.NoError(t, h.st.UpdateJob(ctx(), j))

	h.tick(started.Add(6 * time.Minute))

	got := h.job("build")
	require.Equal(t, model.ConclusionTimedOut, got.Conclusion)

	require.False(t, got.Cancel == nil || got.Cancel.Actor != model.CancelActorTimeout)

	require.Equal(t, 1, got.Attempt)

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
	require.NoError(t, h.st.UpdateJob(ctx(), j))

	h.tick(started.Add(3 * time.Minute))

	got := h.job("build")
	require.NotEqual(t, model.StatusCompleted, got.Status)

	require.False(t, got.Attempt != 2 || got.InfraRetryCount != 1)

	ev := h.st.eventsOfKind(EventRetried)
	require.False(t, len(ev) != 1 || ev[0].Detail["class"] != string(model.ClassInfra))

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
	require.Nil(t, err)

	require.Equal(t, 2, a.Steps[0].TimeoutMinutes)

	started := base.Add(2 * time.Minute)
	j := h.job("build")
	j.SetupCompletedAt = &started
	require.NoError(t, h.st.UpdateJob(ctx(), j))

	require.NoError(t, h.st.UpsertStep(ctx(), &model.Step{JobID: j.ID, Number: 1, Name: "slow", Attempt: j.Attempt, Status: model.StatusInProgress, StartedAt: &started}))

	h.tick(started.Add(3 * time.Minute))

	got := h.job("build")
	require.Equal(t, model.ConclusionTimedOut, got.Conclusion)

	require.False(t, got.Cancel == nil || !strings.Contains(got.Cancel.Sentence, "step 1"))

}

func TestRunTimeoutStopsEverything(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")), func(o *Options) { o.RunTimeout = 10 * time.Minute })
	h.tick(base)
	h.tick(base.Add(11 * time.Minute))
	got := h.job("build")
	require.False(t, got.Conclusion != model.ConclusionTimedOut || got.Cancel == nil)

	require.Equal(t, model.StatusCompleted, h.runRow().Status)

}
