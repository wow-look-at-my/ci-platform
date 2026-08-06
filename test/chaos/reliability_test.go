package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/scheduler"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
	"github.com/wow-look-at-my/ci-platform/internal/workflow"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// rig is a control plane's scheduler over a real store, driven by a real
// workflow. It is the smallest thing that can exercise the reliability layer
// without a runner process.
type rig struct {
	t     *testing.T
	st    store.Store
	s     *scheduler.Scheduler
	run   *model.Run
	plan  *plan.Plan
	notes []scheduler.Notification
}

func newRig(t *testing.T, src string) *rig {
	t.Helper()
	ctx := context.Background()
	st := mem.New()

	repo := &model.Repo{ID: 7, Owner: "acme", Name: "widget", DefaultBranch: "main"}
	require.NoError(t, st.UpsertRepo(ctx, repo))

	run := &model.Run{
		RepoID: repo.ID, RepoFull: repo.FullName(),
		WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		RunNumber: 1, Attempt: 1, Event: "push",
		HeadSHA: "deadbeef", HeadBranch: "main", Actor: "alex",
		Status: model.StatusQueued, CreatedAt: epoch,
	}
	require.NoError(t, st.CreateRun(ctx, run))

	w, err := workflow.Parse(".github/workflows/ci.yml", []byte(src))
	require.NoError(t, err)

	r := &rig{t: t, st: st, run: run}
	r.s = scheduler.New(st, scheduler.Options{
		NewEval:      newEval,
		MintJobToken: func(int64, int64, int) (string, error) { return "job-token", nil },
		Notify: func(_ context.Context, n scheduler.Notification) {
			r.notes = append(r.notes, n)
		},
		LeaseTTL:          time.Minute,
		SetupTimeout:      10 * time.Minute,
		DefaultJobTimeout: time.Hour,
		RunTimeout:        6 * time.Hour,
		ServerURL:         "http://ci.localhost",
	})

	p, err := plan.Build(w, plan.Input{
		Run:      run,
		Contexts: map[string]any{"github": map[string]any{"ref": "refs/heads/main"}},
		NewEval:  newEval,
	})
	require.NoError(t, err)
	r.plan = p
	require.NoError(t, r.s.StartRun(ctx, run, p))
	return r
}

func newEval(contexts map[string]any, status plan.Status) plan.Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success: status.Success, Failure: status.Failure, Cancelled: status.Cancelled,
	})
}

func (r *rig) job(key string) *model.Job {
	r.t.Helper()
	jobs, err := r.st.ListJobsForRun(context.Background(), r.run.ID)
	require.NoError(r.t, err)
	for _, j := range jobs {
		if j.Key == key {
			return j
		}
	}
	r.t.Fatalf("no job %q in run", key)
	return nil
}

func (r *rig) events(jobID int64) []store.Event {
	r.t.Helper()
	evs, err := r.st.ListEvents(context.Background(), 0, jobID)
	require.NoError(r.t, err)
	return evs
}

const oneJob = `name: CI
on: push
jobs:
  build:
    runs-on: [linux]
    steps:
      - run: make build
`

// Incident 2: a job whose runner disappears is requeued, not lost and not
// failed. The lease is the mechanism; a missed heartbeat is the trigger.
func TestIncident2_LostRunnerRequeuesRatherThanFailing(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, oneJob)

	require.NoError(t, r.s.Tick(ctx, epoch))
	job := r.job("build")
	require.Equal(t, model.StatusQueued, job.Status)

	// A runner takes the job and then vanishes without completing it.
	claimed, err := r.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.Equal(t, model.StatusInProgress, r.job("build").Status)

	// Past the lease, the reaper puts the work back. The store stamps leases
	// from the wall clock, so the tick that reaps them has to be wall-clock
	// relative rather than relative to the test's fixed epoch.
	require.NoError(t, r.s.Tick(ctx, time.Now().Add(5*time.Minute)))

	after := r.job("build")
	assert.Equal(t, model.StatusQueued, after.Status,
		"a job whose runner disappeared must be requeued, never failed and never lost")
	assert.Empty(t, after.Conclusion, "it did not conclude; it is waiting again")
	assert.Equal(t, 1, after.RequeueCount)
	assert.Zero(t, after.InfraRetryCount, "a lost runner must not consume a retry attempt")

	// And the requeue says why, with an actor.
	var found bool
	for _, e := range r.events(after.ID) {
		if e.Message != "" && containsAll(e.Message, "Runner", "runner-a") {
			found = true
		}
		if actor, ok := e.Detail["actor"].(string); ok && actor == string(model.CancelActorRunnerLost) {
			found = true
		}
	}
	assert.True(t, found, "the requeue must record which runner was lost: %+v", r.events(after.ID))

	// Another runner can pick it straight up: a lost lease is immediately
	// redispatchable, because the backoff belongs to a retry, not a requeue.
	again, err := r.st.Dequeue(ctx, "runner-b", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, after.ID, again.ID)
}

