package chaos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/agent"
)

// Incident 2: the runner could not reach the control plane, retried three times
// with backoff, and the job never started. From the outside the run just sat
// there, with a BadGateway in the runner log and nothing anywhere else.
//
// The requirement is not that this never happens; it is that when it does, the
// failure is the platform's and is named as such. There is no user code in the
// dispatch path, so a control plane that cannot be reached can never be the
// user's fault.
func TestIncident2_ControlPlaneUnreachableIsInfraNotABuildFailure(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"BadGateway"}`))
	}))
	defer srv.Close()

	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL: srv.URL,
		Token:   "runner-token",
		// Do not wait out the real backoff; the point is the classification.
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = client.Acquire(ctx, protocol.AcquireRequest{
		RunnerID: "runner-a", Labels: []string{"linux"},
		Wait: protocol.Duration(time.Second),
	})
	require.Error(t, err, "an unreachable control plane must be reported, not silently retried forever")
	assert.Greater(t, attempts.Load(), int64(1), "it must retry before giving up")

	// Whatever the agent does next, the classification is the load-bearing
	// part: this is infrastructure, so it is retried and never rendered as a
	// red build.
	var c classify.Classifier
	d := c.Classify(classify.Signal{
		Phase: "dispatch",
		Err:   err,
	})
	require.Equal(t, model.ClassInfra, d.Class,
		"there is no user code in the dispatch path, so this can never be the user's failure")
	assert.True(t, d.Class.Retryable())
	assert.False(t, model.ConclusionInfraFailure.UserVisibleRed())
	assert.True(t, strings.Contains(d.Reason, "platform") || strings.Contains(d.Reason, "control plane"),
		"the reason must say whose failure it was: %s", d.Reason)
}

// A control plane that recovers mid-retry is not a failure at all.
func TestControlPlaneRecoveryIsNotAFailure(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL: srv.URL, Token: "runner-token",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.Acquire(ctx, protocol.AcquireRequest{RunnerID: "runner-a"})
	require.NoError(t, err, "two 503s then success must come out as success")
	assert.Nil(t, resp.Assignment, "an empty poll is a normal outcome")
	assert.Equal(t, int64(3), attempts.Load())
}

// A runner told its lease is gone stops without reporting a result. Two runners
// completing the same job is how a run gets a result nobody can explain.
func TestLeaseLostStopsTheRunnerWithoutAResult(t *testing.T) {
	var completes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case protocol.PathHeartbeat:
			_, _ = w.Write([]byte(`{"lease_lost":true}`))
		case protocol.PathComplete:
			completes.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL: srv.URL, Token: "runner-token",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)

	resp, err := client.Heartbeat(context.Background(), protocol.HeartbeatRequest{
		RunnerID: "runner-a", JobID: 1, Attempt: 1, Phase: "execute",
	})
	require.NoError(t, err)
	require.True(t, resp.LeaseLost, "the control plane must be able to say the lease is gone")
	assert.Nil(t, resp.Cancel, "a lost lease is not a cancellation and carries no reason")
	assert.Zero(t, completes.Load(), "nothing was reported for a job this runner no longer holds")
}

// A cancellation reaches a running job only through the heartbeat, and it
// always carries the reason.
func TestCancellationReachesTheRunnerWithItsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cancel":{"actor":"concurrency_group",` +
			`"sentence":"Superseded by run 42 in concurrency group deploy-main.",` +
			`"triggered_by":"run/42"}}`))
	}))
	defer srv.Close()

	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL: srv.URL, Token: "runner-token",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)

	resp, err := client.Heartbeat(context.Background(), protocol.HeartbeatRequest{
		RunnerID: "runner-a", JobID: 1, Attempt: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Cancel)
	require.NoError(t, resp.Cancel.Validate(),
		"a cancellation that reached a runner without an actor and a sentence is the incident")
	assert.Equal(t, model.CancelActorConcurrencyGroup, resp.Cancel.Actor)
	assert.Contains(t, resp.Cancel.Sentence, "Superseded by run 42")
}
