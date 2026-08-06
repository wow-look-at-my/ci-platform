package plan

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Leg is one expanded matrix combination. Order is the key order used for the
// display name and the matrix key, so both are deterministic: declared
// dimensions in declaration order, then include-only keys sorted.
type Leg struct {
	Values map[string]any
	Order  []string
}

// Key renders the stable per-leg identity, "os=ubuntu,go=1.22".
func (l Leg) Key() string {
	parts := make([]string, 0, len(l.Order))
	for _, k := range l.Order {
		parts = append(parts, k+"="+RenderValue(l.Values[k]))
	}
	return strings.Join(parts, ",")
}

// Suffix renders the GHA display suffix, "(ubuntu, 1.22)".
func (l Leg) Suffix() string {
	parts := make([]string, 0, len(l.Order))
	for _, k := range l.Order {
		parts = append(parts, RenderValue(l.Values[k]))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ExpandMatrix produces the legs of a strategy matrix in GHA's order and with
// GHA's include/exclude semantics. A nil matrix yields a single nil leg,
// meaning "one unmatrixed job".
func ExpandMatrix(m *model.Matrix, ev Evaluator) ([]Leg, error) {
	if m == nil {
		return nil, nil
	}
	resolved, err := resolveMatrix(m, ev)
	if err != nil {
		return nil, err
	}
	dims, err := dimensionOrder(resolved)
	if err != nil {
		return nil, err
	}

	// Cartesian product, first dimension outermost so the last one varies
	// fastest. Declaration order is what makes leg numbering stable.
	var combos []map[string]any
	if len(dims) > 0 {
		combos = []map[string]any{{}}
		for _, k := range dims {
			vals, err := dimensionValues(resolved.Dimensions[k], ev)
			if err != nil {
				return nil, fmt.Errorf("matrix dimension %q: %w", k, err)
			}
			if len(vals) == 0 {
				return nil, fmt.Errorf("matrix dimension %q has no values", k)
			}
			next := make([]map[string]any, 0, len(combos)*len(vals))
			for _, c := range combos {
				for _, v := range vals {
					n := cloneValues(c)
					n[k] = v
					next = append(next, n)
				}
			}
			combos = next
		}
	}

	for _, ex := range resolved.Exclude {
		if len(ex) == 0 {
			return nil, fmt.Errorf("matrix exclude has an empty entry, which would exclude everything")
		}
		kept := combos[:0]
		for _, c := range combos {
			if !matchesAll(c, ex) {
				kept = append(kept, c)
			}
		}
		combos = kept
	}

	isDim := make(map[string]bool, len(dims))
	for _, k := range dims {
		isDim[k] = true
	}

	// Original dimension values are never overwritten by an include, so the
	// conflict test reads this snapshot rather than the mutated combination.
	base := make([]map[string]any, len(combos))
	for i, c := range combos {
		base[i] = cloneValues(c)
	}

	var appended []map[string]any
	for _, inc := range resolved.Include {
		if len(inc) == 0 {
			return nil, fmt.Errorf("matrix include has an empty entry")
		}
		matched := false
		for i, c := range combos {
			conflict := false
			for k, v := range inc {
				if !isDim[k] {
					continue
				}
				bv, ok := base[i][k]
				if !ok || !sameValue(bv, v) {
					conflict = true
					break
				}
			}
			if conflict {
				continue
			}
			matched = true
			for k, v := range inc {
				c[k] = v
			}
		}
		// An include that fits no original combination becomes its own leg. It
		// is not a candidate for later includes, matching GHA.
		if !matched {
			appended = append(appended, cloneValues(inc))
		}
	}

	all := append(combos, appended...)
	legs := make([]Leg, 0, len(all))
	for _, c := range all {
		legs = append(legs, Leg{Values: c, Order: legOrder(c, dims)})
	}
	return legs, nil
}

// legOrder lists a leg's keys: declared dimensions in declaration order, then
// keys an include added, sorted. Include key order is not recoverable from the
// IR (a map), so sorting is what makes the name deterministic.
func legOrder(values map[string]any, dims []string) []string {
	order := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, k := range dims {
		if _, ok := values[k]; ok {
			order = append(order, k)
			seen[k] = true
		}
	}
	extra := make([]string, 0, len(values))
	for k := range values {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// dimensionOrder returns the declared dimensions in declaration order, and
// fails loudly when Order and Dimensions disagree: a dimension missing from
// Order would otherwise silently reorder every leg name.
func dimensionOrder(m *model.Matrix) ([]string, error) {
	if len(m.Dimensions) == 0 {
		return nil, nil
	}
	if len(m.Order) != len(m.Dimensions) {
		return nil, fmt.Errorf("matrix declares %d dimensions but the order lists %d; leg names depend on the order",
			len(m.Dimensions), len(m.Order))
	}
	seen := make(map[string]bool, len(m.Order))
	for _, k := range m.Order {
		if _, ok := m.Dimensions[k]; !ok {
			return nil, fmt.Errorf("matrix order names %q, which is not a declared dimension", k)
		}
		if seen[k] {
			return nil, fmt.Errorf("matrix order names %q twice", k)
		}
		seen[k] = true
	}
	return m.Order, nil
}

// dimensionValues evaluates a dimension's values. A string value containing an
// expression is evaluated; one that evaluates to a list splices into the
// dimension, which is how `${{ fromJSON(needs.x.outputs.y) }}` works.
func dimensionValues(raw []any, ev Evaluator) ([]any, error) {
	out := make([]any, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || model.NewExpr(s).IsLiteral() {
			out = append(out, v)
			continue
		}
		if ev == nil {
			return nil, fmt.Errorf("value %q needs an evaluator and none was supplied", s)
		}
		got, err := ev.Eval(s)
		if err != nil {
			return nil, err
		}
		if list, ok := asList(got); ok {
			out = append(out, list...)
			continue
		}
		out = append(out, got)
	}
	return out, nil
}

// resolveMatrix turns a whole-matrix `${{ fromJSON(...) }}` into a concrete
// matrix. Anything else is returned unchanged.
func resolveMatrix(m *model.Matrix, ev Evaluator) (*model.Matrix, error) {
	if m.FromExpr.Empty() {
		return m, nil
	}
	if ev == nil {
		return nil, fmt.Errorf("matrix %q needs an evaluator and none was supplied", m.FromExpr.Raw)
	}
	v, err := ev.Eval(m.FromExpr.Raw)
	if err != nil {
		return nil, fmt.Errorf("matrix expression %q: %w", m.FromExpr.Raw, err)
	}
	obj, ok := asObject(v)
	if !ok {
		return nil, fmt.Errorf("matrix expression %q evaluated to %T, which is not an object of dimensions", m.FromExpr.Raw, v)
	}
	out := &model.Matrix{Dimensions: map[string][]any{}}
	var extra []string
	for k, val := range obj {
		switch k {
		case "include", "exclude":
			entries, err := asEntryList(val)
			if err != nil {
				return nil, fmt.Errorf("matrix expression %q: %s: %w", m.FromExpr.Raw, k, err)
			}
			if k == "include" {
				out.Include = entries
			} else {
				out.Exclude = entries
			}
		default:
			list, ok := asList(val)
			if !ok {
				return nil, fmt.Errorf("matrix expression %q: dimension %q is %T, not a list", m.FromExpr.Raw, k, val)
			}
			out.Dimensions[k] = list
			extra = append(extra, k)
		}
	}
	// A JSON object has no key order, so declared Order wins where it exists
	// and the rest is sorted.
	sort.Strings(extra)
	for _, k := range m.Order {
		if _, ok := out.Dimensions[k]; ok {
			out.Order = append(out.Order, k)
		}
	}
	for _, k := range extra {
		if !containsString(out.Order, k) {
			out.Order = append(out.Order, k)
		}
	}
	return out, nil
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func asList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func asObject(v any) (map[string]any, bool) {
	switch o := v.(type) {
	case map[string]any:
		return o, true
	case map[any]any:
		out := make(map[string]any, len(o))
		for k, val := range o {
			s, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[s] = val
		}
		return out, true
	}
	return nil, false
}

func asEntryList(v any) ([]map[string]any, error) {
	list, ok := asList(v)
	if !ok {
		return nil, fmt.Errorf("expected a list, got %T", v)
	}
	out := make([]map[string]any, 0, len(list))
	for i, e := range list {
		o, ok := asObject(e)
		if !ok {
			return nil, fmt.Errorf("entry %d is %T, not an object", i, e)
		}
		out = append(out, o)
	}
	return out, nil
}

func cloneValues(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// matchesAll reports whether every key/value of entry is present and equal in
// combo, which is the partial match `exclude` uses.
func matchesAll(combo, entry map[string]any) bool {
	for k, v := range entry {
		got, ok := combo[k]
		if !ok || !sameValue(got, v) {
			return false
		}
	}
	return true
}

// sameValue compares matrix values across the numeric types YAML and JSON
// produce for the same literal.
func sameValue(a, b any) bool {
	if af, aok := asFloat(a); aok {
		if bf, bok := asFloat(b); bok {
			return af == bf
		}
		return false
	}
	return reflect.DeepEqual(a, b)
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
