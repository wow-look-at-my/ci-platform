package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

// Sandbox is the per-job execution environment the agent builds. The concrete
// implementation is internal/runner/sandbox; the interface keeps the agent's
// loop testable without docker.
type Sandbox interface {
	exec.Sandbox
	WorkspaceDir() string
	TempDir() string
	Close(ctx context.Context) error
}

// SetupReport is the measured cost of building a sandbox.
type SetupReport struct {
	Breakdown map[string]time.Duration
	CacheWarm bool
	Total     time.Duration
}

// SandboxFactory builds the sandbox for one assignment.
type SandboxFactory func(ctx context.Context, a *protocol.Assignment) (Sandbox, *SetupReport, error)

// Config wires an agent. Client, RunnerID, StateDir and NewSandbox are
// required; a missing one is an error at construction rather than a runner
// that idles green.
type Config struct {
	Client   ControlPlane
	RunnerID string
	Name     string
	Labels   []string
	Group    string
	Capacity int
	StateDir string
	Version  string

	NewSandbox   SandboxFactory
	NewExecutor  func(cfg exec.Config) (*exec.Executor, error)
	NewEvaluator exec.EvaluatorFactory
	Actions      exec.ActionResolver

	// PollWait is how long an acquire poll may be held open.
	PollWait time.Duration
	// HeartbeatInterval and LogFlushInterval are defaults; the control plane's
	// register response overrides them.
	HeartbeatInterval time.Duration
	LogFlushInterval  time.Duration
	LogBatchSize      int

	Logger *slog.Logger
	// IdleDelay is how long to wait after an empty poll or a failed acquire.
	IdleDelay time.Duration
}

// Agent is one runner process.
type Agent struct {
	cfg   Config
	keys  *KeyStore
	log   *slog.Logger
	lease protocol.RegisterResponse

	mu     sync.Mutex
	active map[int64]*jobHandle
}

type jobHandle struct {
	assignment *protocol.Assignment
	cancel     context.CancelFunc
	// released marks a job already handed back, so shutdown does not release
	// it twice.
	released bool
}

// New validates the configuration and opens the idempotency store.
func New(cfg Config) (*Agent, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("agent: Client is required")
	case strings.TrimSpace(cfg.RunnerID) == "":
		return nil, errors.New("agent: RunnerID is required")
	case strings.TrimSpace(cfg.StateDir) == "":
		return nil, errors.New("agent: StateDir is required")
	case cfg.NewSandbox == nil:
		return nil, errors.New("agent: NewSandbox is required")
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = 30 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.LogFlushInterval <= 0 {
		cfg.LogFlushInterval = 2 * time.Second
	}
	if cfg.LogBatchSize <= 0 {
		cfg.LogBatchSize = 200
	}
	if cfg.IdleDelay <= 0 {
		cfg.IdleDelay = time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.NewExecutor == nil {
		cfg.NewExecutor = exec.New
	}
	keys, err := OpenKeyStore(filepath.Join(cfg.StateDir, "started-keys"))
	if err != nil {
		return nil, err
	}
	return &Agent{cfg: cfg, keys: keys, log: cfg.Logger, active: map[int64]*jobHandle{}}, nil
}

// Close releases the agent's own resources.
func (a *Agent) Close() error { return a.keys.Close() }

// Run registers and then claims work until ctx is cancelled. Cancelling ctx is
// the graceful shutdown: every job still running is released back to the queue
// so it is requeued rather than lost.
func (a *Agent) Run(ctx context.Context) error {
	reg, err := a.cfg.Client.Register(ctx, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion,
		RunnerID:   a.cfg.RunnerID,
		Name:       a.cfg.Name,
		Labels:     a.cfg.Labels,
		Group:      a.cfg.Group,
		Capacity:   a.cfg.Capacity,
		Version:    a.cfg.Version,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("registering runner %s: %w", a.cfg.RunnerID, err)
	}
	a.lease = reg
	a.log.Info("registered",
		"runner", a.cfg.RunnerID, "labels", a.cfg.Labels,
		"lease_ttl", reg.LeaseTTL.D(), "heartbeat", reg.HeartbeatInterval.D())

	var wg sync.WaitGroup
	for i := 0; i < a.cfg.Capacity; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.pollLoop(ctx, slot)
		}(i)
	}
	wg.Wait()
	a.releaseAll()
	return nil
}

