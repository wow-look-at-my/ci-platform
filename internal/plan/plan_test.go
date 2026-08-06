package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func job(key string, mods ...func(*model.JobIR)) *model.JobIR {
	j := &model.JobIR{
		Key:    key,
		RunsOn: model.RunsOn{Labels: []model.Expr{model.NewExpr("ubuntu-latest")}},
	}
	for _, m := range mods {
		m(j)
	}
	return j
}

func workflow(jobs ...*model.JobIR) *model.Workflow {
	w := &model.Workflow{Jobs: map[string]*model.JobIR{}}
	for _, j := range jobs {
		w.Jobs[j.Key] = j
		w.JobOrder = append(w.JobOrder, j.Key)
	}
	return w
}

func build(t *testing.T, w *model.Workflow, canned map[string]any) *Plan {
	t.Helper()
	p, err := Build(w, Input{
		Run:      &model.Run{ID: 1},
		Contexts: map[string]any{"github": map[string]any{"ref": "refs/heads/main"}},
		NewEval:  newFakeFactory(canned),
	})
	require.Nil(t, err)

	return p
}

func names(p *Plan) []string {
	out := make([]string, 0, len(p.Jobs))
	for _, j := range p.Jobs {
		out = append(out, j.Name)
	}
	return out
}

func TestUnmatrixedNaming(t *testing.T) {
	p := build(t, workflow(
		job("build"),
		job("test", func(j *model.JobIR) { j.Name = model.NewExpr("Unit tests on ${{ github.ref }}") }),
	), nil)
	assertStrings(t, names(p), []string{"build", "Unit tests on refs/heads/main"})
}

// The name the operator cares about: an include-only matrix whose leg suffix
// must come out in source order.
func TestOperatorPublishName(t *testing.T) {
	p := build(t, workflow(job("publish", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Order: []string{"image", "dockerfile"},
			Include: []map[string]any{
				{"image": "claude-host/agent-host", "dockerfile": "Dockerfile"},
			},
		}}
	})), nil)
	assertStrings(t, names(p), []string{"publish (claude-host/agent-host, Dockerfile)"})
	require.Equal(t, "image=claude-host/agent-host,dockerfile=Dockerfile", p.Jobs[0].MatrixKey)

}

func TestExplicitNameOnAMatrixedJobGetsNoSuffix(t *testing.T) {
	p := build(t, workflow(job("test", func(j *model.JobIR) {
		j.Name = model.NewExpr("test / ${{ matrix.os }}")
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
			Order:      []string{"os"},
		}}
	})), nil)
	assertStrings(t, names(p), []string{"test / ubuntu", "test / windows"})
}

// Cross-checked against nektos/act's "strategy-all" expectation.
func TestBuildMatchesActStrategyAllCase(t *testing.T) {
	p := build(t, workflow(job("b", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{
				"datacenter":   {"site-c", "site-d"},
				"node-version": {"14.x", "16.x"},
				"site":         {"staging"},
			},
			Order:   []string{"datacenter", "node-version", "site"},
			Exclude: []map[string]any{{"datacenter": "site-d", "node-version": "14.x", "site": "staging"}},
			Include: []map[string]any{
				{"php-version": 5.4},
				{"datacenter": "site-a", "node-version": "10.x", "site": "prod"},
				{"datacenter": "site-b", "node-version": "12.x", "site": "dev"},
			},
		}}
	})), nil)
	assertStrings(t, names(p), []string{
		"b (site-c, 14.x, staging)",
		"b (site-c, 16.x, staging)",
		"b (site-d, 16.x, staging)",
		"b (site-a, 10.x, prod)",
		"b (site-b, 12.x, dev)",
	})
	got := p.Jobs[0].Matrix["php-version"]
	require.Equal(t, 5.4, got)

}

