package exec

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/actions"
)

// runUses resolves an action reference and runs it.
func (e *Executor) runUses(ctx context.Context, spec protocol.StepSpec, s *scope, env map[string]string, col *collector, depth int) outcome {
	if e.cfg.Actions == nil {
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unable to resolve action %q: the runner has no action resolver configured", spec.Uses)}
	}
	if depth > e.cfg.MaxCompositeDepth {
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unsupported: composite action nesting exceeded the depth limit of %d at %q",
				e.cfg.MaxCompositeDepth, spec.Uses)}
	}

	resolved, err := e.cfg.Actions.Resolve(ctx, spec.Uses)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "action-fetch"}
	}

	var (
		meta      *actions.Metadata
		actionDir string
	)
	switch resolved.Ref.Kind {
	case actions.KindDocker:
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unsupported: container actions (%s) are not implemented yet", spec.Uses)}
	case actions.KindLocal:
		actionDir = path.Join(e.cfg.WorkspaceDir, resolved.Ref.LocalPath)
		meta, err = e.loadSandboxMetadata(ctx, actionDir)
		if err != nil {
			return outcome{exitCode: 1, err: err, phase: "action-fetch"}
		}
	default:
		actionDir = e.actionSandboxDir(resolved)
		if err := e.cfg.Sandbox.CopyInto(ctx, resolved.Dir, actionDir); err != nil {
			return outcome{exitCode: 1, phase: "setup",
				err: fmt.Errorf("placing action %s in the sandbox: %w", spec.Uses, err)}
		}
		meta = resolved.Meta
		if resolved.Cached {
			col.emit(fmt.Sprintf("action %s served from the runner's action cache (%s)", spec.Uses, resolved.SHA))
		}
	}
	if meta == nil {
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("action %q has no parsed action.yml", spec.Uses)}
	}

	with, err := e.expandWith(ctx, spec.With, s)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}

	switch {
	case meta.Runs.IsJavaScript():
		return e.runJavaScript(ctx, spec, meta, actionDir, with, env, col)
	case meta.Runs.IsComposite():
		return e.runComposite(ctx, spec, meta, actionDir, with, col, depth)
	case meta.Runs.IsDocker():
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unsupported: action %q uses runs.using %q (image %q), which is not implemented yet",
				spec.Uses, meta.Runs.Using, meta.Runs.Image)}
	default:
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unsupported: action %q uses runs.using %q, which this runner does not implement",
				spec.Uses, meta.Runs.Using)}
	}
}

// expandWith evaluates ${{ }} in a step's inputs. Top-level `with:` arrives
// already evaluated; a composite's nested `with:` does not.
func (e *Executor) expandWith(ctx context.Context, with map[string]string, s *scope) (map[string]string, error) {
	if len(with) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(with))
	var ev Evaluator
	for k, v := range with {
		if strings.Contains(v, "${{") {
			if ev == nil {
				var err error
				if ev, err = e.evaluator(ctx, s.extraContexts()); err != nil {
					return nil, err
				}
			}
			expanded, err := ev.EvalString(v)
			if err != nil {
				return nil, fmt.Errorf("expression error: evaluating with.%s: %w", k, err)
			}
			v = expanded
		}
		out[k] = v
	}
	return out, nil
}

func (e *Executor) loadSandboxMetadata(ctx context.Context, dir string) (*actions.Metadata, error) {
	var lastErr error
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := e.cfg.Sandbox.ReadFile(ctx, path.Join(dir, name))
		if err != nil {
			lastErr = err
			continue
		}
		return actions.ParseMetadata(data)
	}
	return nil, fmt.Errorf("unable to resolve action: no action.yml or action.yaml in %s: %v", dir, lastErr)
}

// runJavaScript executes a node action's main entrypoint, and registers its
// post entrypoint to run at the end of the job.
func (e *Executor) runJavaScript(ctx context.Context, spec protocol.StepSpec, meta *actions.Metadata, actionDir string, with, env map[string]string, col *collector) outcome {
	inputEnv, warnings, err := meta.InputEnv(with)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}
	for _, w := range warnings {
		col.emit("warning: " + w)
	}

	node, err := e.nodeBinary(ctx, meta.Runs.NodeVersion())
	if err != nil {
		// A missing runtime is the sandbox image's problem, never the
		// workflow's, and it is a failure rather than a skipped step.
		return outcome{exitCode: 1, err: err, phase: "setup"}
	}

	full := map[string]string{}
	for k, v := range env {
		full[k] = v
	}
	for k, v := range inputEnv {
		full[k] = v
	}
	full["GITHUB_ACTION_PATH"] = actionDir

	if meta.Runs.Pre != "" {
		col.emit(fmt.Sprintf("running pre entrypoint %s", meta.Runs.Pre))
		if o := e.exec(ctx, []string{node, path.Join(actionDir, meta.Runs.Pre)}, full, e.cfg.WorkspaceDir, col, "run"); o.failed() {
			return o
		}
	}
	if meta.Runs.Post != "" {
		e.posts = append(e.posts, postAction{
			name:      stepName(spec) + " (post)",
			number:    spec.Number,
			script:    meta.Runs.Post,
			actionDir: actionDir,
			meta:      meta,
			env:       full,
		})
	}
	if meta.Runs.Main == "" {
		return outcome{exitCode: 1, phase: "action-fetch",
			err: fmt.Errorf("unsupported: action %q declares runs.using %q with no runs.main", spec.Uses, meta.Runs.Using)}
	}
	return e.exec(ctx, []string{node, path.Join(actionDir, meta.Runs.Main)}, full, e.cfg.WorkspaceDir, col, "run")
}