func (a *Agent) pollLoop(ctx context.Context, slot int) {
	for ctx.Err() == nil {
		resp, err := a.cfg.Client.Acquire(ctx, protocol.AcquireRequest{
			RunnerID: a.cfg.RunnerID,
			Labels:   a.cfg.Labels,
			Wait:     protocol.Duration(a.cfg.PollWait),
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The control plane being unreachable is infra, and it is logged
			// as such rather than retried silently forever.
			a.log.Error("acquire failed", "slot", slot, "err", err, "class", classOf(err))
			if sleepCtx(ctx, a.cfg.IdleDelay) != nil {
				return
			}
			continue
		}
		if resp.Assignment == nil {
			continue
		}
		a.runJob(ctx, resp.Assignment)
	}
}

func classOf(err error) model.FailureClass {
	var e *Error
	if errors.As(err, &e) {
		return e.Decision.Class
	}
	return model.ClassInfra
}

// runJob executes one assignment end to end.
func (a *Agent) runJob(parent context.Context, asg *protocol.Assignment) {
	log := a.log.With("run", asg.RunID, "job", asg.JobID, "attempt", asg.Attempt)

	// Idempotency comes first, before anything with a side effect. A key this
	// runner has already started is handed back rather than run twice.
	if a.keys.Started(asg.IdempotencyKey) {
		log.Warn("refusing duplicate assignment", "key", asg.IdempotencyKey)
		a.release(parent, asg, model.CancelReason{
			Actor: model.CancelActorRunnerLost,
			Sentence: fmt.Sprintf(
				"runner %s had already started idempotency key %s, so this delivery was released instead of executed a second time",
				a.cfg.RunnerID, asg.IdempotencyKey),
		})
		return
	}
	if err := a.keys.MarkStarted(asg.IdempotencyKey); err != nil {
		log.Error("cannot record idempotency key", "err", err)
		a.release(parent, asg, model.CancelReason{
			Actor:    model.CancelActorRunnerLost,
			Sentence: fmt.Sprintf("runner %s could not durably record the job's idempotency key (%v), so it refused to start work it could not promise to run once", a.cfg.RunnerID, err),
		})
		return
	}

	jobCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()

	handle := &jobHandle{assignment: asg, cancel: cancel}
	a.mu.Lock()
	a.active[asg.JobID] = handle
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.active, asg.JobID)
		a.mu.Unlock()
	}()

	masker := mask.New()
	masker.AddAll(asg.Secrets)
	masker.Add(asg.JobToken)

	sink := NewLogSink(LogSinkConfig{
		Client: a.cfg.Client, RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt,
		Masker: masker, Interval: a.flushInterval(), MaxLines: a.cfg.LogBatchSize,
		OnError: func(err error) { log.Error("shipping logs failed", "err", err) },
	})
	sink.Start(jobCtx)
	defer func() {
		if err := sink.Close(context.WithoutCancel(jobCtx)); err != nil {
			log.Error("final log flush failed", "err", err)
		}
	}()

	// The heartbeat is what carries cancellation back to a running job.
	hb := a.startHeartbeat(jobCtx, asg, sink, cancel, log)

	// Shutdown: cancel the job so it stops, then release it below.
	shutdown := make(chan struct{})
	var shuttingDown bool
	var shutOnce sync.Once
	go func() {
		select {
		case <-parent.Done():
			shutOnce.Do(func() {
				shuttingDown = true
				sink.Line(0, "platform", "", "the runner is shutting down; this job is being released back to the queue")
				cancel()
			})
		case <-shutdown:
		}
	}()
	defer close(shutdown)

	result, setupErr := a.executeJob(jobCtx, asg, sink, masker, log)

	hb.stop()
	if hb.leaseLost() {
		// The lease was taken from us. Reporting a result now would overwrite
		// whatever the new owner is doing.
		log.Warn("lease lost; abandoning the job without reporting a result")
		sink.Line(0, "platform", "", "the control plane took this job's lease; the runner stopped without reporting a result")
		return
	}
	if shuttingDown {
		a.release(context.WithoutCancel(parent), asg, model.CancelReason{
			Actor:    model.CancelActorShutdown,
			Sentence: fmt.Sprintf("runner %s is shutting down, so job attempt %d was released back to the queue and will be picked up by another runner", a.cfg.RunnerID, asg.Attempt),
		})
		a.markReleased(asg.JobID)
		return
	}

	req := protocol.CompleteRequest{
		RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt,
		Conclusion:        result.Conclusion,
		Class:             result.Class,
		ClassReason:       result.ClassReason,
		Explanation:       result.Explanation,
		Outputs:           result.Outputs,
		ClassificationLog: result.ClassificationLog,
	}
	if reason := hb.cancelReason(); reason != nil {
		req.Conclusion = model.ConclusionCancelled
		req.Class = model.ClassNone
		req.Cancel = reason
		req.Explanation = reason.Sentence
	}
	if setupErr != nil {
		log.Error("job setup failed", "err", setupErr)
	}

	if err := sink.Flush(context.WithoutCancel(jobCtx)); err != nil {
		log.Error("log flush before completion failed", "err", err)
	}
	resp, err := a.cfg.Client.Complete(context.WithoutCancel(jobCtx), req)
	if err != nil {
		log.Error("reporting completion failed", "err", err, "class", classOf(err))
		return
	}
	if resp.WillRetry {
		log.Info("control plane will retry", "next_attempt", resp.NextAttempt, "after", resp.RetryAfter.D())
	}
}

