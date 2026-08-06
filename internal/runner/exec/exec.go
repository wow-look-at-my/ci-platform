// Package exec runs a job's steps inside a sandbox: shells, actions, retries,
// timeouts, and the classification of everything that goes wrong.
package exec

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/actions"
	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

// Evaluator evaluates workflow expressions. It is declared here rather than
// imported so this package does not depend on the expression implementation.
type Evaluator interface {
	EvalString(string) (string, error)
	EvalBool(string) (bool, error)
}

// EvaluatorFactory builds an Evaluator over the contexts visible at one point
// in the job, plus the job's current status.
type EvaluatorFactory func(contexts map[string]any, status Status) Evaluator

// Status is the job status an `if:` expression sees through success(),
// failure() and cancelled().
type Status struct {
	Success   bool
	Failure   bool
	Cancelled bool
}

// Log receives job output. Text arrives already masked and grouped; the agent
// assigns sequence numbers and batches it.
type Log interface {
	Line(stepNumber int, stream, group, text string)
}

// Reporter forwards step boundaries and annotations to the control plane.
type Reporter interface {
	StepStarted(ctx context.Context, spec protocol.StepSpec) error
	StepEnded(ctx context.Context, res StepResult) error
	Annotate(ctx context.Context, anns []model.Annotation) error
}

// ActionResolver turns a `uses:` value into a directory on the host.
type ActionResolver interface {
	Resolve(ctx context.Context, uses string) (actions.Resolved, error)
}

// StepResult is one finished step.
type StepResult struct {
	Number int
	Name   string
	StepID string
	// Outcome is what happened; Conclusion is what the job acts on, which
	// differs only when continue-on-error is set.
	Outcome     model.Conclusion
	Conclusion  model.Conclusion
	Class       model.FailureClass
	ClassReason string
	ExitCode    int
	Outputs     map[string]string
	Attempts    int
	Duration    time.Duration
}

// Result is the whole job attempt.
type Result struct {
	Conclusion        model.Conclusion
	Class             model.FailureClass
	ClassReason       string
	Explanation       string
	Outputs           map[string]string
	Steps             []StepResult
	ClassificationLog []string
}

// Config wires an executor. Assignment, Sandbox and Log are required.
type Config struct {
	Assignment *protocol.Assignment
	Sandbox    Sandbox
	Log        Log
	Reporter   Reporter
	Masker     *mask.Masker
	Classifier *classify.Classifier
	// NewEvaluator is required for any job with an `if:` or a composite action;
	// its absence is reported when one is reached, never silently treated as
	// true.
	NewEvaluator EvaluatorFactory
	Actions      ActionResolver

	// WorkflowEnv sits under the job env, which sits under the step env.
	WorkflowEnv map[string]string

	WorkspaceDir string
	TempDir      string
	// ActionsDir is where resolved actions are placed inside the sandbox.
	ActionsDir string
	// MaxCompositeDepth bounds nested composite `uses:` recursion.
	MaxCompositeDepth int

	// RuntimeToken and IDTokenRequestURL are minted by the control plane and
	// injected verbatim; the runner never constructs them.
	RuntimeToken      string
	IDTokenRequestURL string
	ResultsURL        string

	RunnerName string
	RunnerOS   string
	RunnerArch string

	// BasePath is the sandbox image PATH that $GITHUB_PATH additions prepend to.
	BasePath string
}

// Executor runs one job attempt.
type Executor struct {
	cfg Config

	jobEnv    map[string]string
	extraPath []string
	stepsCtx  map[string]any
	// classifications is the full decision log the control plane records, so
	// an operator can see why anything was called infra.
	classifications []string
	posts           []postAction
	failed          bool
	firstFailure    *StepResult
}

// postAction is an action's post: entrypoint, deferred to the end of the job.
type postAction struct {
	name      string
	number    int
	script    string
	actionDir string
	meta      *actions.Metadata
	env       map[string]string
}

// New validates the configuration and returns an executor. A missing
// requirement is an error at construction: there is no half-configured mode.
func New(cfg Config) (*Executor, error) {
	if cfg.Assignment == nil {
		return nil, errors.New("exec: Assignment is required")
	}
	if cfg.Sandbox == nil {
		return nil, errors.New("exec: Sandbox is required")
	}
	if cfg.Log == nil {
		return nil, errors.New("exec: Log is required")
	}
	if cfg.Masker == nil {
		cfg.Masker = mask.New()
	}
	if cfg.Classifier == nil {
		cfg.Classifier = &classify.Classifier{}
	}
	if cfg.Reporter == nil {
		cfg.Reporter = noopReporter{}
	}
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = "/workspace"
	}
	if cfg.TempDir == "" {
		cfg.TempDir = "/home/runner/work/_temp"
	}
	if cfg.ActionsDir == "" {
		cfg.ActionsDir = "/home/runner/work/_actions"
	}
	if cfg.MaxCompositeDepth <= 0 {
		cfg.MaxCompositeDepth = 10
	}

	e := &Executor{
		cfg:      cfg,
		jobEnv:   map[string]string{},
		stepsCtx: map[string]any{},
	}
	for k, v := range cfg.Assignment.Env {
		e.jobEnv[k] = v
	}
	cfg.Masker.AddAll(cfg.Assignment.Secrets)
	cfg.Masker.Add(cfg.Assignment.JobToken)
	cfg.Masker.Add(cfg.RuntimeToken)
	return e, nil
}

