// Package plan turns a parsed workflow into the concrete set of jobs one run
// will execute: matrix legs expanded with GitHub Actions' include/exclude
// semantics, display names fixed byte-for-byte because branch protection
// matches on them, and the needs DAG validated and ordered.
package plan

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Input is everything Build needs besides the workflow itself.
type Input struct {
	Run      *model.Run
	Contexts map[string]any // github, vars, inputs, ...
	NewEval  EvaluatorFactory
}

// Plan is the concrete job set for one run.
type Plan struct {
	Jobs  []*PlannedJob
	Order []string // PlannedJob.ID values, topologically sorted

	// Workflow is the source, kept so the scheduler can resolve workflow-level
	// env and defaults when it builds an assignment.
	Workflow *model.Workflow
	// Contexts are the run-scoped contexts Build was given (github, vars,
	// inputs, ...), kept so the scheduler evaluates if: and step expressions
	// against exactly what the plan was built from.
	Contexts map[string]any
	// RunConcurrencyGroup is the workflow-level concurrency group, evaluated.
	RunConcurrencyGroup string
	RunCancelInProgress bool
}

// PlannedJob is one node of the run's DAG: one check run, one runner.
type PlannedJob struct {
	Key, Name, MatrixKey string
	Matrix               map[string]any
	// MatrixOrder is the key order used for the name and the matrix key.
	MatrixOrder []string
	Needs       []string
	Labels      []string
	// RunnerGroup is runs-on.group, empty when the workflow selects by label.
	RunnerGroup      string
	IR               *model.JobIR
	Retry            model.RetryPolicy
	ConcurrencyGroup string
	CancelInProgress bool
	FailFast         bool
	// MaxParallel caps concurrent legs of this job; 0 means no cap.
	MaxParallel int
	// MatrixSiblings holds the IDs of every leg of this job, including this
	// one, so fail-fast can find them. Nil for an unmatrixed job.
	MatrixSiblings []string

	ContinueOnError bool
	TimeoutMinutes  int
	Environment     string
	// LegIndex and LegTotal populate the strategy context.
	LegIndex, LegTotal int
}

// ID is the plan-unique identity of one leg: the job key for an unmatrixed
// job, key#matrix-key otherwise.
func (p *PlannedJob) ID() string {
	if p.MatrixKey == "" {
		return p.Key
	}
	return p.Key + "#" + p.MatrixKey
}

// Find returns the leg with this job key and matrix key, or nil.
func (p *Plan) Find(key, matrixKey string) *PlannedJob {
	for _, j := range p.Jobs {
		if j.Key == key && j.MatrixKey == matrixKey {
			return j
		}
	}
	return nil
}

// ByID returns the leg with this ID, or nil.
func (p *Plan) ByID(id string) *PlannedJob {
	for _, j := range p.Jobs {
		if j.ID() == id {
			return j
		}
	}
	return nil
}

// Legs returns every leg of one job key, in leg order.
func (p *Plan) Legs(key string) []*PlannedJob {
	var out []*PlannedJob
	for _, j := range p.Jobs {
		if j.Key == key {
			out = append(out, j)
		}
	}
	return out
}

