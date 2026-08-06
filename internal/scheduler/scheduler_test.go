package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

	require.Equal(t, "", got.Conclusion)

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
	require.Equal(t, "", got)

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

func TestCancelRunRecordsAReasonOnEveryJob(t *testing.T) {
	h := newHarness(t, wf(jobIR("a"), jobIR("b")))
	h.tick(base)
	reason := model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "someone pressed cancel on the run page.",
		TriggeredBy: "someone",
	}
	require.NoError(t, h.s.CancelAt(ctx(), h.run.ID, reason, base.Add(time.Minute)))

	for _, j := range h.jobs() {
		require.Equal(t, model.ConclusionCancelled, j.Conclusion)

		require.False(t, j.Cancel == nil || j.Cancel.Sentence == "")

	}
	got := h.runRow().Conclusion
	require.Equal(t, model.ConclusionCancelled, got)

}

func TestCancellationWithoutASentenceIsRejected(t *testing.T) {
	h := newHarness(t, wf(jobIR("a")))
	h.tick(base)
	err := h.s.CancelAt(ctx(), h.run.ID, model.CancelReason{Actor: model.CancelActorUser}, base)
	require.False(t, err == nil || !strings.Contains(err.Error(), "explanation"))

	err = h.s.CancelJobAt(ctx(), h.job("a").ID, model.CancelReason{Sentence: "no actor"}, base)
	require.False(t, err == nil || !strings.Contains(err.Error(), "actor"))

	require.Equal(t, "", h.job("a").Conclusion)

}

func TestCancelJobSkipsItsDependents(t *testing.T) {
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
	))
	h.tick(base)
	require.NoError(t, h.s.CancelJobAt(ctx(), h.job("build").ID, model.CancelReason{Actor: model.CancelActorUser, Sentence: "cancelled by hand."}, base.Add(time.Minute)))

	h.tick(base.Add(2 * time.Minute))
	h.requireConclusion("test", model.ConclusionSkipped)
	got := h.runRow().Conclusion
	require.Equal(t, model.ConclusionCancelled, got)

}

func TestAssignmentCarriesIdempotencyKeyAndToken(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	require.Nil(t, err)

	j := h.job("build")
	want := "1/" + itoa(j.ID) + "/1"
	require.Equal(t, want, a.IdempotencyKey)

	require.False(t, a.JobToken != "tok" || a.ServerURL != "https://ci.example.com")

	require.Equal(t, model.DefaultRetryPolicy().Attempts, a.Retry.Attempts)

	require.False(t, j.Status != model.StatusInProgress || j.RunnerID != "runner-1" || j.LeaseExpiresAt == nil)

	require.Equal(t, 1, len(h.st.eventsOfKind(EventDispatched)))

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
	require.NoError(t, h.st.PutSecret(ctx(), "repo", "wow-look-at-my/ci-platform", "DEPLOY_KEY", []byte("s3cret")))

	h.tick(base)

	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	require.Nil(t, err)

	require.Equal(t, "s3cret", a.Secrets["DEPLOY_KEY"])

	// Now the same workflow as a fork PR.
	fh := newHarness(t, wf(jobIR("build")))
	fh.run.IsForkPR = true
	require.NoError(t, fh.st.UpdateRun(ctx(), fh.run))

	require.NoError(t, fh.st.PutSecret(ctx(), "repo", "wow-look-at-my/ci-platform", "DEPLOY_KEY", []byte("s3cret")))

	fh.tick(base)
	fa, err := fh.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	require.Nil(t, err)

	require.Equal(t, 0, len(fa.Secrets))

	_, ok := fa.Contexts["secrets"]
	require.False(t, ok)

	require.False(t, OIDCAllowed(fh.run) || SecretsAllowed(fh.run))

	require.Equal(t, 1, len(fh.st.eventsOfKind(EventRestricted)))

}

func TestForkApprovalHoldsJobs(t *testing.T) {
	h := &harness{t: t, st: newFakeStore()}
	h.s = New(h.st, Options{
		NewEval: fakeFactory, ServerURL: "https://ci.example.com",
		MintJobToken:        func(int64, int64, int) (string, error) { return "tok", nil },
		RequireForkApproval: true,
	})
	repo := &model.Repo{ID: 7, Owner: "o", Name: "r", DefaultBranch: "master"}
	require.NoError(t, h.st.UpsertRepo(ctx(), repo))

	h.run = &model.Run{ID: 1, RepoID: 7, IsForkPR: true, Status: model.StatusQueued, CreatedAt: base, HeadBranch: "pr"}
	require.NoError(t, h.st.CreateRun(ctx(), h.run))

	p, err := plan.Build(wf(jobIR("build")), plan.Input{Run: h.run, NewEval: fakeFactory})
	require.Nil(t, err)

	require.NoError(t, h.s.StartRun(ctx(), h.run, p))

	h.tick(base)
	require.Nil(t, h.st.queued(h.job("build").ID))

	require.NoError(t, h.s.Approve(ctx(), h.run.ID, "maintainer", base.Add(time.Minute)))

	h.tick(base.Add(2 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("build").ID))

	err = h.s.Approve(ctx(), h.run.ID, "", base)
	require.NotNil(t, err)

}