func TestMatrixSiblingsAndOrder(t *testing.T) {
	p := build(t, workflow(
		job("a"),
		job("b", func(j *model.JobIR) {
			j.Needs = []string{"a"}
			j.Strategy = &model.Strategy{Matrix: &model.Matrix{
				Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
				Order:      []string{"os"},
			}}
		}),
	), nil)
	assertStrings(t, p.Order, []string{"a", "b#os=ubuntu", "b#os=windows"})
	require.Nil(t, p.Jobs[0].MatrixSiblings)

	assertStrings(t, p.Jobs[1].MatrixSiblings, []string{"b#os=ubuntu", "b#os=windows"})
	assertStrings(t, p.Jobs[2].MatrixSiblings, []string{"b#os=ubuntu", "b#os=windows"})
	require.False(t, p.ByID("b#os=windows") != p.Jobs[2] || p.Find("b", "os=ubuntu") != p.Jobs[1])

	require.Equal(t, 2, len(p.Legs("b")))

}

func TestTopologicalOrderFollowsNeeds(t *testing.T) {
	p := build(t, workflow(
		job("deploy", func(j *model.JobIR) { j.Needs = []string{"test", "build"} }),
		job("test", func(j *model.JobIR) { j.Needs = []string{"build"} }),
		job("build"),
	), nil)
	assertStrings(t, p.Order, []string{"build", "test", "deploy"})
}

func TestCycleIsRejected(t *testing.T) {
	w := workflow(
		job("a", func(j *model.JobIR) { j.Needs = []string{"b"} }),
		job("b", func(j *model.JobIR) { j.Needs = []string{"a"} }),
	)
	_, err := Build(w, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "cycle"))

}

func TestSelfNeedAndUnknownNeedAreRejected(t *testing.T) {
	_, err := Build(workflow(job("a", func(j *model.JobIR) { j.Needs = []string{"a"} })),
		Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "needs itself"))

	_, err = Build(workflow(job("a", func(j *model.JobIR) { j.Needs = []string{"ghost"} })),
		Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "does not define"))

}

func TestJobOrderMustCoverEveryJob(t *testing.T) {
	w := workflow(job("a"), job("b"))
	w.JobOrder = []string{"a"}
	_, err := Build(w, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "job order"))

}

func TestRetryPolicyResolution(t *testing.T) {
	custom := model.RetryPolicy{Attempts: 5, On: []model.FailureClass{model.ClassInfra}, Backoff: model.BackoffFixed, Initial: time.Second}
	p := build(t, workflow(
		job("a"),
		job("b", func(j *model.JobIR) { j.Retry = &custom }),
	), nil)
	require.Equal(t, model.DefaultRetryPolicy().Attempts, p.Jobs[0].Retry.Attempts)

	require.False(t, p.Jobs[1].Retry.Attempts != 5 || p.Jobs[1].Retry.Backoff != model.BackoffFixed)

}

func TestZeroAttemptRetryPolicyIsRejected(t *testing.T) {
	bad := model.RetryPolicy{Attempts: 0}
	_, err := Build(workflow(job("a", func(j *model.JobIR) { j.Retry = &bad })),
		Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "attempts"))

}

func TestFailFastDefaultsToTrue(t *testing.T) {
	no := false
	p := build(t, workflow(
		job("a", func(j *model.JobIR) { j.Strategy = &model.Strategy{} }),
		job("b", func(j *model.JobIR) { j.Strategy = &model.Strategy{FailFast: &no} }),
		job("c"),
	), nil)
	require.True(t, p.Jobs[0].FailFast)

	require.False(t, p.Jobs[1].FailFast)

	require.True(t, p.Jobs[2].FailFast)

}

func TestMaxParallelAndConcurrencyAreEvaluated(t *testing.T) {
	p := build(t, workflow(job("a", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{
			MaxParallel: model.NewExpr("${{ vars.parallel }}"),
			Matrix:      &model.Matrix{Dimensions: map[string][]any{"n": {1, 2, 3}}, Order: []string{"n"}},
		}
		j.Concurrency = &model.Concurrency{
			Group:            model.NewExpr("deploy-${{ github.ref }}"),
			CancelInProgress: model.NewExpr("true"),
		}
	})), map[string]any{"vars.parallel": 2})
	require.Equal(t, 2, p.Jobs[0].MaxParallel)

	require.False(t, p.Jobs[0].ConcurrencyGroup != "deploy-refs/heads/main" || !p.Jobs[0].CancelInProgress)

}

