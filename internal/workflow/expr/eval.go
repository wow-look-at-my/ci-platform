package expr

import (
	"fmt"
	"math"
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
		if x.name == "*" {
			return indexValue(obj, nil, true), nil
		}
		return indexValue(obj, x.name, false), nil
	case indexNode:
		obj, err := e.eval(x.x)
		if err != nil {
			return nil, err
		}
		if x.i == nil {
			return indexValue(obj, nil, true), nil
		}
		idx, err := e.eval(x.i)
		if err != nil {
			return nil, err
		}
		return indexValue(obj, idx, false), nil
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

// indexValue is both `obj.name` and `obj[idx]`; the reference implementation
// compiles property access into an index by a string literal, so they share
// every rule. It never errors: a missing key, an out-of-range index and a
// wrong-kind operand are all null.
//
// The one exception is a wildcard against a non-collection, which yields an
// EMPTY array rather than null, so `'str'.*` and `github.missing.*` are both
// safe to keep indexing into.
func indexValue(obj, idx any, wildcard bool) any {
	obj = normalize(obj)
	switch x := obj.(type) {
	case filtered:
		return indexFiltered(x, idx, wildcard)
	case map[string]any:
		if wildcard {
			out := make(filtered, 0, len(x))
			// The reference iterates in insertion order, which a Go map does
			// not have; sorting is the only deterministic choice available.
			for _, k := range sortedKeys(x) {
				out = append(out, normalize(x[k]))
			}
			return out
		}
		return objectIndex(x, idx)
	case []any:
		if wildcard {
			out := make(filtered, 0, len(x))
			for _, e := range x {
				out = append(out, normalize(e))
			}
			return out
		}
		return arrayIndex(x, idx)
	}
	if wildcard {
		return filtered{}
	}
	return nil
}

// indexFiltered applies the index to every ELEMENT of a filtered array, which
// is why `commits.*.message[0]` is not the first message: the [0] is applied to
// each message, and a string has no element 0.
func indexFiltered(f filtered, idx any, wildcard bool) any {
	out := filtered{}
	for _, item := range f {
		switch x := normalize(item).(type) {
		case map[string]any:
			if wildcard {
				for _, k := range sortedKeys(x) {
					out = append(out, normalize(x[k]))
				}
				continue
			}
			if v := objectIndex(x, idx); v != nil {
				out = append(out, v)
			}
		case []any:
			if wildcard {
				for _, e := range x {
					out = append(out, normalize(e))
				}
				continue
			}
			if v := arrayIndex(x, idx); v != nil {
				out = append(out, v)
			}
		case filtered:
			if wildcard {
				out = append(out, x...)
				continue
			}
			if v := arrayIndex(x, idx); v != nil {
				out = append(out, v)
			}
		}
	}
	return out
}

// objectIndex looks up by the index's string form; any primitive index works,
// so obj[0] reads the key "0".
func objectIndex(m map[string]any, idx any) any {
	if !isPrimitive(idx) {
		return nil
	}
	if v, ok := lookupFold(m, stringify(idx)); ok {
		return normalize(v)
	}
	return nil
}

// arrayIndex floors a non-integer index, so arr[1.1] is arr[1].
func arrayIndex(arr []any, idx any) any {
	n := toNumber(idx)
	if math.IsNaN(n) || n < 0 || n > math.MaxInt32 {
		return nil
	}
	i := int(math.Floor(n))
	if i >= len(arr) {
		return nil
	}
	return normalize(arr[i])
}

func isPrimitive(v any) bool {
	switch normalize(v).(type) {
	case nil, bool, float64, string:
		return true
	}
	return false
}
