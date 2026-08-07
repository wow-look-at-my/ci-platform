package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type agentHarness struct {
	cp    *fakeControlPlane
	box   *fakeSandbox
	agent *Agent
	setup *SetupReport
	err   error
}

func newAgentHarness(t *testing.T, cp *fakeControlPlane, tweak func(*Config)) *agentHarness {
	t.Helper()
	h := &agentHarness{cp: cp, box: newFakeSandbox()}
	cfg := Config{
		Client:   cp,
		RunnerID: "runner-1",
		Name:     "test-runner",
		Labels:   []string{"self-hosted", "linux"},
		StateDir: t.TempDir(),
		Logger:   quietLogger(),
		NewSandbox: func(context.Context, *protocol.Assignment) (Sandbox, *SetupReport, error) {
			if h.err != nil {
				return nil, h.setup, h.err
			}
			report := h.setup
			if report == nil {
				report = &SetupReport{
					Breakdown: map[string]time.Duration{"dockerd_ready": 3 * time.Second},
					CacheWarm: true,
					Total:     4 * time.Second,
				}
			}
			return h.box, report, nil
		},
		PollWait:         10 * time.Millisecond,
		LogFlushInterval: 5 * time.Millisecond,
		IdleDelay:        time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	a, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	h.agent = a
	return h
}

// runUntilDrained runs the agent until every scripted assignment is taken, then
// shuts it down.
func runUntilDrained(t *testing.T, h *agentHarness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx) }()

	select {
	case <-h.cp.onAcquireDrained:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("assignments were never acquired")
	}
	// Give the job time to finish before shutting the agent down.
	deadline := time.After(5 * time.Second)
	for {
		var n int
		h.cp.snapshot(func(f *fakeControlPlane) { n = len(f.completes) + len(f.releases) })
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("the job never completed or was released")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not shut down")
	}
}

func TestNewValidatesRequiredConfig(t *testing.T) {
	_, err := New(Config{})
	require.ErrorContains(t, err, "Client is required")

	_, err = New(Config{Client: newFakeControlPlane()})
	require.ErrorContains(t, err, "RunnerID is required")

	_, err = New(Config{Client: newFakeControlPlane(), RunnerID: "r"})
	require.ErrorContains(t, err, "StateDir is required")

	_, err = New(Config{Client: newFakeControlPlane(), RunnerID: "r", StateDir: t.TempDir()})
	require.ErrorContains(t, err, "NewSandbox is required")
}

func TestAgentRegistersAndRunsAJob(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Name: "build", Run: "make"}))
	h := newAgentHarness(t, cp, nil)
	runUntilDrained(t, h)

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.registered, 1)
		assert.Equal(t, protocol.APIVersion, f.registered[0].APIVersion)
		assert.Equal(t, []string{"self-hosted", "linux"}, f.registered[0].Labels)

		require.Len(t, f.completes, 1)
		assert.Equal(t, model.ConclusionSuccess, f.completes[0].Conclusion)
		assert.Equal(t, int64(2), f.completes[0].JobID)

		require.Len(t, f.stepStarts, 1)
		require.Len(t, f.stepEnds, 1)
		assert.Equal(t, model.ConclusionSuccess, f.stepEnds[0].Conclusion)

		// Setup is reported as a measured boundary, twice.
		require.Len(t, f.setups, 2)
		assert.Equal(t, "started", f.setups[0].Phase)
		assert.Equal(t, "completed", f.setups[1].Phase)
		assert.True(t, f.setups[1].CacheWarm)
		assert.Contains(t, f.setups[1].Breakdown, "dockerd_ready")
		assert.Equal(t, 3*time.Second, f.setups[1].Breakdown["dockerd_ready"].D())
	})

	assert.True(t, h.box.isClosed(), "the sandbox is always torn down")
	text := cp.logText()
	assert.Contains(t, text, "setup completed in")
	assert.Contains(t, text, "setup/dockerd_ready")
}

