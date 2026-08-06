package plan

import (
	"fmt"
	"strings"
)

// fakeEval is a deliberately small stand-in for workflow/expr: it resolves
// dotted context paths, the four status functions, and a table of canned
// answers for expressions (fromJSON and friends) that a real evaluator would
// compute.
type fakeEval struct {
	contexts map[string]any
	status   Status
	canned   map[string]any
}

func newFakeFactory(canned map[string]any) EvaluatorFactory {
	return func(contexts map[string]any, status Status) Evaluator {
		return &fakeEval{contexts: contexts, status: status, canned: canned}
	}
}

func (f *fakeEval) Eval(raw string) (any, error) {
	inner := strings.TrimSpace(raw)
	if strings.HasPrefix(inner, "${{") && strings.HasSuffix(inner, "}}") {
		inner = strings.TrimSpace(inner[3 : len(inner)-2])
	}
	if v, ok := f.canned[inner]; ok {
		return v, nil
	}
	switch inner {
	case "success()":
		return f.status.Success, nil
	case "failure()":
		return f.status.Failure, nil
	case "cancelled()":
		return f.status.Cancelled, nil
	case "always()":
		return true, nil
	}
	return f.lookup(inner)
}

func (f *fakeEval) lookup(path string) (any, error) {
	var cur any = f.contexts
	for _, part := range strings.Split(path, ".") {
		obj, ok := asObject(cur)
		if !ok {
			return nil, fmt.Errorf("unrecognized named-value %q", path)
		}
		cur, ok = obj[part]
		if !ok {
			return nil, fmt.Errorf("unrecognized named-value %q", path)
		}
	}
	return cur, nil
}

func (f *fakeEval) EvalString(raw string) (string, error) {
	var b strings.Builder
	rest := raw
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("invalid expression %q", raw)
		}
		b.WriteString(rest[:i])
		v, err := f.Eval(rest[i : i+j+2])
		if err != nil {
			return "", err
		}
		b.WriteString(RenderValue(v))
		rest = rest[i+j+2:]
	}
}

func (f *fakeEval) EvalBool(raw string) (bool, error) {
	v, err := f.Eval(raw)
	if err != nil {
		return false, err
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		return t != "" && t != "false", nil
	case nil:
		return false, nil
	}
	n, ok := asFloat(v)
	if ok {
		return n != 0, nil
	}
	return true, nil
}