// Incident 3: nothing is cancelled without a recorded, surfaced reason.
func TestIncident3_CancellationRecordsItsReasonEverywhere(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, oneJob)
	require.NoError(t, r.s.Tick(ctx, epoch))

	reason := model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "Alex cancelled this run from the web UI.",
		TriggeredBy: "alex",
	}
	require.NoError(t, r.s.Cancel(ctx, r.run.ID, reason))

	job := r.job("build")
	require.Equal(t, model.ConclusionCancelled, job.Conclusion)
	require.NotNil(t, job.Cancel, "the job row carries the reason")
	assert.Equal(t, model.CancelActorUser, job.Cancel.Actor)
	// The scheduler appends which job it was, so the sentence a reader sees on
	// the job says more than the one the operator typed.
	assert.Contains(t, job.Cancel.Sentence, reason.Sentence)
	require.NoError(t, job.Cancel.Validate())

	run, err := r.st.GetRun(ctx, r.run.ID)
	require.NoError(t, err)
	require.NotNil(t, run.Cancel, "the run row carries it too")
	assert.Contains(t, run.Cancel.Sentence, reason.Sentence)

	// And it is in the timeline the UI reads back.
	var inTimeline bool
	for _, e := range r.events(job.ID) {
		if contains(e.Message, reason.Sentence) {
			inTimeline = true
		}
	}
	assert.True(t, inTimeline, "the sentence must reach the job timeline: %+v", r.events(job.ID))
}

// The scheduler must offer no way to cancel without an explanation.
func TestCancellationWithoutAReasonIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, oneJob)
	require.NoError(t, r.s.Tick(ctx, epoch))

	require.Error(t, r.s.Cancel(ctx, r.run.ID, model.CancelReason{Actor: model.CancelActorUser}),
		"an actor with no sentence must be refused")
	require.Error(t, r.s.Cancel(ctx, r.run.ID, model.CancelReason{Sentence: "because"}),
		"a sentence with no actor must be refused")

	assert.NotEqual(t, model.ConclusionCancelled, r.job("build").Conclusion,
		"a refused cancellation must not have half-happened")
}

// An infra failure retries with backoff and every attempt is visible. A user
// failure never does.
func TestInfraFailureRetriesAndUserFailureDoesNot(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, oneJob)
	require.NoError(t, r.s.Tick(ctx, epoch))
	id := r.job("build").ID

	_, err := r.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	require.NoError(t, r.s.JobCompletedAt(ctx, id, scheduler.Result{
		Conclusion:  model.ConclusionInfraFailure,
		Class:       model.ClassInfra,
		ClassReason: `classified infra via rule "cloudflare-524": the remote returned HTTP 524`,
	}, epoch.Add(time.Minute)))

	retried := r.job("build")
	assert.NotEqual(t, model.StatusCompleted, retried.Status, "an infra failure is retried, not concluded")
	assert.Equal(t, 2, retried.Attempt, "attempt 2 of 3 is visible on the job")
	assert.Equal(t, 1, retried.InfraRetryCount)

	// The classification reason is recorded, so an operator can see why.
	var explained bool
	for _, e := range r.events(id) {
		if containsAll(e.Message, "cloudflare-524") {
			explained = true
		}
	}
	assert.True(t, explained, "the classification decision must be recorded: %+v", r.events(id))

	// A user failure on the next attempt concludes immediately.
	require.NoError(t, r.s.Tick(ctx, time.Now().Add(10*time.Minute)))
	_, err = r.st.Dequeue(ctx, "runner-b", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.s.JobCompletedAt(ctx, id, scheduler.Result{
		Conclusion: model.ConclusionFailure,
		Class:      model.ClassUser,
	}, epoch.Add(11*time.Minute)))

	done := r.job("build")
	assert.Equal(t, model.StatusCompleted, done.Status)
	assert.Equal(t, model.ConclusionFailure, done.Conclusion)
	assert.Equal(t, 2, done.Attempt, "a user failure never buys another attempt")
}

// Incident 5: a default-branch run that did not succeed fires the alarm. This
// is the merged, green PR that never published.
func TestIncident5_DefaultBranchFailureFiresTheAlarm(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, oneJob)
	require.NoError(t, r.s.Tick(ctx, epoch))
	id := r.job("build").ID

	_, err := r.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.s.JobCompletedAt(ctx, id, scheduler.Result{
		Conclusion: model.ConclusionFailure,
		Class:      model.ClassUser,
	}, epoch.Add(time.Minute)))
	require.NoError(t, r.s.Tick(ctx, epoch.Add(2*time.Minute)))

	require.NotEmpty(t, r.notes, "a failed run on the default branch must notify, not sit silently in a list")
	n := r.notes[0]
	assert.Equal(t, "main", n.Branch)
	assert.Equal(t, model.ConclusionFailure, n.Conclusion)
	assert.NotEmpty(t, n.Summary)
}

// A run whose only job was skipped is not a success, and the rollup says so.
func TestSkippedRunIsNotASuccess(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, `name: CI
on: push
jobs:
  build:
    runs-on: [linux]
    if: github.ref == 'refs/heads/never'
    steps:
      - run: make build
`)
	require.NoError(t, r.s.Tick(ctx, epoch))

	job := r.job("build")
	require.Equal(t, model.ConclusionSkipped, job.Conclusion)

	roll, err := r.s.RunRollup(ctx, r.run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ConclusionSkipped, roll.Conclusion,
		"a run whose work was all skipped must not conclude success")
	assert.NotContains(t, roll.Summary, "succeeded")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