func TestAgentRefusesADuplicateIdempotencyKey(t *testing.T) {
	first := testAssignment(protocol.StepSpec{Number: 1, Run: "make"})
	second := testAssignment(protocol.StepSpec{Number: 1, Run: "make"}) // same key
	cp := newFakeControlPlane(first, second)
	h := newAgentHarness(t, cp, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx) }()

	require.Eventually(t, func() bool {
		var n int
		cp.snapshot(func(f *fakeControlPlane) { n = len(f.releases) })
		return n == 1
	}, 5*time.Second, 2*time.Millisecond, "the duplicate was never released")
	cancel()
	<-done

	cp.snapshot(func(f *fakeControlPlane) {
		assert.Len(t, f.completes, 1, "the job runs exactly once")
		require.Len(t, f.releases, 1)
		r := f.releases[0]
		assert.Equal(t, model.CancelActorRunnerLost, r.Reason.Actor)
		assert.Contains(t, r.Reason.Sentence, "already started idempotency key 1/2/1")
		require.NoError(t, r.Reason.Validate(), "a release always carries a valid reason")
	})
}

func TestIdempotencySurvivesARestart(t *testing.T) {
	stateDir := t.TempDir()
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "make"}))
	h := newAgentHarness(t, cp, func(c *Config) { c.StateDir = stateDir })
	runUntilDrained(t, h)
	require.NoError(t, h.agent.Close())

	// A fresh agent process, same state dir, same assignment redelivered.
	cp2 := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "make"}))
	h2 := newAgentHarness(t, cp2, func(c *Config) { c.StateDir = stateDir })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h2.agent.Run(ctx) }()
	require.Eventually(t, func() bool {
		var n int
		cp2.snapshot(func(f *fakeControlPlane) { n = len(f.releases) })
		return n == 1
	}, 5*time.Second, 2*time.Millisecond)
	cancel()
	<-done

	cp2.snapshot(func(f *fakeControlPlane) {
		assert.Empty(t, f.completes, "a key started before the restart must never run again")
	})
}

func TestHeartbeatCancellationStopsTheJobAndRecordsTheReason(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Name: "long", Run: "sleep"}))
	reason := model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "alice cancelled run 1 from the web UI",
		TriggeredBy: "alice",
	}
	cp.heartbeatResponse = &protocol.HeartbeatResponse{Cancel: &reason}

	h := newAgentHarness(t, cp, nil)
	h.box.runFn = blockUntilCancelled()
	runUntilDrained(t, h)

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.completes, 1)
		c := f.completes[0]
		assert.Equal(t, model.ConclusionCancelled, c.Conclusion)
		require.NotNil(t, c.Cancel)
		assert.Equal(t, model.CancelActorUser, c.Cancel.Actor)
		assert.Equal(t, reason.Sentence, c.Explanation)
	})
	// The reason is surfaced in the log, not only in the API payload.
	assert.Contains(t, cp.logText(), "alice cancelled run 1 from the web UI")
}

func TestCancellationWithoutAReasonIsRepairedNotPassedThrough(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "x"}))
	cp.heartbeatResponse = &protocol.HeartbeatResponse{Cancel: &model.CancelReason{Actor: model.CancelActorUser}}

	h := newAgentHarness(t, cp, nil)
	h.box.runFn = blockUntilCancelled()
	runUntilDrained(t, h)

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.completes, 1)
		require.NotNil(t, f.completes[0].Cancel)
		require.NoError(t, f.completes[0].Cancel.Validate(),
			"a cancellation with no sentence is the incident this platform exists to prevent")
		assert.Contains(t, f.completes[0].Cancel.Sentence, "control-plane defect")
	})
}

func TestLeaseLostStopsWithoutReportingAResult(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "x"}))
	cp.heartbeatResponse = &protocol.HeartbeatResponse{LeaseLost: true}
	h := newAgentHarness(t, cp, nil)
	h.box.runFn = blockUntilCancelled()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx) }()
	require.Eventually(t, func() bool { return h.box.isClosed() }, 5*time.Second, 2*time.Millisecond)
	cancel()
	<-done

	cp.snapshot(func(f *fakeControlPlane) {
		assert.Empty(t, f.completes, "reporting a result would overwrite the new owner's work")
		assert.Empty(t, f.releases)
	})
	assert.Contains(t, cp.logText(), "lease")
}

func TestSandboxSetupFailureIsInfraAndNeverStartsTheJob(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "x"}))
	h := newAgentHarness(t, cp, nil)
	h.err = errors.New("sandbox dockerd_ready failed: the inner docker daemon did not become ready within 5m0s")
	runUntilDrained(t, h)

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.completes, 1)
		c := f.completes[0]
		assert.Equal(t, model.ConclusionInfraFailure, c.Conclusion)
		assert.Equal(t, model.ClassInfra, c.Class)
		assert.Contains(t, c.Explanation, "the job never started")
		assert.NotEmpty(t, c.ClassificationLog)
		assert.Empty(t, f.stepStarts, "no step runs when the sandbox never came up")
	})
}

