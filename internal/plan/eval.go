package plan

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Evaluator evaluates workflow expressions. Satisfied by workflow/expr.Evaluator.
type Evaluator interface {
	EvalString(raw string) (string, error)
	EvalBool(raw string) (bool, error)
	Eval(raw string) (any, error)
}

// EvaluatorFactory builds an Evaluator over a set of named contexts
// ("github", "env", "needs", "matrix", "strategy", "vars", "secrets", "inputs", ...).
type EvaluatorFactory func(contexts map[string]any, status Status) Evaluator

// Status is what success(), failure() and cancelled() answer inside an
// expression. Exactly one of the three is true for a given evaluation.
type Status struct{ Success, Failure, Cancelled bool }

// EvalString evaluates an Expr to text, short-circuiting literals so a literal
// never needs an evaluator at all.
func EvalString(ev Evaluator, e model.Expr) (string, error) {
	if e.Empty() || e.IsLiteral() {
		return e.Raw, nil
	}
	if ev == nil {
		return "", fmt.Errorf("expression %q needs an evaluator and none was supplied", e.Raw)
	}
	return ev.EvalString(e.Raw)
}

// EvalBool evaluates an Expr to a boolean, returning def only when the
// expression is absent. A present-but-unparseable literal is an error, never
// the default: "faIse" must not read as false.
func EvalBool(ev Evaluator, e model.Expr, def bool) (bool, error) {
	if e.Empty() {
		return def, nil
	}
	if e.IsLiteral() {
		switch strings.ToLower(strings.TrimSpace(e.Raw)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("expected true or false, got %q", e.Raw)
	}
	if ev == nil {
		return false, fmt.Errorf("expression %q needs an evaluator and none was supplied", e.Raw)
	}
	return ev.EvalBool(e.Raw)
}

// EvalInt evaluates an Expr to an integer, returning def only when absent.
func EvalInt(ev Evaluator, e model.Expr, def int) (int, error) {
	if e.Empty() {
		return def, nil
	}
	if e.IsLiteral() {
		n, err := strconv.Atoi(strings.TrimSpace(e.Raw))
		if err != nil {
			return 0, fmt.Errorf("expected a number, got %q", e.Raw)
		}
		return n, nil
	}
	if ev == nil {
		return 0, fmt.Errorf("expression %q needs an evaluator and none was supplied", e.Raw)
	}
	v, err := ev.Eval(e.Raw)
	if err != nil {
		return 0, err
	}
	n, ok := toInt(v)
	if !ok {
		return 0, fmt.Errorf("expression %q evaluated to %v, which is not a number", e.Raw, v)
	}
	return n, nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// EvalStringMap evaluates every value of a map of expressions. Key order is
// irrelevant to the result, so the map is returned as a map.
func EvalStringMap(ev Evaluator, in map[string]model.Expr) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, e := range in {
		v, err := EvalString(ev, e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}
