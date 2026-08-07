package exec

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/commands"
)

// scope is the expression and failure scope a step runs in. The job's own
// scope is the zero value; a composite action's steps get their own.
type scope struct {
	depth     int
	nested    bool
	inputs    map[string]any
	steps     map[string]any
	actionDir string
	env       map[string]string
	failed    *bool
}

func (s *scope) isFailed(e *Executor) bool {
	if s != nil && s.nested && s.failed != nil {
		return *s.failed
	}
	return e.failed
}

func (s *scope) markFailed(e *Executor) {
	if s != nil && s.nested && s.failed != nil {
		*s.failed = true
		return
	}
	e.failed = true
}

func (s *scope) extraContexts() map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{}
	if s.inputs != nil {
		out["inputs"] = s.inputs
	}
	if s.steps != nil {
		out["steps"] = s.steps
	}
	return out
}

// outcome is one attempt's raw result, before classification.
type outcome struct {
	exitCode int
	err      error
	timedOut bool
	phase    string
	output   string
	outputs  map[string]string
	annots   []model.Annotation
}

func (o outcome) failed() bool { return o.exitCode != 0 || o.err != nil }

// runStep executes one step, including its retries, and publishes its result.
func (e *Executor) runStep(ctx context.Context, spec protocol.StepSpec, s *scope, depth int) StepResult {
	started := time.Now()
	res := StepResult{
		Number:  spec.Number,
		Name:    stepName(spec),
		StepID:  spec.ID,
		Outputs: map[string]string{},
	}

	if !s.isNested() {
		if err := e.cfg.Reporter.StepStarted(ctx, spec); err != nil {
			e.platform(spec.Number, fmt.Sprintf("warning: reporting the start of step %d failed: %v", spec.Number, err))
		}
	}

	run, skipReason, cfgErr := e.shouldRun(ctx, spec, s)
	switch {
	case cfgErr != nil:
		res = e.fail(ctx, res, spec, s, outcome{exitCode: 1, err: cfgErr, phase: "run"}, started)
		return res
	case !run:
		res.Outcome = model.ConclusionSkipped
		res.Conclusion = model.ConclusionSkipped
		res.Duration = time.Since(started)
		e.platform(spec.Number, fmt.Sprintf("skipped step %q: %s", res.Name, skipReason))
		e.setStepContext(spec.ID, res)
		e.finish(ctx, res, s)
		return res
	}

	policy := model.DefaultRetryPolicy()
	if e.cfg.Assignment.Retry.Attempts > 0 {
		policy = e.cfg.Assignment.Retry
	}
	if spec.Retry != nil {
		policy = *spec.Retry
	}
	if policy.Attempts < 1 {
		policy.Attempts = 1
	}

	var last outcome
	var decision classify.Decision
	for attemptNo := 1; ; attemptNo++ {
		res.Attempts = attemptNo
		if policy.Attempts > 1 {
			e.platform(spec.Number, fmt.Sprintf("Attempt %d of %d", attemptNo, policy.Attempts))
		}
		last = e.attempt(ctx, spec, s, attemptNo, depth)
		for k, v := range last.outputs {
			res.Outputs[k] = v
		}
		if len(last.annots) > 0 {
			e.sendAnnotations(ctx, last.annots)
		}
		if !last.failed() {
			break
		}
		e.reportErr(spec.Number, last.err)
		decision = e.cfg.Classifier.Classify(classify.Signal{
			ExitCode:  last.exitCode,
			Output:    last.output,
			Err:       last.err,
			Phase:     last.phase,
			TimedOut:  last.timedOut,
			Cancelled: ctx.Err() != nil,
		})
		e.record(spec.Number, decision)
		if !policy.Retries(decision.Class, attemptNo) {
			break
		}
		delay := policy.Delay(attemptNo + 1)
		e.platform(spec.Number, fmt.Sprintf("retrying step %q in %s: %s is retryable under this policy",
			res.Name, delay.Round(time.Millisecond), decision.Class))
		select {
		case <-ctx.Done():
			e.platform(spec.Number, "retry abandoned: the job was cancelled")
			return e.fail(ctx, res, spec, s, last, started)
		case <-time.After(delay):
		}
	}

	res.ExitCode = last.exitCode
	res.Duration = time.Since(started)
	if !last.failed() {
		res.Outcome = model.ConclusionSuccess
		res.Conclusion = model.ConclusionSuccess
		e.setStepContext(spec.ID, res)
		e.finish(ctx, res, s)
		return res
	}
	res.Class = decision.Class
	res.ClassReason = decision.String()
	return e.completeFailure(ctx, res, spec, s, decision, started)
}