func TestRollupSummaryAndDefaultBranchAlarm(t *testing.T) {
	once := model.RetryPolicy{Attempts: 1, On: []model.FailureClass{model.ClassInfra}}
	h := newHarness(t, wf(
		jobIR("build"),
		jobIR("publish", func(j *model.JobIR) { j.Retry = &once }),
	))
	h.run.HeadBranch = "master"
	require.NoError(t, h.st.UpdateRun(ctx(), h.run))

	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.complete("publish", model.ConclusionInfraFailure, model.ClassInfra, base.Add(2*time.Minute))
	h.tick(base.Add(3 * time.Minute))

	roll, err := h.s.RunRollup(ctx(), h.run.ID)
	require.Nil(t, err)

	require.Equal(t, model.ConclusionInfraFailure, roll.Conclusion)

	require.False(t, roll.ByClass[model.ClassInfra] != 1 || roll.ByConclusion[model.ConclusionSuccess] != 1)

	require.Contains(t, roll.Summary, "not your code's")

	require.Equal(t, 1, len(h.notes))

	require.False(t, h.notes[0].Kind != NotifyDefaultBranchNotSuccess || h.notes[0].Branch != "master")

}

func TestNoAlarmOnASuccessfulDefaultBranchRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.run.HeadBranch = "master"
	require.NoError(t, h.st.UpdateRun(ctx(), h.run))

	h.tick(base)
	h.complete("build", model.ConclusionSuccess, model.ClassNone, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	require.Equal(t, 0, len(h.notes))

}

func TestNoAlarmOffTheDefaultBranch(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	h.complete("build", model.ConclusionFailure, model.ClassUser, base.Add(time.Minute))
	h.tick(base.Add(2 * time.Minute))
	require.Equal(t, 0, len(h.notes))

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
	err := h.s.RerunFailed(ctx(), h.run.ID, "someone")
	require.True(t, errors.Is(err, ErrNoPlan))

	require.NoError(t, h.s.RegisterPlan(h.run.ID, h.plan))

	require.NoError(t, h.s.RerunFailed(ctx(), h.run.ID, "someone"))

	got := h.job("build")
	require.False(t, got.Conclusion != "" || got.Attempt != 2)

	got = h.job("test")
	require.Equal(t, "", got.Conclusion)

	got = h.job("lint")
	require.False(t, got.Conclusion != model.ConclusionSuccess || got.Attempt != 1)

	require.Equal(t, 2, h.runRow().Attempt)

	h.tick(base.Add(3 * time.Minute))
	require.NotNil(t, h.st.queued(h.job("build").ID))

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

	require.NoError(t, h.s.RegisterPlan(h.run.ID, h.plan))

	require.NoError(t, h.s.RerunJob(ctx(), h.job("build").ID, "someone"))

	require.False(t, h.job("build").Conclusion != "" || h.job("test").Conclusion != "")

	err := h.s.RerunFailed(ctx(), h.run.ID, "")
	require.NotNil(t, err)

}

func TestSchedulerWithoutAPlanFailsLoudly(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	id := h.job("build").ID
	h.s.forgetPlan(h.run.ID)
	err := h.s.JobCompletedAt(ctx(), id, Result{Conclusion: model.ConclusionSuccess}, base)
	require.True(t, errors.Is(err, ErrNoPlan))

}

func TestAcquireSkipsAJobCancelledWhileQueued(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	require.NoError(t, h.s.CancelJobAt(ctx(), h.job("build").ID, model.CancelReason{Actor: model.CancelActorUser, Sentence: "cancelled before a runner took it."}, base.Add(time.Minute)))

	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(2*time.Minute))
	require.True(t, errors.Is(err, store.ErrNotFound))

}

func TestAcquireNeedsARunnerID(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	_, err := h.s.Acquire(ctx(), "", nil, base)
	require.NotNil(t, err)

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
		t.Run(name, func(t *testing.T) {
			require.Error(t, h.s.JobCompletedAt(ctx(), id, res, base),
				"a result the platform cannot honestly report must be refused")
		})
	}
}