func TestTeardownFailureIsReportedInTheJobLog(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "x"}))
	h := newAgentHarness(t, cp, nil)
	h.box.closeFn = func() error { return errors.New("removing container ci-job-2-1: permission denied") }
	runUntilDrained(t, h)

	assert.Contains(t, cp.logText(), "TEARDOWN FAILED")
	cp.snapshot(func(f *fakeControlPlane) {
		assert.Len(t, f.completes, 1, "a teardown failure does not lose the job's result")
	})
}

func TestShutdownReleasesTheJobInsteadOfCompletingIt(t *testing.T) {
	cp := newFakeControlPlane(testAssignment(protocol.StepSpec{Number: 1, Run: "x"}))
	h := newAgentHarness(t, cp, nil)

	started := make(chan struct{})
	var once sync.Once
	h.box.runFn = func(ctx context.Context, _ exec.RunRequest) (exec.RunResult, error) {
		once.Do(func() { close(started) })
		// Block until the job context is cancelled by the shutdown.
		<-ctx.Done()
		return exec.RunResult{ExitCode: 1}, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.agent.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the step never started")
	}
	cancel() // SIGTERM
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not shut down")
	}

	cp.snapshot(func(f *fakeControlPlane) {
		assert.Empty(t, f.completes, "a job interrupted by shutdown is requeued, not concluded")
		require.Len(t, f.releases, 1)
		assert.Equal(t, model.CancelActorShutdown, f.releases[0].Reason.Actor)
		require.NoError(t, f.releases[0].Reason.Validate())
	})
	assert.Contains(t, cp.logText(), "shutting down")
}

func TestAcquireFailureIsLoggedAsInfraAndRetried(t *testing.T) {
	cp := newFakeControlPlane()
	cp.acquireErr = &Error{Path: protocol.PathAcquire, Err: errors.New("connection refused")}
	h := newAgentHarness(t, cp, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.NoError(t, h.agent.Run(ctx))

	cp.snapshot(func(f *fakeControlPlane) {
		assert.Len(t, f.registered, 1)
	})
}

func TestRegistrationFailureIsFatal(t *testing.T) {
	cp := &failingRegisterPlane{fakeControlPlane: newFakeControlPlane()}
	h := newAgentHarness(t, cp.fakeControlPlane, func(c *Config) { c.Client = cp })
	err := h.agent.Run(context.Background())
	require.Error(t, err, "a runner that cannot register must not idle green")
	assert.Contains(t, err.Error(), "registering runner")
}

type failingRegisterPlane struct {
	*fakeControlPlane
}

func (f *failingRegisterPlane) Register(context.Context, protocol.RegisterRequest) (protocol.RegisterResponse, error) {
	return protocol.RegisterResponse{}, errors.New("HTTP 503")
}

func TestClassOf(t *testing.T) {
	assert.Equal(t, model.ClassInfra, classOf(errors.New("plain")))
	assert.Equal(t, model.ClassInfra, classOf(&Error{Decision: infraDecision()}))
}

func TestSortedDurationKeys(t *testing.T) {
	got := sortedDurationKeys(map[string]time.Duration{"c": 1, "a": 2, "b": 3})
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestWarmWord(t *testing.T) {
	assert.Equal(t, "warm", warmWord(true))
	assert.Equal(t, "cold", warmWord(false))
}

func TestReleaseWithoutAReasonIsRepaired(t *testing.T) {
	cp := newFakeControlPlane()
	h := newAgentHarness(t, cp, nil)
	h.agent.release(context.Background(), testAssignment(), model.CancelReason{})

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.releases, 1)
		require.NoError(t, f.releases[0].Reason.Validate(),
			"a job that reappears in the queue must always say why")
		assert.Contains(t, strings.ToLower(f.releases[0].Reason.Sentence), "runner defect")
	})
}

func infraDecision() classify.Decision {
	return classify.Decision{Class: model.ClassInfra, Rule: "test", Reason: "test"}
}

// blockUntilCancelled makes a step run until the heartbeat or shutdown stops
// it, which is the only way a control-plane instruction can reach a job.
func blockUntilCancelled() func(context.Context, exec.RunRequest) (exec.RunResult, error) {
	return func(ctx context.Context, _ exec.RunRequest) (exec.RunResult, error) {
		<-ctx.Done()
		return exec.RunResult{ExitCode: 1}, ctx.Err()
	}
}
