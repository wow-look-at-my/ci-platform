package agent

import (
	"context"
	"io/fs"
	"sync"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

// fakeControlPlane records every call and lets a test script the answers.
type fakeControlPlane struct {
	mu sync.Mutex

	registered []protocol.RegisterRequest
	acquires   int
	heartbeats []protocol.HeartbeatRequest
	setups     []protocol.SetupRequest
	logs       []protocol.LogBatch
	stepStarts []protocol.StepStartRequest
	stepEnds   []protocol.StepEndRequest
	annots     []protocol.AnnotateRequest
	completes  []protocol.CompleteRequest
	releases   []protocol.ReleaseRequest

	// assignments are handed out one per Acquire; nil entries are empty polls.
	assignments []*protocol.Assignment
	// heartbeatResponse is returned by every heartbeat when set.
	heartbeatResponse *protocol.HeartbeatResponse
	acquireErr        error
	logErr            error
	// onAcquireDrained is closed when the last assignment has been handed out.
	onAcquireDrained chan struct{}
	drainOnce        sync.Once
}

func newFakeControlPlane(assignments ...*protocol.Assignment) *fakeControlPlane {
	return &fakeControlPlane{assignments: assignments, onAcquireDrained: make(chan struct{})}
}

func (f *fakeControlPlane) Register(_ context.Context, r protocol.RegisterRequest) (protocol.RegisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, r)
	return protocol.RegisterResponse{
		LeaseTTL:          protocol.Duration(60e9),
		HeartbeatInterval: protocol.Duration(5e6), // 5ms, so tests see heartbeats
		LogFlushInterval:  protocol.Duration(5e6),
	}, nil
}

func (f *fakeControlPlane) Acquire(ctx context.Context, _ protocol.AcquireRequest) (protocol.AcquireResponse, error) {
	f.mu.Lock()
	if f.acquireErr != nil {
		err := f.acquireErr
		f.mu.Unlock()
		return protocol.AcquireResponse{}, err
	}
	f.acquires++
	var a *protocol.Assignment
	if len(f.assignments) > 0 {
		a = f.assignments[0]
		f.assignments = f.assignments[1:]
	}
	drained := len(f.assignments) == 0
	f.mu.Unlock()

	if drained {
		f.drainOnce.Do(func() { close(f.onAcquireDrained) })
	}
	if a == nil {
		// An empty poll blocks like the real long poll rather than spinning.
		<-ctx.Done()
		return protocol.AcquireResponse{}, ctx.Err()
	}
	return protocol.AcquireResponse{Assignment: a}, nil
}

func (f *fakeControlPlane) Heartbeat(_ context.Context, r protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, r)
	if f.heartbeatResponse != nil {
		return *f.heartbeatResponse, nil
	}
	return protocol.HeartbeatResponse{}, nil
}

func (f *fakeControlPlane) Setup(_ context.Context, r protocol.SetupRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setups = append(f.setups, r)
	return nil
}

func (f *fakeControlPlane) Logs(_ context.Context, r protocol.LogBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logErr != nil {
		return f.logErr
	}
	f.logs = append(f.logs, r)
	return nil
}

func (f *fakeControlPlane) StepStart(_ context.Context, r protocol.StepStartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stepStarts = append(f.stepStarts, r)
	return nil
}

func (f *fakeControlPlane) StepEnd(_ context.Context, r protocol.StepEndRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stepEnds = append(f.stepEnds, r)
	return nil
}

func (f *fakeControlPlane) Annotate(_ context.Context, r protocol.AnnotateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.annots = append(f.annots, r)
	return nil
}

func (f *fakeControlPlane) Complete(_ context.Context, r protocol.CompleteRequest) (protocol.CompleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completes = append(f.completes, r)
	return protocol.CompleteResponse{}, nil
}

func (f *fakeControlPlane) Release(_ context.Context, r protocol.ReleaseRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, r)
	return nil
}

func (f *fakeControlPlane) snapshot(fn func(*fakeControlPlane)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// logText joins every delivered log line.
func (f *fakeControlPlane) logText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out string
	for _, b := range f.logs {
		for _, l := range b.Lines {
			out += l.Text + "\n"
		}
	}
	return out
}

// fakeSandbox is a no-op execution environment for the agent's loop tests.
type fakeSandbox struct {
	mu      sync.Mutex
	files   map[string][]byte
	closed  bool
	closeFn func() error
	runFn   func(ctx context.Context, req exec.RunRequest) (exec.RunResult, error)
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{files: map[string][]byte{}}
}

func (s *fakeSandbox) Run(ctx context.Context, req exec.RunRequest) (exec.RunResult, error) {
	if s.runFn != nil {
		return s.runFn(ctx, req)
	}
	return exec.RunResult{}, nil
}

func (s *fakeSandbox) WriteFile(_ context.Context, p string, d []byte, _ fs.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = append([]byte(nil), d...)
	return nil
}

func (s *fakeSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[p], nil
}

func (s *fakeSandbox) MkdirAll(context.Context, string) error         { return nil }
func (s *fakeSandbox) RemoveAll(context.Context, string) error        { return nil }
func (s *fakeSandbox) CopyInto(context.Context, string, string) error { return nil }
func (s *fakeSandbox) LookPath(_ context.Context, b string) (string, error) {
	return "/usr/bin/" + b, nil
}
func (s *fakeSandbox) WorkspaceDir() string { return "/workspace" }
func (s *fakeSandbox) TempDir() string      { return "/tmp/_temp" }

func (s *fakeSandbox) Close(context.Context) error {
	s.mu.Lock()
	s.closed = true
	fn := s.closeFn
	s.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}

func (s *fakeSandbox) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func testAssignment(steps ...protocol.StepSpec) *protocol.Assignment {
	return &protocol.Assignment{
		RunID: 1, JobID: 2, Attempt: 1,
		IdempotencyKey: "1/2/1",
		JobName:        "build", JobKey: "build",
		RepoOwner: "octo", RepoName: "demo",
		Steps:     steps,
		ServerURL: "https://ci.example.localhost",
		JobToken:  "tok",
		Retry:     model.RetryPolicy{Attempts: 1},
	}
}
