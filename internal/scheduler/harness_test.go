package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
)

var base = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

type harness struct {
	t     *testing.T
	st    *fakeStore
	s     *Scheduler
	run   *model.Run
	plan  *plan.Plan
	notes []Notification
}

func jobIR(key string, mods ...func(*model.JobIR)) *model.JobIR {
	j := &model.JobIR{
		Key:    key,
		RunsOn: model.RunsOn{Labels: []model.Expr{model.NewExpr("ubuntu-latest")}},
		Steps:  []*model.StepIR{{Number: 1, Name: model.NewExpr("run"), Run: model.NewExpr("make")}},
	}
	for _, m := range mods {
		m(j)
	}
	return j
}

func wf(jobs ...*model.JobIR) *model.Workflow {
	w := &model.Workflow{Name: "CI", Path: ".github/workflows/ci.yml", Jobs: map[string]*model.JobIR{}}
	for _, j := range jobs {
		w.Jobs[j.Key] = j
		w.JobOrder = append(w.JobOrder, j.Key)
	}
	return w
}

func newHarness(t *testing.T, w *model.Workflow, mods ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, st: newFakeStore()}
	opts := Options{
		NewEval:      fakeFactory,
		ServerURL:    "https://ci.example.com",
		MintJobToken: func(runID, jobID int64, attempt int) (string, error) { return "tok", nil },
		Notify:       func(_ context.Context, n Notification) { h.notes = append(h.notes, n) },
	}
	for _, m := range mods {
		m(&opts)
	}
	h.s = New(h.st, opts)

	repo := &model.Repo{ID: 7, Owner: "wow-look-at-my", Name: "ci-platform", DefaultBranch: "master"}
	if err := h.st.UpsertRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	h.run = &model.Run{
		ID: 1, RepoID: repo.ID, RepoFull: repo.FullName(),
		WorkflowName: "CI", WorkflowPath: w.Path, RunNumber: 1, Attempt: 1,
		Event: "push", HeadSHA: "deadbeef", HeadBranch: "feature", Actor: "someone",
		Status: model.StatusQueued, CreatedAt: base,
	}
	if err := h.st.CreateRun(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(w, plan.Input{
		Run:      h.run,
		Contexts: map[string]any{"github": map[string]any{"ref": "refs/heads/feature"}},
		NewEval:  fakeFactory,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	h.plan = p
	if err := h.s.StartRun(context.Background(), h.run, p); err != nil {
		t.Fatalf("start run: %v", err)
	}
	return h
}

func (h *harness) tick(at time.Time) {
	h.t.Helper()
	if err := h.s.Tick(context.Background(), at); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

func (h *harness) job(name string) *model.Job {
	h.t.Helper()
	jobs, err := h.st.ListJobsForRun(context.Background(), h.run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Name == name || j.Key == name {
			return j
		}
	}
	h.t.Fatalf("no job named %q", name)
	return nil
}

func (h *harness) jobs() []*model.Job {
	h.t.Helper()
	jobs, err := h.st.ListJobsForRun(context.Background(), h.run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return jobs
}

// complete reports an outcome for a job, as a runner would.
func (h *harness) complete(name string, c model.Conclusion, class model.FailureClass, at time.Time) {
	h.t.Helper()
	j := h.job(name)
	j.Status = model.StatusInProgress
	if j.StartedAt == nil {
		j.StartedAt = &at
	}
	if err := h.st.UpdateJob(context.Background(), j); err != nil {
		h.t.Fatal(err)
	}
	res := Result{Conclusion: c, Class: class}
	if c != model.ConclusionSuccess {
		res.ClassReason = "test-supplied classification"
		res.Explanation = string(c)
	}
	if err := h.s.JobCompletedAt(context.Background(), j.ID, res, at); err != nil {
		h.t.Fatalf("complete %s: %v", name, err)
	}
}

func (h *harness) runRow() *model.Run {
	h.t.Helper()
	r, err := h.st.GetRun(context.Background(), h.run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) requireConclusion(name string, want model.Conclusion) {
	h.t.Helper()
	if got := h.job(name).Conclusion; got != want {
		h.t.Fatalf("%s concluded %q, want %q", name, got, want)
	}
}
