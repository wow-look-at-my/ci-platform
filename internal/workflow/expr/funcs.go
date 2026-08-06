package expr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// arity holds the accepted argument counts of a built-in. max -1 is variadic.
type arity struct{ min, max int }

var builtins = map[string]arity{
	"contains":   {2, 2},
	"startswith": {2, 2},
	"endswith":   {2, 2},
	"format":     {1, -1},
	"join":       {1, 2},
	"tojson":     {1, 1},
	"fromjson":   {1, 1},
	"hashfiles":  {1, -1},
	"success":    {0, 0},
	"failure":    {0, 0},
	"always":     {0, 0},
	"cancelled":  {0, 0},
}

func (e *Evaluator) call(c callNode) (any, error) {
	name := strings.ToLower(c.name)
	a, known := builtins[name]
	if !known {
		return nil, fmt.Errorf("unrecognized function: '%s'", c.name)
	}
	if len(c.args) < a.min || (a.max >= 0 && len(c.args) > a.max) {
		return nil, fmt.Errorf("expression error: %s() expects %s, got %d", c.name, wantArgs(a), len(c.args))
	}
	args := make([]any, len(c.args))
	for i, an := range c.args {
		v, err := e.eval(an)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}

	switch name {
	case "success":
		return e.status.Success, nil
	case "failure":
		return e.status.Failure, nil
	case "always":
		return true, nil
	case "cancelled":
		return e.status.Cancelled, nil
	case "contains":
		return fnContains(args[0], args[1]), nil
	case "startswith":
		return affix(args[0], args[1], strings.HasPrefix), nil
	case "endswith":
		return affix(args[0], args[1], strings.HasSuffix), nil
	case "format":
		return fnFormat(stringify(args[0]), args[1:])
	case "join":
		return fnJoin(args), nil
	case "tojson":
		return fnToJSON(args[0])
	case "fromjson":
		return fnFromJSON(stringify(args[0]))
	case "hashfiles":
		pats := make([]string, len(args))
		for i, v := range args {
			pats[i] = stringify(v)
		}
		return e.hashFiles(pats)
	}
	return nil, fmt.Errorf("unrecognized function: '%s'", c.name)
}

func wantArgs(a arity) string {
	switch {
	case a.max < 0:
		return fmt.Sprintf("at least %d argument(s)", a.min)
	case a.min == a.max:
		return fmt.Sprintf("%d argument(s)", a.min)
	}
	return fmt.Sprintf("%d to %d arguments", a.min, a.max)
}

// fnContains is array membership when the haystack is an array and a
// case-insensitive substring test when both operands are primitives. An object
// haystack, or a non-primitive needle in a string search, is false: the
// reference never stringifies a container to satisfy these.
func fnContains(search, item any) bool {
	if arr, ok := asArray(search); ok {
		for _, e := range arr {
			if looseEqual(e, item) {
				return true
			}
		}
		return false
	}
	return affix(search, item, strings.Contains)
}

func affix(left, right any, test func(string, string) bool) bool {
	if !isPrimitive(left) || !isPrimitive(right) {
		return false
	}
	return test(strings.ToUpper(stringify(left)), strings.ToUpper(stringify(right)))
}

// fnFormat expands {0}-style placeholders; {{ and }} are literal braces.
func fnFormat(f string, args []any) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch c {
		case '{':
			if i+1 < len(f) && f[i+1] == '{' {
				b.WriteByte('{')
				i++
				continue
			}
			j := strings.IndexByte(f[i:], '}')
			if j < 0 {
				return "", fmt.Errorf("expression error: format() has an unclosed '{' at offset %d", i)
			}
			idxText := f[i+1 : i+j]
			if strings.Contains(idxText, ":") {
				return "", fmt.Errorf("unsupported: format() alignment and format specifiers (%q) are not implemented", "{"+idxText+"}")
			}
			n, err := strconv.Atoi(idxText)
			if err != nil {
				return "", fmt.Errorf("expression error: format() placeholder %q is not a number", "{"+idxText+"}")
			}
			if n < 0 || n >= len(args) {
				return "", fmt.Errorf("expression error: format() references {%d} but only %d argument(s) were supplied", n, len(args))
			}
			b.WriteString(stringify(args[n]))
			i += j
		case '}':
			if i+1 < len(f) && f[i+1] == '}' {
				b.WriteByte('}')
				i++
				continue
			}
			return "", fmt.Errorf("expression error: format() has an unescaped '}' at offset %d (write '}}' for a literal brace)", i)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// fnJoin joins an array; a primitive is returned as its own string and an
// object yields "". A non-primitive separator falls back to the default.
func fnJoin(args []any) string {
	arr, isArr := asArray(args[0])
	if !isArr {
		if isPrimitive(args[0]) {
			return stringify(args[0])
		}
		return ""
	}
	if len(arr) == 0 {
		return ""
	}
	sep := ","
	if len(args) == 2 && isPrimitive(args[1]) {
		sep = stringify(args[1])
	}
	parts := make([]string, len(arr))
	for i, e := range arr {
		parts[i] = stringify(e)
	}
	return strings.Join(parts, sep)
}

func fnToJSON(v any) (string, error) {
	b, err := json.MarshalIndent(jsonable(v), "", "  ")
	if err != nil {
		return "", fmt.Errorf("expression error: toJSON could not encode the value: %w", err)
	}
	return string(b), nil
}

func fnFromJSON(s string) (any, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, fmt.Errorf("expression error: fromJSON received an empty string")
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return nil, fmt.Errorf("expression error: fromJSON could not parse %q: %w", truncate(t, 80), err)
	}
	return normalize(v), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