// nodeBinary finds the node runtime in the sandbox. The version-specific name
// is tried first so an image can ship several.
func (e *Executor) nodeBinary(ctx context.Context, version string) (string, error) {
	candidates := []string{"node" + version, "node"}
	var lastErr error
	for _, c := range candidates {
		p, err := e.cfg.Sandbox.LookPath(ctx, c)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("a node%s action needs a node runtime in the sandbox image and none was found: %w", version, lastErr)
}

// runPost executes a deferred post entrypoint. It runs whatever the job's
// outcome, which is the only reason an action can clean up after itself.
func (e *Executor) runPost(ctx context.Context, p postAction) StepResult {
	col := newCollector(p.number, e.cfg.Log, e.cfg.Masker)
	defer col.flush()
	col.emit(fmt.Sprintf("running post entrypoint %s", p.script))

	res := StepResult{Number: p.number, Name: p.name, Attempts: 1, Outputs: map[string]string{}}
	node, err := e.nodeBinary(ctx, p.meta.Runs.NodeVersion())
	if err != nil {
		res.Outcome, res.Conclusion = model.ConclusionInfraFailure, model.ConclusionInfraFailure
		res.Class = model.ClassInfra
		res.ClassReason = err.Error()
		e.platform(p.number, "post entrypoint could not run: "+err.Error())
		return res
	}
	o := e.exec(ctx, []string{node, path.Join(p.actionDir, p.script)}, p.env, e.cfg.WorkspaceDir, col, "run")
	col.flush()
	res.ExitCode = o.exitCode
	if o.failed() {
		d := e.cfg.Classifier.Classify(classifySignalFor(o, col))
		e.record(p.number, d)
		res.Class = d.Class
		res.Outcome = d.Class.Conclusion()
		res.Conclusion = res.Outcome
		res.ClassReason = d.String()
		return res
	}
	res.Outcome, res.Conclusion = model.ConclusionSuccess, model.ConclusionSuccess
	return res
}

// runComposite runs a composite action's steps in their own expression scope.
func (e *Executor) runComposite(ctx context.Context, spec protocol.StepSpec, meta *actions.Metadata, actionDir string, with map[string]string, col *collector, depth int) outcome {
	inputs, err := meta.InputValues(with)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}
	inputEnv, warnings, err := meta.InputEnv(with)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}
	for _, w := range warnings {
		col.emit("warning: " + w)
	}

	failed := false
	local := map[string]any{}
	sub := &scope{
		depth:     depth + 1,
		nested:    true,
		inputs:    inputs,
		steps:     local,
		actionDir: actionDir,
		env:       inputEnv,
		failed:    &failed,
	}

	for i, cs := range meta.Runs.Steps {
		if cs.Run != "" && strings.TrimSpace(cs.Shell) == "" {
			return outcome{exitCode: 1, phase: "run",
				err: fmt.Errorf("unsupported: composite action %q step %d has run: without the required shell:", spec.Uses, i+1)}
		}
		if cs.Run == "" && cs.Uses == "" {
			return outcome{exitCode: 1, phase: "run",
				err: fmt.Errorf("unsupported: composite action %q step %d has neither run: nor uses:", spec.Uses, i+1)}
		}
		nested := protocol.StepSpec{
			Number:           spec.Number,
			ID:               cs.ID,
			Name:             compositeStepName(spec, cs, i),
			IfExpr:           cs.If,
			Uses:             cs.Uses,
			Run:              cs.Run,
			With:             cs.With,
			Env:              cs.Env,
			Shell:            cs.Shell,
			WorkingDirectory: cs.WorkingDirectory,
			ContinueOnError:  cs.ContinueOnError,
			TimeoutMinutes:   cs.TimeoutMinutes,
		}
		r := e.runStep(ctx, nested, sub, depth+1)
		if cs.ID != "" {
			outputs := map[string]any{}
			for k, v := range r.Outputs {
				outputs[k] = v
			}
			local[cs.ID] = map[string]any{
				"outputs":    outputs,
				"outcome":    string(r.Outcome),
				"conclusion": string(r.Conclusion),
			}
		}
	}

	outputs, err := e.compositeOutputs(ctx, meta, sub)
	if err != nil {
		return outcome{exitCode: 1, err: err, phase: "run"}
	}
	if failed {
		return outcome{exitCode: 1, phase: "run", outputs: outputs,
			err: fmt.Errorf("composite action %q had a failing step", spec.Uses)}
	}
	return outcome{exitCode: 0, phase: "run", outputs: outputs}
}

// compositeOutputs evaluates the action's declared outputs against its own
// steps context.
func (e *Executor) compositeOutputs(ctx context.Context, meta *actions.Metadata, s *scope) (map[string]string, error) {
	if len(meta.Outputs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	var ev Evaluator
	for name, spec := range meta.Outputs {
		if spec.Value == "" {
			continue
		}
		if ev == nil {
			var err error
			if ev, err = e.evaluator(ctx, s.extraContexts()); err != nil {
				return nil, err
			}
		}
		v, err := ev.EvalString(spec.Value)
		if err != nil {
			return nil, fmt.Errorf("expression error: evaluating output %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func compositeStepName(parent protocol.StepSpec, cs actions.CompositeStep, i int) string {
	name := cs.Name
	if name == "" {
		name = cs.Uses
	}
	if name == "" {
		name = fmt.Sprintf("step %d", i+1)
	}
	return stepName(parent) + " > " + name
}
