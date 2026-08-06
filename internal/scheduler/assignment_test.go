package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatal(err)
	}

	if a.Env["SCOPE"] != "job" || a.Env["GLOBAL"] != "yes" || a.Env["REF"] != "refs/heads/feature" {
		t.Fatalf("env %v", a.Env)
	}
	if a.DefaultShell != "bash" || a.WorkingDirectory != "/src" {
		t.Fatalf("defaults %q %q", a.DefaultShell, a.WorkingDirectory)
	}
	if a.Container == nil || a.Container.Image != "golang:1.24" || a.Container.Options != "--cpus 2" {
		t.Fatalf("container %+v", a.Container)
	}
	if a.Container.Ports[0] != "8080:8080" || a.Container.Volumes[0] != "/cache:/cache" {
		t.Fatalf("container ports/volumes %+v", a.Container)
	}
	if a.Services["db"].Image != "postgres:16" {
		t.Fatalf("services %+v", a.Services)
	}
	if a.Steps[0].Name != "actions/checkout@v4" {
		t.Fatalf("a step with no name falls back to its uses: %q", a.Steps[0].Name)
	}
	if a.Steps[0].With["ref"] != "refs/heads/feature" {
		t.Fatalf("step with: %v", a.Steps[0].With)
	}
	if a.Steps[1].Name != "Run step 2" || !a.Steps[1].ContinueOnError {
		t.Fatalf("step 2 %+v", a.Steps[1])
	}
	// if: must survive unevaluated: it depends on earlier steps.
	if a.Steps[1].IfExpr != "success()" {
		t.Fatalf("step if was evaluated away: %q", a.Steps[1].IfExpr)
	}
	if a.TimeoutMinutes != int(DefaultJobTimeout/time.Minute) {
		t.Fatalf("timeout %d", a.TimeoutMinutes)
	}
	needs, ok := a.Contexts["needs"].(map[string]any)
	if !ok || len(needs) != 0 {
		t.Fatalf("needs context %v", a.Contexts["needs"])
	}
}

func TestAssignmentFailsLoudlyOnBadWiring(t *testing.T) {
	h := newHarness(t, wf(jobIR("build")), func(o *Options) { o.ServerURL = "" })
	h.tick(base)
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	if err == nil || !strings.Contains(err.Error(), "server URL") {
		t.Fatalf("want a server URL error, got %v", err)
	}

	h2 := newHarness(t, wf(jobIR("build")), func(o *Options) {
		o.MintJobToken = func(int64, int64, int) (string, error) { return "", nil }
	})
	h2.tick(base)
	_, err = h2.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("want an empty-token error, got %v", err)
	}

	h3 := newHarness(t, wf(jobIR("build")), func(o *Options) {
		o.MintJobToken = func(int64, int64, int) (string, error) { return "", errors.New("kms down") }
	})
	h3.tick(base)
	if _, err := h3.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base); err == nil {
		t.Fatal("a token minting failure must not produce an assignment")
	}
}

func TestBadContainerImageIsAnError(t *testing.T) {
	h := newHarness(t, wf(jobIR("build", func(j *model.JobIR) {
		j.Container = &model.ContainerSpec{Image: model.NewExpr("")}
	})))
	h.tick(base)
	_, err := h.s.Acquire(ctx(), "runner-1", []string{"ubuntu-latest"}, base)
	if err == nil || !strings.Contains(err.Error(), "empty string") {
		t.Fatalf("want an empty-image error, got %v", err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if a.Steps[0].Run != "build for ubuntu" {
		t.Fatalf("step run %q", a.Steps[0].Run)
	}
	if a.JobName != "test (ubuntu)" {
		t.Fatalf("job name %q", a.JobName)
	}
}

// The default-clock wrappers exist for production callers; prove they reach
// the same funnel.
func TestDefaultClockWrappers(t *testing.T) {
	h := newHarness(t, wf(jobIR("a"), jobIR("b")))
	h.tick(base)
	if err := h.s.JobCompleted(ctx(), h.job("a").ID, Result{Conclusion: model.ConclusionSuccess}); err != nil {
		t.Fatal(err)
	}
	h.requireConclusion("a", model.ConclusionSuccess)
	if err := h.s.CancelJob(ctx(), h.job("b").ID, model.CancelReason{
		Actor: model.CancelActorUser, Sentence: "stopped by hand.",
	}); err != nil {
		t.Fatal(err)
	}
	h.requireConclusion("b", model.ConclusionCancelled)
	if err := h.s.Cancel(ctx(), h.run.ID, model.CancelReason{
		Actor: model.CancelActorUser, Sentence: "stopped the whole run.",
	}); err != nil {
		t.Fatal(err)
	}
}