// Build expands the workflow into a plan.
//
// Every expression reachable at plan time (name, runs-on, concurrency,
// max-parallel, the matrix itself) is evaluated here against the supplied
// contexts. Expressions that depend on a job's own needs outputs therefore
// cannot resolve yet and are reported as errors rather than silently emptied.
func Build(w *model.Workflow, in Input) (*Plan, error) {
	if w == nil {
		return nil, errors.New("plan: nil workflow")
	}
	if in.Run == nil {
		return nil, errors.New("plan: nil run")
	}
	if in.NewEval == nil {
		return nil, errors.New("plan: no evaluator factory")
	}
	order, err := TopoSort(w)
	if err != nil {
		return nil, err
	}

	p := &Plan{Workflow: w, Contexts: in.Contexts}
	base := in.NewEval(contextsWith(in.Contexts, nil, nil), Status{Success: true})

	if w.Concurrency != nil {
		p.RunConcurrencyGroup, err = EvalString(base, w.Concurrency.Group)
		if err != nil {
			return nil, fmt.Errorf("plan: workflow concurrency group: %w", err)
		}
		if p.RunConcurrencyGroup == "" {
			return nil, errors.New("plan: workflow concurrency group evaluated to an empty string")
		}
		p.RunCancelInProgress, err = EvalBool(base, w.Concurrency.CancelInProgress, false)
		if err != nil {
			return nil, fmt.Errorf("plan: workflow concurrency cancel-in-progress: %w", err)
		}
	}

	for _, key := range order {
		recordIncludeOrderDeviation(w, key)
		jobs, err := buildJob(w.Jobs[key], in, base)
		if err != nil {
			return nil, fmt.Errorf("plan: job %q: %w", key, err)
		}
		p.Jobs = append(p.Jobs, jobs...)
		for _, j := range jobs {
			p.Order = append(p.Order, j.ID())
		}
	}
	return p, nil
}

// recordIncludeOrderDeviation surfaces the one place a name can differ from
// GitHub's: an include key the parser did not record in Matrix.Order has no
// source order left in the IR, so its name segment is placed alphabetically.
func recordIncludeOrderDeviation(w *model.Workflow, key string) {
	ir := w.Jobs[key]
	if ir.Strategy == nil || ir.Strategy.Matrix == nil {
		return
	}
	m := ir.Strategy.Matrix
	var loose []string
	for _, inc := range m.Include {
		for k := range inc {
			if _, isDim := m.Dimensions[k]; isDim || containsString(m.Order, k) || containsString(loose, k) {
				continue
			}
			loose = append(loose, k)
		}
	}
	if len(loose) == 0 {
		return
	}
	sort.Strings(loose)
	w.Deviations = append(w.Deviations, model.Deviation{
		Path:        fmt.Sprintf("jobs.%s.strategy.matrix.include", key),
		GHABehavior: "include keys appear in the job name in the order they are written in the YAML",
		OurBehavior: fmt.Sprintf("the include-only keys %s are ordered alphabetically in the job name", strings.Join(loose, ", ")),
		Rationale:   "the IR stores an include entry as a map, so its source key order survives only if the parser records it in matrix Order",
	})
}