// executeJob builds the sandbox, runs the steps, and always tears down.
func (a *Agent) executeJob(ctx context.Context, asg *protocol.Assignment, sink *LogSink, masker *mask.Masker, log *slog.Logger) (exec.Result, error) {
	cl := classify.Classifier{}

	_ = a.cfg.Client.Setup(ctx, protocol.SetupRequest{
		RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt, Phase: "started",
	})

	box, report, err := a.cfg.NewSandbox(ctx, asg)
	if err != nil {
		d := cl.Classify(classify.Signal{Err: err, Phase: "setup", TimedOut: isTimeout(err)})
		sink.Line(0, "platform", "", "sandbox setup failed: "+err.Error())
		sink.Line(0, "platform", "", d.String())
		return exec.Result{
			Conclusion:        d.Class.Conclusion(),
			Class:             d.Class,
			ClassReason:       d.String(),
			Explanation:       "the job never started: " + err.Error(),
			ClassificationLog: []string{d.String()},
		}, err
	}
	defer func() {
		// Teardown always runs, and its failure is reported rather than
		// swallowed: a leaked container eats the next job's disk.
		if cerr := box.Close(context.WithoutCancel(ctx)); cerr != nil {
			log.Error("sandbox teardown failed", "err", cerr)
			sink.Line(0, "platform", "", "TEARDOWN FAILED: "+cerr.Error())
		}
	}()

	if report != nil {
		a.reportSetup(ctx, asg, report, sink)
	}

	ex, err := a.cfg.NewExecutor(exec.Config{
		Assignment:   asg,
		Sandbox:      box,
		Log:          sink,
		Reporter:     &reporter{agent: a, asg: asg, sink: sink},
		Masker:       masker,
		Classifier:   &cl,
		NewEvaluator: a.cfg.NewEvaluator,
		Actions:      a.cfg.Actions,
		WorkspaceDir: box.WorkspaceDir(),
		TempDir:      box.TempDir(),
		RunnerName:   a.cfg.Name,
		RunnerOS:     runtime.GOOS,
		RunnerArch:   runtime.GOARCH,
	})
	if err != nil {
		d := cl.Classify(classify.Signal{Err: err, Phase: "setup"})
		sink.Line(0, "platform", "", d.String())
		return exec.Result{
			Conclusion: d.Class.Conclusion(), Class: d.Class, ClassReason: d.String(),
			Explanation: err.Error(), ClassificationLog: []string{d.String()},
		}, err
	}
	return ex.Run(ctx), nil
}

func (a *Agent) reportSetup(ctx context.Context, asg *protocol.Assignment, report *SetupReport, sink *LogSink) {
	breakdown := map[string]protocol.Duration{}
	for k, v := range report.Breakdown {
		breakdown[k] = protocol.Duration(v)
	}
	if err := a.cfg.Client.Setup(ctx, protocol.SetupRequest{
		RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt,
		Phase: "completed", Breakdown: breakdown, CacheWarm: report.CacheWarm,
	}); err != nil {
		a.log.Error("reporting setup breakdown failed", "err", err)
	}
	sink.Line(0, "platform", "", fmt.Sprintf("setup completed in %s (image cache %s)",
		report.Total.Round(time.Millisecond), warmWord(report.CacheWarm)))
	for _, k := range sortedDurationKeys(report.Breakdown) {
		sink.Line(0, "platform", "", fmt.Sprintf("  setup/%s: %s", k, report.Breakdown[k].Round(time.Millisecond)))
	}
}

func warmWord(warm bool) string {
	if warm {
		return "warm"
	}
	return "cold"
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

func (a *Agent) flushInterval() time.Duration {
	if d := a.lease.LogFlushInterval.D(); d > 0 {
		return d
	}
	return a.cfg.LogFlushInterval
}

func (a *Agent) heartbeatInterval() time.Duration {
	if d := a.lease.HeartbeatInterval.D(); d > 0 {
		return d
	}
	return a.cfg.HeartbeatInterval
}

func (a *Agent) release(ctx context.Context, asg *protocol.Assignment, reason model.CancelReason) {
	if err := reason.Validate(); err != nil {
		// A release with no explanation is exactly the incident this platform
		// exists to prevent, so it is repaired loudly rather than sent.
		a.log.Error("refusing to release without a reason", "err", err)
		reason.Sentence = fmt.Sprintf("runner %s released job %d attempt %d without recording a reason, which is a runner defect", a.cfg.RunnerID, asg.JobID, asg.Attempt)
		reason.Actor = model.CancelActorRunnerLost
	}
	if err := a.cfg.Client.Release(ctx, protocol.ReleaseRequest{
		RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt, Reason: reason,
	}); err != nil {
		a.log.Error("releasing job failed", "job", asg.JobID, "err", err)
	}
}

func (a *Agent) markReleased(jobID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.active[jobID]; ok {
		h.released = true
	}
}

// releaseAll hands back anything still held when the loop exits.
func (a *Agent) releaseAll() {
	a.mu.Lock()
	handles := make([]*jobHandle, 0, len(a.active))
	for _, h := range a.active {
		if !h.released {
			handles = append(handles, h)
		}
	}
	a.mu.Unlock()
	for _, h := range handles {
		a.release(context.Background(), h.assignment, model.CancelReason{
			Actor:    model.CancelActorShutdown,
			Sentence: fmt.Sprintf("runner %s stopped while holding this job, so it was released back to the queue", a.cfg.RunnerID),
		})
	}
}

func sortedDurationKeys(m map[string]time.Duration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
