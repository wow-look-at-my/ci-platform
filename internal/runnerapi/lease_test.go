package runnerapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// A runner that lost its lease -- a partition, a reap, or simply another host
// holding the shared runner token -- must not be able to write anything for a
// job it no longer holds. The agent honouring LeaseLost is cooperation; this is
// enforcement.
func TestJobScopedWritesRequireHoldingTheLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)

	_, err := h.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	const impostor = "runner-b"
	cases := []struct {
		name string
		path string
		body any
	}{
		{"complete", protocol.PathComplete, protocol.CompleteRequest{
			RunnerID: impostor, JobID: job.ID, Attempt: 1,
			Conclusion: model.ConclusionSuccess,
		}},
		{"logs", protocol.PathLogs, protocol.LogBatch{
			RunnerID: impostor, JobID: job.ID, Attempt: 1,
			Lines: []model.LogLine{{Seq: 1, Text: "forged"}},
		}},
		{"step start", protocol.PathStepStart, protocol.StepStartRequest{
			RunnerID: impostor, JobID: job.ID, Attempt: 1, Number: 1, Name: "forged",
		}},
		{"step end", protocol.PathStepEnd, protocol.StepEndRequest{
			RunnerID: impostor, JobID: job.ID, Attempt: 1, Number: 1,
			Conclusion: model.ConclusionSuccess,
		}},
		{"annotate", protocol.PathAnnotate, protocol.AnnotateRequest{
			RunnerID: impostor, JobID: job.ID, Attempt: 1,
			Annotations: []model.Annotation{{Path: "x", StartLine: 1, Message: "forged"}},
		}},
		{"setup", protocol.PathSetup, protocol.SetupRequest{
			RunnerID: impostor, JobID: job.ID, Attempt: 1, Phase: "completed",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := h.post(t, tc.path, tc.body, nil)
			assert.Equal(t, http.StatusConflict, code,
				"a runner that does not hold the lease must be refused")
		})
	}

	assert.Empty(t, h.sched.completed, "no result was recorded for a job this runner never held")
	assert.Empty(t, h.logs.lines, "no log line was accepted")

	steps, err := h.st.ListSteps(ctx, job.ID, 1)
	require.NoError(t, err)
	assert.Empty(t, steps, "no step was recorded")

	as, err := h.st.ListAnnotations(ctx, job.ID)
	require.NoError(t, err)
	assert.Empty(t, as, "no annotation was recorded")
}

// The attempt is part of the identity: a runner holding the lease may not
// report for an attempt that is already over.
func TestJobScopedWritesRequireTheCurrentAttempt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)
	_, err := h.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	code := h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		RunnerID: "runner-a", JobID: job.ID, Attempt: 7,
		Conclusion: model.ConclusionSuccess,
	}, nil)
	assert.Equal(t, http.StatusConflict, code)
	assert.Empty(t, h.sched.completed)
}

// The holder is still allowed to do its job.
func TestTheLeaseHolderCanStillReport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, job := seedQueuedJob(t, h.st)
	_, err := h.st.Dequeue(ctx, "runner-a", []string{"linux"}, time.Minute)
	require.NoError(t, err)

	code := h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		RunnerID: "runner-a", JobID: job.ID, Attempt: 1,
		Conclusion: model.ConclusionSuccess,
	}, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, h.sched.completed, 1)
}

func TestUnknownJobIsNotFound(t *testing.T) {
	h := newHarness(t)
	code := h.post(t, protocol.PathComplete, protocol.CompleteRequest{
		RunnerID: "runner-a", JobID: 999999, Attempt: 1,
		Conclusion: model.ConclusionSuccess,
	}, nil)
	assert.Equal(t, http.StatusNotFound, code)
}
