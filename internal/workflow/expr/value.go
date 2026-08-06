package expr

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// filtered is the result of an array filter (`x.*` or `x[*]`). It behaves as an
// array everywhere except property access, where it maps over its elements.
type filtered []any

// normalize maps arbitrary Go values from the caller's Context onto the five
// kinds the language has: null, bool, number, string, and array/object.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, string, float64, map[string]any, []any, filtered:
		return v
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return string(x)
		}
		return f
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Sprint(v)
		}
		m := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			m[k.String()] = rv.MapIndex(k).Interface()
		}
		return m
	case reflect.Slice, reflect.Array:
		a := make([]any, rv.Len())
		for i := range a {
			a[i] = rv.Index(i).Interface()
		}
		return a
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return normalize(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

// asArray returns the elements of an array-ish value.
func asArray(v any) ([]any, bool) {
	switch x := normalize(v).(type) {
	case []any:
		return x, true
	case filtered:
		return x, true
	}
	return nil, false
}

func asMap(v any) (map[string]any, bool) {
	m, ok := normalize(v).(map[string]any)
	return m, ok
}

// truthy follows the GHA loose rules: only null, false, 0, NaN and "" are false.
func truthy(v any) bool {
	switch x := normalize(v).(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// toNumber casts for comparison. Arrays, objects and unparseable strings become
// NaN, which makes every comparison involving them false.
func toNumber(v any) float64 {
	switch x := normalize(v).(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return n
		}
		if len(s) > 2 && (strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")) {
			if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
				return float64(n)
			}
		}
		return math.NaN()
	}
	return math.NaN()
}

// looseEqual implements ==. Same-kind values compare directly (strings
// case-insensitively, containers by identity); different kinds are cast to
// number, which is what makes null == false == 0 == "" all true.
func looseEqual(a, b any) bool {
	a, b = normalize(a), normalize(b)
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return strings.EqualFold(as, bs)
	}
	if isContainer(a) || isContainer(b) {
		if isContainer(a) && isContainer(b) {
			return sameContainer(a, b)
		}
		// A container compared against a scalar casts to NaN, never equal.
		return false
	}
	if a == nil && b == nil {
		return true
	}
	ab, aIsBool := a.(bool)
	bb, bIsBool := b.(bool)
	if aIsBool && bIsBool {
		return ab == bb
	}
	an, bn := toNumber(a), toNumber(b)
	if math.IsNaN(an) || math.IsNaN(bn) {
		return false
	}
	return an == bn
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any, filtered:
		return true
	}
	return false
}

// sameContainer reports reference identity, matching GHA where two distinct
// objects with the same contents are not equal.
func sameContainer(a, b any) bool {
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Kind() != bv.Kind() {
		return false
	}
	switch av.Kind() {
	case reflect.Map:
		return av.UnsafePointer() == bv.UnsafePointer()
	case reflect.Slice:
		return av.Len() == bv.Len() && av.UnsafePointer() == bv.UnsafePointer()
	}
	return false
}

// compareValues returns -1, 0 or 1, and false when the values are not
// comparable (NaN), which makes <, <=, > and >= all false.
func compareValues(a, b any) (int, bool) {
	a, b = normalize(a), normalize(b)
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return strings.Compare(strings.ToLower(as), strings.ToLower(bs)), true
	}
	an, bn := toNumber(a), toNumber(b)
	if math.IsNaN(an) || math.IsNaN(bn) {
		return 0, false
	}
	switch {
	case an < bn:
		return -1, true
	case an > bn:
		return 1, true
	}
	return 0, true
}

// stringify renders a value for interpolation. Containers render as JSON, which
// is a deliberate divergence from GHA's literal "Object"/"Array".
func stringify(v any) string {
	switch x := normalize(v).(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return formatNumber(x)
	case string:
		return x
	}
	b, err := json.Marshal(jsonable(v))
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func formatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == math.Trunc(f) && math.Abs(f) < 1e21:
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsonable converts a value into something encoding/json renders the way GHA
// does, notably unwrapping filtered arrays and normalizing nested numbers.
func jsonable(v any) any {
	switch x := normalize(v).(type) {
	case nil, bool, string:
		return x
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonable(e)
		}
		return out
	case filtered:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonable(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonable(e)
		}
		return out
	}
	return fmt.Sprint(v)
}

// sortedKeys gives object wildcard expansion a deterministic order; Go map
// iteration order is random and leg naming downstream depends on stability.
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