func TestStartRunRejectsAnEmptyPlan(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	err := h.s.StartRun(ctx(), h.run, &plan.Plan{})
	require.False(t, err == nil || !strings.Contains(err.Error(), "no jobs"))

	err = h.s.StartRun(ctx(), nil, h.plan)
	require.NotNil(t, err)

	err = h.s.StartRun(ctx(), h.run, nil)
	require.NotNil(t, err)

}

func TestNewRejectsMissingWiring(t *testing.T) {
	for name, f := range map[string]func(){
		"no store":     func() { New(nil, Options{NewEval: fakeFactory}) },
		"no evaluator": func() { New(newFakeStore(), Options{}) },
		"no minter":    func() { New(newFakeStore(), Options{NewEval: fakeFactory}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				require.NotNil(t, recover(), "missing wiring must panic at construction, not fail silently later")
			}()
			f()
		})
	}
}

func TestEnqueueFailureIsNotSwallowed(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.st.failEnqueue = true
	err := h.s.Tick(ctx(), base)
	require.NotNil(t, err)

}

func TestRunConcurrencySupersedesTheOlderRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)

	// A second run of the same workflow, same group, cancel-in-progress.
	w2 := wf(jobIR("build"))
	w2.Concurrency = &model.Concurrency{Group: model.NewExpr("ci-main"), CancelInProgress: model.NewExpr("true")}
	p1, err := plan.Build(w2, plan.Input{Run: h.run, NewEval: fakeFactory})
	require.Nil(t, err)

	h.s.registerPlan(h.run.ID, p1)

	run2 := &model.Run{ID: 2, RepoID: 7, Status: model.StatusQueued, CreatedAt: base.Add(time.Minute), HeadBranch: "feature", HeadSHA: "cafe"}
	require.NoError(t, h.st.CreateRun(ctx(), run2))

	p2, err := plan.Build(w2, plan.Input{Run: run2, NewEval: fakeFactory})
	require.Nil(t, err)

	require.NoError(t, h.s.StartRun(ctx(), run2, p2))

	old := h.runRow()
	require.Equal(t, model.ConclusionCancelled, old.Conclusion)

	require.False(t, old.Cancel == nil || old.Cancel.Actor != model.CancelActorSupersededByRun)

}

func TestRunConcurrencyWithoutCancelHoldsTheNewerRun(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	w2 := wf(jobIR("build"))
	w2.Concurrency = &model.Concurrency{Group: model.NewExpr("ci-main")}
	p1, err := plan.Build(w2, plan.Input{Run: h.run, NewEval: fakeFactory})
	require.Nil(t, err)

	h.s.registerPlan(h.run.ID, p1)
	h.tick(base)

	run2 := &model.Run{ID: 2, RepoID: 7, Status: model.StatusQueued, CreatedAt: base.Add(time.Minute), HeadBranch: "feature"}
	require.NoError(t, h.st.CreateRun(ctx(), run2))

	p2, err := plan.Build(w2, plan.Input{Run: run2, NewEval: fakeFactory})
	require.Nil(t, err)

	require.NoError(t, h.s.StartRun(ctx(), run2, p2))

	h.tick(base.Add(2 * time.Minute))

	jobs, err := h.st.ListJobsForRun(ctx(), 2)
	require.Nil(t, err)

	require.Nil(t, h.st.queued(jobs[0].ID))

}

func TestJobSetupCompletedRecordsTheBoundary(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	require.Nil(t, err)

	at := base.Add(3 * time.Minute)
	require.NoError(t, h.s.JobSetupCompleted(ctx(), h.job("build").ID, at))

	require.NotNil(t, h.job("build").SetupCompletedAt)

	ev := h.st.eventsOfKind(EventStarted)
	require.False(t, len(ev) != 1 || !strings.Contains(ev[0].Message, "setup finished in 2m0s"))

	// Idempotent.
	require.NoError(t, h.s.JobSetupCompleted(ctx(), h.job("build").ID, at.Add(time.Minute)))

	require.Equal(t, 1, len(h.st.eventsOfKind(EventStarted)))

}

func TestReleaseJobRequiresAReason(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")))
	h.tick(base)
	id := h.job("build").ID
	err := h.s.ReleaseJob(ctx(), "runner-1", id, model.CancelReason{Actor: model.CancelActorShutdown})
	require.NotNil(t, err)

	require.NoError(t, h.s.ReleaseJob(ctx(), "runner-1", id, model.CancelReason{Actor: model.CancelActorShutdown, Sentence: "the agent is shutting down, so the job goes back on the queue."}))

}
