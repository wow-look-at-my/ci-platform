package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{
		BaseURL: srv.URL, Token: "runner-token",
		MaxAttempts: 3, Backoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
	})
	require.NoError(t, err)
	return c, srv
}

func TestNewClientRequiresURLAndToken(t *testing.T) {
	_, err := NewClient(ClientConfig{Token: "t"})
	require.ErrorContains(t, err, "control plane URL is required")

	_, err = NewClient(ClientConfig{BaseURL: "https://x"})
	require.ErrorContains(t, err, "runner token is required")
}

func TestClientRoundTripsEveryEndpoint(t *testing.T) {
	var seen []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		assert.Equal(t, "Bearer runner-token", r.Header.Get("Authorization"))
		assert.Equal(t, protocol.APIVersion, r.Header.Get("X-CI-Api-Version"))
		assert.Equal(t, http.MethodPost, r.Method)

		switch r.URL.Path {
		case protocol.PathRegister:
			_ = json.NewEncoder(w).Encode(protocol.RegisterResponse{
				LeaseTTL: protocol.Duration(90 * time.Second),
			})
		case protocol.PathAcquire:
			_ = json.NewEncoder(w).Encode(protocol.AcquireResponse{
				Assignment: &protocol.Assignment{JobID: 9, IdempotencyKey: "1/9/1"},
			})
		case protocol.PathHeartbeat:
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{LeaseLost: true})
		case protocol.PathComplete:
			_ = json.NewEncoder(w).Encode(protocol.CompleteResponse{WillRetry: true, NextAttempt: 2})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	ctx := context.Background()

	reg, err := c.Register(ctx, protocol.RegisterRequest{RunnerID: "r"})
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, reg.LeaseTTL.D())

	acq, err := c.Acquire(ctx, protocol.AcquireRequest{RunnerID: "r"})
	require.NoError(t, err)
	require.NotNil(t, acq.Assignment)
	assert.Equal(t, int64(9), acq.Assignment.JobID)

	hb, err := c.Heartbeat(ctx, protocol.HeartbeatRequest{RunnerID: "r"})
	require.NoError(t, err)
	assert.True(t, hb.LeaseLost)

	comp, err := c.Complete(ctx, protocol.CompleteRequest{RunnerID: "r"})
	require.NoError(t, err)
	assert.True(t, comp.WillRetry)

	require.NoError(t, c.Setup(ctx, protocol.SetupRequest{Phase: "started"}))
	require.NoError(t, c.Logs(ctx, protocol.LogBatch{}))
	require.NoError(t, c.StepStart(ctx, protocol.StepStartRequest{}))
	require.NoError(t, c.StepEnd(ctx, protocol.StepEndRequest{}))
	require.NoError(t, c.Annotate(ctx, protocol.AnnotateRequest{}))
	require.NoError(t, c.Release(ctx, protocol.ReleaseRequest{
		Reason: model.CancelReason{Actor: model.CancelActorShutdown, Sentence: "s"},
	}))

	assert.Len(t, seen, 10)
}

func TestClientRetriesServerErrorsAndSucceeds(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.AcquireResponse{})
	}))

	_, err := c.Acquire(context.Background(), protocol.AcquireRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

func TestClientGivesUpAfterMaxAttemptsAndClassifiesInfra(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))

	_, err := c.Acquire(context.Background(), protocol.AcquireRequest{})
	require.Error(t, err)
	assert.Equal(t, int32(3), calls.Load())

	var e *Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, http.StatusBadGateway, e.Status)
	assert.Equal(t, 3, e.Attempts)
	// The control plane being unreachable is never the workflow's fault.
	assert.Equal(t, model.ClassInfra, e.Class())
	assert.Contains(t, e.Error(), "HTTP 502")
	assert.NotEmpty(t, e.Decision.Reason)
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unknown runner", http.StatusUnauthorized)
	}))

	_, err := c.Register(context.Background(), protocol.RegisterRequest{})
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load(), "repeating a rejected request cannot help")

	var e *Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, http.StatusUnauthorized, e.Status)
}

func TestClientRetriesRateLimiting(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	require.NoError(t, c.Logs(context.Background(), protocol.LogBatch{}))
	assert.Equal(t, int32(2), calls.Load())
}

func TestClientRetriesNetworkFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c, err := NewClient(ClientConfig{
		BaseURL: url, Token: "t", MaxAttempts: 2,
		Backoff: time.Millisecond, MaxBackoff: time.Millisecond,
	})
	require.NoError(t, err)

	_, err = c.Acquire(context.Background(), protocol.AcquireRequest{})
	require.Error(t, err)
	var e *Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, 2, e.Attempts)
	assert.Equal(t, model.ClassInfra, e.Class())
}

func TestClientStopsRetryingWhenTheContextEnds(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Acquire(ctx, protocol.AcquireRequest{})
	require.Error(t, err)
}

func TestClientRejectsUnparseableResponse(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	_, err := c.Register(context.Background(), protocol.RegisterRequest{})
	require.Error(t, err)
}

func TestBackoffIsBounded(t *testing.T) {
	assert.Equal(t, time.Second, backoff(time.Second, 10*time.Second, 1))
	assert.Equal(t, 2*time.Second, backoff(time.Second, 10*time.Second, 2))
	assert.Equal(t, 4*time.Second, backoff(time.Second, 10*time.Second, 3))
	assert.Equal(t, 10*time.Second, backoff(time.Second, 10*time.Second, 9))
	assert.Equal(t, 5*time.Second, backoff(30*time.Second, 5*time.Second, 1))
}

func TestErrorMessageWithoutStatus(t *testing.T) {
	e := &Error{Path: "/x", Attempts: 2, Err: errors.New("dial tcp: connection refused")}
	assert.Contains(t, e.Error(), "after 2 attempt(s)")
	assert.EqualError(t, errors.Unwrap(e), "dial tcp: connection refused")
}

func TestClientTrimsTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{BaseURL: srv.URL + "/", Token: "t"})
	require.NoError(t, err)
	require.NoError(t, c.Logs(context.Background(), protocol.LogBatch{}))
	assert.Equal(t, protocol.PathLogs, path)
}