// Run executes every step and returns the attempt's outcome.
func (e *Executor) Run(ctx context.Context) Result {
	e.checkServerURL()

	res := Result{Outputs: map[string]string{}}
	for _, spec := range e.cfg.Assignment.Steps {
		sr := e.runStep(ctx, spec, nil, 0)
		res.Steps = append(res.Steps, sr)
	}
	// Post entrypoints run even when the job has already failed, which is the
	// only reason an action can be trusted to clean up after itself.
	for i := len(e.posts) - 1; i >= 0; i-- {
		res.Steps = append(res.Steps, e.runPost(ctx, e.posts[i]))
	}

	res.ClassificationLog = e.classifications
	switch {
	case ctx.Err() != nil:
		res.Conclusion = model.ConclusionCancelled
	case e.firstFailure != nil:
		f := e.firstFailure
		res.Class = f.Class
		res.Conclusion = f.Class.Conclusion()
		res.ClassReason = f.ClassReason
		res.Explanation = fmt.Sprintf("step %d (%s) failed with exit code %d", f.Number, f.Name, f.ExitCode)
	default:
		res.Conclusion = model.ConclusionSuccess
	}
	return res
}

// checkServerURL warns when the server URL will make artifact actions fail.
// @actions/artifact treats anything not ending in .ghe.com or .localhost as
// GitHub Enterprise Server and refuses to run, so a silent mismatch surfaces
// later as an unexplained action error.
func (e *Executor) checkServerURL() {
	u := e.cfg.Assignment.ServerURL
	if u == "" {
		return
	}
	host := u
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.TrimSuffix(strings.SplitN(host, "/", 2)[0], ".")
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	if strings.HasSuffix(host, ".ghe.com") || strings.HasSuffix(host, ".localhost") || host == "localhost" {
		return
	}
	e.platform(0, fmt.Sprintf(
		"warning: GITHUB_SERVER_URL is %q; actions/upload-artifact@v4 rejects any host not ending in .ghe.com or .localhost", u))
}

func (e *Executor) platform(step int, text string) {
	e.cfg.Log.Line(step, "platform", "", e.cfg.Masker.Mask(text))
}

// record adds a classification decision to the job log and to the decision log
// the control plane stores.
func (e *Executor) record(step int, d classify.Decision) {
	line := d.String()
	e.classifications = append(e.classifications, line)
	e.platform(step, line)
}

// status is what success()/failure()/cancelled() see right now.
func (e *Executor) status(ctx context.Context) Status {
	return Status{
		Success:   !e.failed && ctx.Err() == nil,
		Failure:   e.failed,
		Cancelled: ctx.Err() != nil,
	}
}

// contexts builds the expression scope: whatever the control plane evaluated,
// plus what only the runner knows.
func (e *Executor) contexts(extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range e.cfg.Assignment.Contexts {
		out[k] = v
	}
	out["steps"] = e.stepsCtx
	env := map[string]any{}
	for k, v := range e.stepEnv(nil, nil) {
		env[k] = v
	}
	out["env"] = env
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (e *Executor) evaluator(ctx context.Context, extra map[string]any) (Evaluator, error) {
	if e.cfg.NewEvaluator == nil {
		return nil, errors.New("expression error: the runner has no expression evaluator configured")
	}
	return e.cfg.NewEvaluator(e.contexts(extra), e.status(ctx)), nil
}

// setStepContext publishes a step's outcome and outputs for later `if:` and
// ${{ steps.x }} references.
func (e *Executor) setStepContext(id string, r StepResult) {
	if id == "" {
		return
	}
	outputs := map[string]any{}
	for k, v := range r.Outputs {
		outputs[k] = v
	}
	e.stepsCtx[id] = map[string]any{
		"outputs":    outputs,
		"outcome":    string(r.Outcome),
		"conclusion": string(r.Conclusion),
	}
}

type noopReporter struct{}

func (noopReporter) StepStarted(context.Context, protocol.StepSpec) error { return nil }
func (noopReporter) StepEnded(context.Context, StepResult) error          { return nil }
func (noopReporter) Annotate(context.Context, []model.Annotation) error   { return nil }

// actionSandboxDir is where a resolved repository action lands inside the
// sandbox.
func (e *Executor) actionSandboxDir(r actions.Resolved) string {
	return path.Join(e.cfg.ActionsDir, r.Ref.Owner, r.Ref.Repo, r.SHA, r.Ref.Path)
}