func (s *scope) isNested() bool { return s != nil && s.nested }

// fail classifies and records a failure that happened before or outside an
// attempt loop.
func (e *Executor) fail(ctx context.Context, res StepResult, spec protocol.StepSpec, s *scope, o outcome, started time.Time) StepResult {
	e.reportErr(spec.Number, o.err)
	d := e.cfg.Classifier.Classify(classify.Signal{
		ExitCode: o.exitCode, Output: o.output, Err: o.err,
		Phase: o.phase, TimedOut: o.timedOut, Cancelled: ctx.Err() != nil,
	})
	e.record(spec.Number, d)
	res.ExitCode = o.exitCode
	res.Class = d.Class
	res.ClassReason = d.String()
	res.Duration = time.Since(started)
	return e.completeFailure(ctx, res, spec, s, d, started)
}

func (e *Executor) completeFailure(ctx context.Context, res StepResult, spec protocol.StepSpec, s *scope, d classify.Decision, started time.Time) StepResult {
	res.Outcome = d.Class.Conclusion()
	if d.Class == model.ClassNone {
		res.Outcome = model.ConclusionCancelled
	}
	res.Conclusion = res.Outcome
	if res.Duration == 0 {
		res.Duration = time.Since(started)
	}
	if spec.ContinueOnError {
		// The step still reports what really happened; only the job's verdict
		// changes, exactly as continue-on-error promises.
		res.Conclusion = model.ConclusionSuccess
		e.platform(spec.Number, fmt.Sprintf("step %q failed but continue-on-error is set, so the job continues", res.Name))
	} else {
		s.markFailed(e)
		if e.firstFailure == nil {
			r := res
			e.firstFailure = &r
		}
	}
	e.setStepContext(spec.ID, res)
	e.finish(ctx, res, s)
	return res
}

func (e *Executor) finish(ctx context.Context, res StepResult, s *scope) {
	if s.isNested() {
		return
	}
	if err := e.cfg.Reporter.StepEnded(ctx, res); err != nil {
		e.platform(res.Number, fmt.Sprintf("warning: reporting the end of step %d failed: %v", res.Number, err))
	}
}

// reportErr puts the failure's own message in the log. The classification says
// whose fault it was; only this says what actually happened.
func (e *Executor) reportErr(step int, err error) {
	if err != nil {
		e.platform(step, "error: "+err.Error())
	}
}

func (e *Executor) sendAnnotations(ctx context.Context, anns []model.Annotation) {
	if err := e.cfg.Reporter.Annotate(ctx, anns); err != nil {
		e.platform(0, fmt.Sprintf("warning: sending %d annotation(s) failed: %v", len(anns), err))
	}
}

// shouldRun evaluates the step's if:. A step with no if: runs only while the
// job is still succeeding, which is GitHub Actions' default.
func (e *Executor) shouldRun(ctx context.Context, spec protocol.StepSpec, s *scope) (bool, string, error) {
	if ctx.Err() != nil {
		return false, "the job was cancelled", nil
	}
	if strings.TrimSpace(spec.IfExpr) == "" {
		if s.isFailed(e) {
			return false, "an earlier step failed and this step has no if: condition", nil
		}
		return true, "", nil
	}
	ev, err := e.evaluator(ctx, s.extraContexts())
	if err != nil {
		return false, "", err
	}
	ok, err := ev.EvalBool(spec.IfExpr)
	if err != nil {
		return false, "", fmt.Errorf("expression error: evaluating if: %q for step %q: %w", spec.IfExpr, stepName(spec), err)
	}
	if !ok {
		return false, fmt.Sprintf("if: %q evaluated false", spec.IfExpr), nil
	}
	return true, "", nil
}

