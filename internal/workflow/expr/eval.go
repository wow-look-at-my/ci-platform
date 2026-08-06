package expr

import (
	"fmt"
	"strings"
)

func (e *Evaluator) eval(n node) (any, error) {
	switch x := n.(type) {
	case litNode:
		return x.v, nil
	case nameNode:
		v, ok := lookupFold(e.ctx, x.name)
		if !ok {
			return nil, fmt.Errorf("unrecognized named-value: '%s'", x.name)
		}
		return normalize(v), nil
	case propNode:
		obj, err := e.eval(x.x)
		if err != nil {
			return nil, err
		}
		return getProp(obj, x.name), nil
	case indexNode:
		obj, err := e.eval(x.x)
		if err != nil {
			return nil, err
		}
		if x.i == nil {
			return getProp(obj, "*"), nil
		}
		idx, err := e.eval(x.i)
		if err != nil {
			return nil, err
		}
		return getIndex(obj, idx), nil
	case unaryNode:
		v, err := e.eval(x.x)
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil
	case binNode:
		return e.evalBinary(x)
	case callNode:
		return e.call(x)
	}
	return nil, fmt.Errorf("invalid expression: unsupported node %T", n)
}

func (e *Evaluator) evalBinary(x binNode) (any, error) {
	l, err := e.eval(x.l)
	if err != nil {
		return nil, err
	}
	// && and || short-circuit and yield an OPERAND, not a boolean.
	switch x.op {
	case "&&":
		if !truthy(l) {
			return l, nil
		}
		return e.eval(x.r)
	case "||":
		if truthy(l) {
			return l, nil
		}
		return e.eval(x.r)
	}
	r, err := e.eval(x.r)
	if err != nil {
		return nil, err
	}
	switch x.op {
	case "==":
		return looseEqual(l, r), nil
	case "!=":
		return !looseEqual(l, r), nil
	}
	c, ok := compareValues(l, r)
	if !ok {
		return false, nil
	}
	switch x.op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return nil, fmt.Errorf("invalid expression: unknown operator %q", x.op)
}

// lookupFold resolves a context or property name, exact match first. GHA
// resolves both case-insensitively.
func lookupFold(m map[string]any, name string) (any, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// getProp never errors: a missing property is null, matching GHA.
func getProp(obj any, name string) any {
	obj = normalize(obj)
	if name == "*" {
		switch x := obj.(type) {
		case map[string]any:
			out := make(filtered, 0, len(x))
			for _, k := range sortedKeys(x) {
				out = append(out, normalize(x[k]))
			}
			return out
		case []any:
			out := make(filtered, 0, len(x))
			for _, e := range x {
				out = append(out, normalize(e))
			}
			return out
		case filtered:
			// A wildcard on an already-filtered array flattens one level.
			out := make(filtered, 0, len(x))
			for _, e := range x {
				if inner, ok := asArray(e); ok {
					out = append(out, inner...)
					continue
				}
				out = append(out, normalize(e))
			}
			return out
		}
		return nil
	}
	if f, ok := obj.(filtered); ok {
		out := make(filtered, 0, len(f))
		for _, e := range f {
			if m, ok := asMap(e); ok {
				if v, ok := lookupFold(m, name); ok {
					out = append(out, normalize(v))
				}
			}
		}
		return out
	}
	if m, ok := obj.(map[string]any); ok {
		if v, ok := lookupFold(m, name); ok {
			return normalize(v)
		}
	}
	return nil
}

// getIndex never errors either: out of range and wrong-kind both yield null.
func getIndex(obj, idx any) any {
	obj, idx = normalize(obj), normalize(idx)
	if s, ok := idx.(string); ok {
		if m, ok := asMap(obj); ok {
			if v, ok := lookupFold(m, s); ok {
				return normalize(v)
			}
		}
		return nil
	}
	arr, isArr := asArray(obj)
	if !isArr {
		return nil
	}
	n := toNumber(idx)
	i := int(n)
	if float64(i) != n || i < 0 || i >= len(arr) {
		return nil
	}
	return normalize(arr[i])
}
