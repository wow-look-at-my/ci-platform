package scheduler

import (
	"fmt"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/plan"
)

// fakeEval is a small stand-in for workflow/expr: dotted context paths, the
// four status functions, and `a == 'b'` comparisons, which is everything these
// tests put in an if:.
type fakeEval struct {
	contexts map[string]any
	status   plan.Status
}

func fakeFactory(contexts map[string]any, status plan.Status) plan.Evaluator {
	return &fakeEval{contexts: contexts, status: status}
}

func (f *fakeEval) strip(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") {
		s = strings.TrimSpace(s[3 : len(s)-2])
	}
	return s
}

func (f *fakeEval) Eval(raw string) (any, error) {
	s := f.strip(raw)
	switch s {
	case "success()":
		return f.status.Success, nil
	case "failure()":
		return f.status.Failure, nil
	case "cancelled()":
		return f.status.Cancelled, nil
	case "always()":
		return true, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if lit, ok := literal(s); ok {
		return lit, nil
	}
	return f.lookup(s)
}

func literal(s string) (string, bool) {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1], true
	}
	return "", false
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

func asObject(v any) (map[string]any, bool) {
	switch o := v.(type) {
	case map[string]any:
		return o, true
	case map[string]string:
		out := make(map[string]any, len(o))
		for k, val := range o {
			out[k] = val
		}
		return out, true
	}
	return nil, false
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
		b.WriteString(plan.RenderValue(v))
		rest = rest[i+j+2:]
	}
}

func (f *fakeEval) EvalBool(raw string) (bool, error) {
	s := f.strip(raw)
	if lhs, rhs, ok := strings.Cut(s, "=="); ok {
		l, err := f.Eval(strings.TrimSpace(lhs))
		if err != nil {
			return false, err
		}
		r, err := f.Eval(strings.TrimSpace(rhs))
		if err != nil {
			return false, err
		}
		return plan.RenderValue(l) == plan.RenderValue(r), nil
	}
	if and := strings.SplitN(s, "&&", 2); len(and) == 2 {
		l, err := f.EvalBool(strings.TrimSpace(and[0]))
		if err != nil || !l {
			return false, err
		}
		return f.EvalBool(strings.TrimSpace(and[1]))
	}
	v, err := f.Eval(s)
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
	return true, nil
}