// attempt runs the step's body once.
func (e *Executor) attempt(ctx context.Context, spec protocol.StepSpec, s *scope, attemptNo, depth int) outcome {
	col := newCollector(spec.Number, e.cfg.Log, e.cfg.Masker)
	defer col.flush()

	key := fmt.Sprintf("%d_%d", spec.Number, attemptNo)
	if depth > 0 {
		key = fmt.Sprintf("%d_%d_%d", depth, spec.Number, attemptNo)
	}
	files := commands.NewEnvFiles(e.cfg.TempDir, key)
	if err := e.createEnvFiles(ctx, files); err != nil {
		return outcome{exitCode: 1, err: err, phase: "setup"}
	}

	env, err := e.buildStepEnv(ctx, spec, s, files)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}

	stepCtx := ctx
	var cancel context.CancelFunc
	if spec.TimeoutMinutes > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutMinutes)*time.Minute)
		defer cancel()
	}

	var o outcome
	switch {
	case spec.Uses != "":
		o = e.runUses(stepCtx, spec, s, env, col, depth)
	case spec.Run != "":
		o = e.runScript(stepCtx, spec, s, env, col)
	default:
		o = outcome{exitCode: 1, phase: "run",
			err: fmt.Errorf("unsupported: step %d has neither run: nor uses:", spec.Number)}
	}

	if stepCtx.Err() == context.DeadlineExceeded {
		o.timedOut = true
	}
	col.flush()
	if o.output == "" {
		o.output = col.tail.String()
	}
	if o.outputs == nil {
		o.outputs = map[string]string{}
	}
	for k, v := range col.outputs {
		o.outputs[k] = v
	}
	o.annots = append(o.annots, col.annots...)

	// Env files are read even when the step failed: a step that set an output
	// and then exited non-zero still set the output.
	if err := e.readEnvFiles(ctx, files, o.outputs, col); err != nil && o.err == nil {
		o.err = err
		if o.exitCode == 0 {
			o.exitCode = 1
		}
	}
	return o
}

// runScript writes the step's script into the sandbox and runs it under the
// requested shell.
func (e *Executor) runScript(ctx context.Context, spec protocol.StepSpec, s *scope, env map[string]string, col *collector) outcome {
	script := spec.Run
	if s != nil && s.inputs != nil && strings.Contains(script, "${{") {
		ev, err := e.evaluator(ctx, s.extraContexts())
		if err != nil {
			return outcome{exitCode: 1, err: err, phase: "run"}
		}
		expanded, err := ev.EvalString(script)
		if err != nil {
			return outcome{exitCode: 1, phase: "run",
				err: fmt.Errorf("expression error: expanding run: for step %q: %w", stepName(spec), err)}
		}
		script = expanded
	}

	shell := spec.Shell
	if shell == "" {
		shell = e.cfg.Assignment.DefaultShell
	}
	base := path.Join(e.cfg.TempDir, fmt.Sprintf("step_%d_%d", spec.Number, time.Now().UnixNano()))
	argv, scriptPath, err := shellCommand(shell, base)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}

	if err := e.cfg.Sandbox.WriteFile(ctx, scriptPath, []byte(script), 0o700); err != nil {
		return outcome{exitCode: 1, err: err, phase: "setup"}
	}
	defer func() { _ = e.cfg.Sandbox.RemoveAll(context.WithoutCancel(ctx), scriptPath) }()

	return e.exec(ctx, argv, env, e.workingDir(spec, s), col, "run")
}