func buildJob(ir *model.JobIR, in Input, base Evaluator) ([]*PlannedJob, error) {
	failFast := true
	maxParallel := 0
	var legs []Leg
	if ir.Strategy != nil {
		if ir.Strategy.FailFast != nil {
			failFast = *ir.Strategy.FailFast
		}
		var err error
		maxParallel, err = EvalInt(base, ir.Strategy.MaxParallel, 0)
		if err != nil {
			return nil, fmt.Errorf("max-parallel: %w", err)
		}
		if maxParallel < 0 {
			return nil, fmt.Errorf("max-parallel is %d, which is not a count", maxParallel)
		}
		legs, err = ExpandMatrix(ir.Strategy.Matrix, base)
		if err != nil {
			return nil, err
		}
		if ir.Strategy.Matrix != nil && len(legs) == 0 {
			return nil, errors.New("matrix expanded to zero combinations, so the job would silently never run")
		}
	}

	retry := model.DefaultRetryPolicy()
	if ir.Retry != nil {
		retry = *ir.Retry
		if retry.Attempts < 1 {
			return nil, fmt.Errorf("retry policy allows %d attempts", retry.Attempts)
		}
		// Retrying a user failure manufactures a flaky green, and retrying a
		// config error cannot fix the YAML. Rejecting the policy is louder
		// than accepting it and quietly not honouring it.
		for _, c := range retry.On {
			if c != model.ClassInfra {
				return nil, fmt.Errorf("retry policy asks to retry %q failures; only infrastructure failures are ever retried", c)
			}
		}
	}

	total := len(legs)
	if total == 0 {
		total = 1
	}
	out := make([]*PlannedJob, 0, total)
	for i := 0; i < total; i++ {
		var leg *Leg
		if len(legs) > 0 {
			leg = &legs[i]
		}
		ev := in.NewEval(contextsWith(in.Contexts, leg, map[string]any{
			"fail-fast":    failFast,
			"job-index":    i,
			"job-total":    total,
			"max-parallel": maxParallel,
		}), Status{Success: true})

		pj := &PlannedJob{
			Key:         ir.Key,
			Needs:       append([]string(nil), ir.Needs...),
			IR:          ir,
			Retry:       retry,
			FailFast:    failFast,
			MaxParallel: maxParallel,
			LegIndex:    i,
			LegTotal:    total,
		}
		if leg != nil {
			pj.Matrix = leg.Values
			pj.MatrixOrder = leg.Order
			pj.MatrixKey = leg.Key()
		}

		var err error
		if pj.Name, err = DisplayName(ir.Key, ir.Name, leg, ev); err != nil {
			return nil, err
		}
		if pj.Labels, pj.RunnerGroup, err = resolveRunsOn(ir.RunsOn, ev); err != nil {
			return nil, err
		}
		if ir.Concurrency != nil {
			if pj.ConcurrencyGroup, err = EvalString(ev, ir.Concurrency.Group); err != nil {
				return nil, fmt.Errorf("concurrency group: %w", err)
			}
			if pj.ConcurrencyGroup == "" {
				return nil, errors.New("concurrency group evaluated to an empty string")
			}
			if pj.CancelInProgress, err = EvalBool(ev, ir.Concurrency.CancelInProgress, false); err != nil {
				return nil, fmt.Errorf("concurrency cancel-in-progress: %w", err)
			}
		}
		if pj.ContinueOnError, err = EvalBool(ev, ir.ContinueOnError, false); err != nil {
			return nil, fmt.Errorf("continue-on-error: %w", err)
		}
		if pj.TimeoutMinutes, err = EvalInt(ev, ir.TimeoutMinutes, 0); err != nil {
			return nil, fmt.Errorf("timeout-minutes: %w", err)
		}
		if pj.TimeoutMinutes < 0 {
			return nil, fmt.Errorf("timeout-minutes is %d", pj.TimeoutMinutes)
		}
		if ir.Environment != nil {
			if pj.Environment, err = EvalString(ev, ir.Environment.Name); err != nil {
				return nil, fmt.Errorf("environment: %w", err)
			}
		}
		out = append(out, pj)
	}

	if len(legs) > 0 {
		ids := make([]string, 0, len(out))
		for _, j := range out {
			ids = append(ids, j.ID())
		}
		for _, j := range out {
			j.MatrixSiblings = ids
		}
	}
	return out, nil
}

// resolveRunsOn evaluates the runner selector. A job that selects neither a
// label nor a group would match every runner, so it is rejected.
func resolveRunsOn(r model.RunsOn, ev Evaluator) ([]string, string, error) {
	labels := make([]string, 0, len(r.Labels))
	for _, e := range r.Labels {
		s, err := EvalString(ev, e)
		if err != nil {
			return nil, "", fmt.Errorf("runs-on: %w", err)
		}
		if s == "" {
			return nil, "", fmt.Errorf("runs-on: label %q evaluated to an empty string", e.Raw)
		}
		labels = append(labels, s)
	}
	group, err := EvalString(ev, r.Group)
	if err != nil {
		return nil, "", fmt.Errorf("runs-on group: %w", err)
	}
	if len(labels) == 0 && group == "" {
		return nil, "", errors.New("runs-on selects no labels and no runner group")
	}
	return labels, group, nil
}

// contextsWith copies the caller's contexts and adds the per-leg matrix and
// strategy contexts. The copy is shallow: the caller's map is never mutated.
func contextsWith(in map[string]any, leg *Leg, strategy map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	if leg != nil {
		out["matrix"] = leg.Values
	} else if _, ok := out["matrix"]; !ok {
		out["matrix"] = map[string]any{}
	}
	if strategy != nil {
		out["strategy"] = strategy
	}
	return out
}
