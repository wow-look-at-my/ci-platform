package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func TestAssignmentResolvesEverythingTheRunnerNeeds(t *testing.T) {
	w := wf(jobIR("build", func(j *model.JobIR) {
		j.Env = map[string]model.Expr{"SCOPE": model.NewExpr("job"), "REF": model.NewExpr("${{ github.ref }}")}
		j.Defaults = model.Defaults{Shell: "bash"}
		j.Container = &model.ContainerSpec{
			Image:   model.NewExpr("golang:1.24"),
			Env:     map[string]model.Expr{"CGO_ENABLED": model.NewExpr("0")},
			Ports:   []model.Expr{model.NewExpr("8080:8080")},
			Volumes: []model.Expr{model.NewExpr("/cache:/cache")},
			Options: model.NewExpr("--cpus 2"),
		}
		j.Services = map[string]*model.ContainerSpec{
			"db": {Image: model.NewExpr("postgres:16")},
		}
		j.Steps = []*model.StepIR{
			{Number: 1, Uses: "actions/checkout@v4", With: map[string]model.Expr{"ref": model.NewExpr("${{ github.ref }}")}},
			{Number: 2, Run: model.NewExpr("make test"), If: model.NewExpr("success()"),
				Env: map[string]model.Expr{"V": model.NewExpr("1")}, ContinueOnError: model.NewExpr("true")},
		}
	}))
	w.Env = map[string]model.Expr{"SCOPE": model.NewExpr("workflow"), "GLOBAL": model.NewExpr("yes")}
	w.Defaults = model.Defaults{WorkingDirectory: "/src"}

	h := newHarness(t, w)
	h.tick(base)
	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base.Add(time.Minute))
	require.Nil(t, err)

	require.False(t, a.Env["SCOPE"] != "job" || a.Env["GLOBAL"] != "yes" || a.Env["REF"] != "refs/heads/feature")

	require.False(t, a.DefaultShell != "bash" || a.WorkingDirectory != "/src")

	require.False(t, a.Container == nil || a.Container.Image != "golang:1.24" || a.Container.Options != "--cpus 2")

	require.False(t, a.Container.Ports[0] != "8080:8080" || a.Container.Volumes[0] != "/cache:/cache")

	require.Equal(t, "postgres:16", a.Services["db"].Image)

	require.Equal(t, "actions/checkout@v4", a.Steps[0].Name)

	require.Equal(t, "refs/heads/feature", a.Steps[0].With["ref"])

	require.False(t, a.Steps[1].Name != "Run step 2" || !a.Steps[1].ContinueOnError)

	// if: must survive unevaluated: it depends on earlier steps.
	require.Equal(t, "success()", a.Steps[1].IfExpr)

	require.Equal(t, int(DefaultJobTimeout/time.Minute), a.TimeoutMinutes)

	needs, ok := a.Contexts["needs"].(map[string]any)
	require.False(t, !ok || len(needs) != 0)

}

func TestAssignmentFailsLoudlyOnBadWiring(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")), func(o *Options) { o.ServerURL = "" })
	h.tick(base)
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	require.False(t, err == nil || !strings.Contains(err.Error(), "server URL"))

	h2 := newHarness(t, wf(jobIR("build")), func(o *Options) {
		o.MintJobToken = func(int64, int64, int) (string, error) { return "", nil }
	})
	h2.tick(base)
	_, err = h2.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	require.False(t, err == nil || !strings.Contains(err.Error(), "empty token"))

	h3 := newHarness(t, wf(jobIR("build")), func(o *Options) {
		o.MintJobToken = func(int64, int64, int) (string, error) { return "", errors.New("kms down") }
	})
	h3.tick(base)
	_, err = h3.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	require.NotNil(t, err)

}

func TestBadContainerImageIsAnError(t *testing.T) {
	h := newHarness(t, wf(jobIR("build", func(j *model.JobIR) {
		j.Container = &model.ContainerSpec{Image: model.NewExpr("")}
	})))
	h.tick(base)
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	require.False(t, err == nil || !strings.Contains(err.Error(), "empty string"))

}

func TestMatrixLegSeesItsOwnMatrixContext(t *testing.T) {
	h := newHarness(t, wf(jobIR("test", func(j *model.JobIR) {
		j.Strategy = &model.Strategy{Matrix: &model.Matrix{
			Dimensions: map[string][]any{"os": {"ubuntu"}},
			Order:      []string{"os"},
		}}
		j.Steps = []*model.StepIR{{Number: 1, Run: model.NewExpr("build for ${{ matrix.os }}")}}
	})))
	h.tick(base)
	a, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	require.Nil(t, err)

	require.Equal(t, "build for ubuntu", a.Steps[0].Run)

	require.Equal(t, "test (ubuntu)", a.JobName)

}

// The default-clock wrappers exist for production callers; prove they reach
// the same funnel.
func TestDefaultClockWrappers(t *testing.T) {
	h := newHarness(t, wf(jobIR("a"), jobIR("b")))
	h.tick(base)
	require.NoError(t, h.s.JobCompleted(ctx(), h.job("a").ID, Result{Conclusion: model.ConclusionSuccess}))

	h.requireConclusion("a", model.ConclusionSuccess)
	require.NoError(t, h.s.CancelJob(ctx(), h.job("b").ID, model.CancelReason{Actor: model.CancelActorUser, Sentence: "stopped by hand."}))

	h.requireConclusion("b", model.ConclusionCancelled)
	require.NoError(t, h.s.Cancel(ctx(), h.run.ID, model.CancelReason{Actor: model.CancelActorUser, Sentence: "stopped the whole run."}))

}