// exec runs one command in the sandbox and reports its exit.
func (e *Executor) exec(ctx context.Context, argv []string, env map[string]string, wd string, col *collector, phase string) outcome {
	res, err := e.cfg.Sandbox.Run(ctx, RunRequest{
		Argv:       argv,
		Env:        env,
		WorkingDir: wd,
		Stdout:     col.writer("stdout"),
		Stderr:     col.writer("stderr"),
	})
	if err != nil {
		// The sandbox itself failed; that is never the workflow's fault.
		return outcome{exitCode: -1, err: err, phase: "setup"}
	}
	return outcome{exitCode: res.ExitCode, phase: phase}
}

func (e *Executor) workingDir(spec protocol.StepSpec, s *scope) string {
	wd := e.cfg.WorkspaceDir
	if e.cfg.Assignment.WorkingDirectory != "" {
		wd = absUnder(wd, e.cfg.Assignment.WorkingDirectory)
	}
	if spec.WorkingDirectory != "" {
		wd = absUnder(wd, spec.WorkingDirectory)
	}
	return wd
}

func absUnder(base, p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return path.Join(base, p)
}

// createEnvFiles makes the five per-step files empty, so a step reading one
// before writing it sees an empty file rather than an error.
func (e *Executor) createEnvFiles(ctx context.Context, files commands.EnvFiles) error {
	if err := e.cfg.Sandbox.MkdirAll(ctx, e.cfg.TempDir); err != nil {
		return err
	}
	for _, p := range files.All() {
		if err := e.cfg.Sandbox.WriteFile(ctx, p, nil, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// readEnvFiles applies what the step wrote: outputs, environment for later
// steps, PATH additions, and the step summary.
func (e *Executor) readEnvFiles(ctx context.Context, files commands.EnvFiles, outputs map[string]string, col *collector) error {
	read := func(p string) (string, error) {
		b, err := e.cfg.Sandbox.ReadFile(ctx, p)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", p, err)
		}
		return string(b), nil
	}

	data, err := read(files.Output)
	if err != nil {
		return err
	}
	kv, err := ParseEnvFile(data)
	if err != nil {
		return fmt.Errorf("$GITHUB_OUTPUT: %w", err)
	}
	for _, k := range kv.Order {
		outputs[k] = kv.Values[k]
	}

	data, err = read(files.Env)
	if err != nil {
		return err
	}
	kv, err = ParseEnvFile(data)
	if err != nil {
		return fmt.Errorf("$GITHUB_ENV: %w", err)
	}
	for _, k := range kv.Order {
		e.jobEnv[k] = kv.Values[k]
	}

	data, err = read(files.Path)
	if err != nil {
		return err
	}
	for _, p := range commands.ParsePathFile(data) {
		e.extraPath = append([]string{p}, e.extraPath...)
	}

	data, err = read(files.StepSummary)
	if err != nil {
		return err
	}
	if strings.TrimSpace(data) != "" {
		col.emit(fmt.Sprintf("step summary: %d bytes written to $GITHUB_STEP_SUMMARY", len(data)))
	}
	return nil
}

// ParseEnvFile is exported for tests and for the agent's own diagnostics.
func ParseEnvFile(data string) (commands.KeyValues, error) { return commands.ParseKeyValues(data) }

func stepName(spec protocol.StepSpec) string {
	switch {
	case spec.Name != "":
		return spec.Name
	case spec.Uses != "":
		return spec.Uses
	case spec.ID != "":
		return spec.ID
	default:
		return fmt.Sprintf("step %d", spec.Number)
	}
}

// classifySignalFor builds the classifier's input from an attempt's outcome.
func classifySignalFor(o outcome, col *collector) classify.Signal {
	out := o.output
	if out == "" && col != nil {
		out = col.tail.String()
	}
	return classify.Signal{
		ExitCode: o.exitCode,
		Output:   out,
		Err:      o.err,
		Phase:    o.phase,
		TimedOut: o.timedOut,
	}
}