func TestWorkflowConcurrencyIsResolved(t *testing.T) {
	w := workflow(job("a"))
	w.Concurrency = &model.Concurrency{
		Group:            model.NewExpr("ci-${{ github.ref }}"),
		CancelInProgress: model.NewExpr("true"),
	}
	p := build(t, w, nil)
	require.False(t, p.RunConcurrencyGroup != "ci-refs/heads/main" || !p.RunCancelInProgress)

}

func TestRunsOnMustSelectSomething(t *testing.T) {
	w := workflow(job("a", func(j *model.JobIR) { j.RunsOn = model.RunsOn{} }))
	_, err := Build(w, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "runs-on"))

}

func TestRunnerGroupIsKept(t *testing.T) {
	p := build(t, workflow(job("a", func(j *model.JobIR) {
		j.RunsOn = model.RunsOn{Group: model.NewExpr("big-boxes")}
	})), nil)
	require.False(t, p.Jobs[0].RunnerGroup != "big-boxes" || len(p.Jobs[0].Labels) != 0)

}

func TestContinueOnErrorTimeoutAndEnvironment(t *testing.T) {
	p := build(t, workflow(job("a", func(j *model.JobIR) {
		j.ContinueOnError = model.NewExpr("true")
		j.TimeoutMinutes = model.NewExpr("42")
		j.Environment = &model.Environment{Name: model.NewExpr("production")}
	})), nil)
	got := p.Jobs[0]
	require.False(t, !got.ContinueOnError || got.TimeoutMinutes != 42 || got.Environment != "production")

}

func TestBadBooleanIsAnErrorNotADefault(t *testing.T) {
	w := workflow(job("a", func(j *model.JobIR) { j.ContinueOnError = model.NewExpr("ture") }))
	_, err := Build(w, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "true or false"))

}

func TestBuildRejectsMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    *model.Workflow
		in   Input
	}{
		{"no workflow", nil, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)}},
		{"no run", workflow(), Input{NewEval: newFakeFactory(nil)}},
		{"no evaluator", workflow(), Input{Run: &model.Run{}}},
	} {
		_, err := Build(tc.w, tc.in)
		require.NotNil(t, err)

	}
}

func TestFullyExcludedMatrixIsAnError(t *testing.T) {
	w := workflow(job("a", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu"}},
			Order:      []string{"os"},
			Exclude:    []map[string]any{{"os": "ubuntu"}},
		}}
	}))
	_, err := Build(w, Input{Run: &model.Run{}, NewEval: newFakeFactory(nil)})
	require.False(t, err == nil || !strings.Contains(err.Error(), "zero combinations"))

}

func TestStrategyContextIsExposedToExpressions(t *testing.T) {
	p := build(t, workflow(job("a", func(j *model.JobIR) {
		j.Name = model.NewExpr("leg ${{ strategy.job-index }} of ${{ strategy.job-total }}")
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"n": {1, 2}}, Order: []string{"n"},
		}}
	})), nil)
	assertStrings(t, names(p), []string{"leg 0 of 2", "leg 1 of 2"})
}

func TestIncludeKeyWithNoRecordedOrderIsSurfacedAsADeviation(t *testing.T) {
	w := workflow(job("publish", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu"}},
			Order:      []string{"os"},
			Include:    []map[string]any{{"os": "ubuntu", "artifact": "tar.gz"}},
		}}
	}))
	build(t, w, nil)
	require.Equal(t, 1, len(w.Deviations))

	d := w.Deviations[0]
	require.False(t, d.Path != "jobs.publish.strategy.matrix.include" || !strings.Contains(d.OurBehavior, "artifact"))

	// With the order recorded, there is nothing to surface.
	w2 := workflow(job("publish", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu"}},
			Order:      []string{"os", "artifact"},
			Include:    []map[string]any{{"os": "ubuntu", "artifact": "tar.gz"}},
		}}
	}))
	build(t, w2, nil)
	require.Equal(t, 0, len(w2.Deviations))

}
