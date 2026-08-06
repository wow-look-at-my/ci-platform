package expr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func testCtx() Context {
	return Context{
		"github": map[string]any{
			"ref":        "refs/heads/main",
			"ref_name":   "main",
			"head_ref":   "",
			"event_name": "push",
			"run_number": 42,
			"event": map[string]any{
				"commits": []any{
					map[string]any{"message": "first", "id": "aaa"},
					map[string]any{"message": "second", "id": "bbb"},
					map[string]any{"id": "ccc"}, // no message: filtered out
				},
				"number": 7,
			},
		},
		"env":    map[string]any{"FOO": "bar", "EMPTY": ""},
		"matrix": map[string]any{"os": "ubuntu-latest", "go": 1.24},
		"needs": map[string]any{
			"build": map[string]any{"outputs": map[string]any{"tag": "v1.2.3"}, "result": "success"},
		},
		"inputs":   map[string]any{"debug": true, "count": 3},
		"vars":     map[string]any{},
		"nothing":  nil,
		"numbers":  []any{10, 20, 30},
		"emptyStr": "",
	}
}

func evalOK(t *testing.T, src string) any {
	t.Helper()
	v, err := New(testCtx()).Eval(src)
	require.NoError(t, err, "evaluating %q", src)
	return v
}

func TestLiterals(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"1", 1.0},
		{"0", 0.0},
		{"-3", -3.0},
		{"1.5", 1.5},
		{"-0.25", -0.25},
		{"0x1f", 31.0},
		{"0X10", 16.0},
		{"-0xff", -255.0},
		{"1e3", 1000.0},
		{"1.5e-2", 0.015},
		{"'hello'", "hello"},
		{"''", ""},
		{"'it''s'", "it's"},
		{"'a}}b'", "a}}b"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			require.Equal(t, c.want, evalOK(t, c.src))
		})
	}
}

func TestLogicalOperatorsReturnOperands(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"'a' || 'b'", "a"},
		{"'' || 'b'", "b"},
		{"null || 'fallback'", "fallback"},
		{"'a' && 'b'", "b"},
		{"'' && 'b'", ""},
		{"false && 'b'", false},
		{"0 || 3", 3.0},
		{"github.head_ref || github.ref_name", "main"},
		// && binds tighter than ||
		{"false || true && 'x'", "x"},
		{"true || 'a' && 'b'", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			require.Equal(t, c.want, evalOK(t, c.src))
		})
	}
}

func TestShortCircuitSkipsErrors(t *testing.T) {
	// The right operand is never evaluated, so its unknown named-value never
	// becomes an error.
	v, err := New(testCtx()).Eval("true || nosuchcontext.x")
	require.NoError(t, err)
	require.Equal(t, true, v)

	v, err = New(testCtx()).Eval("false && nosuchcontext.x")
	require.NoError(t, err)
	require.Equal(t, false, v)
}

func TestLooseEquality(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"null == false", true},
		{"null == 0", true},
		{"null == ''", true},
		{"false == 0", true},
		{"false == ''", true},
		{"0 == ''", true},
		{"'1' == 1", true},
		{"'1.0' == 1", true},
		{"' 2 ' == 2", true},
		{"'0x10' == 16", true},
		{"'abc' == 'abc'", true},
		{"'ABC' == 'abc'", true},
		{"'ABC' != 'abd'", true},
		{"'abc' == 1", false},
		{"'abc' != 1", true},
		{"true == 1", true},
		{"true == 'true'", false}, // 'true' casts to NaN
		{"1 == 1.0", true},
		{"null == null", true},
		{"'' == ' '", false}, // both strings, so compared as text, not as numbers
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			require.Equal(t, c.want, evalOK(t, c.src))
		})
	}
}

func TestContainerEquality(t *testing.T) {
	// Two distinct containers are never equal, even with identical contents.
	v := evalOK(t, "fromJSON('[1,2]') == fromJSON('[1,2]')")
	require.Equal(t, false, v)
	// A container compared against a scalar is never equal.
	require.Equal(t, false, evalOK(t, "fromJSON('[1]') == 1"))
	require.Equal(t, false, evalOK(t, "fromJSON('{}') == ''"))
	// Same instance is equal.
	require.Equal(t, true, evalOK(t, "github.event == github.event"))
}

func TestComparison(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 2", true},
		{"3 >= 4", false},
		{"'1' < 2", true}, // mixed kinds cast to number
		{"'a' < 'b'", true},
		{"'B' > 'a'", true}, // case-insensitive ordinal
		{"'abc' < 1", false},
		{"'abc' > 1", false}, // NaN makes every comparison false
		{"'abc' >= 1", false},
		{"null < 1", true},
		{"github.run_number > 41", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			require.Equal(t, c.want, evalOK(t, c.src))
		})
	}
}

func TestNot(t *testing.T) {
	require.Equal(t, false, evalOK(t, "!true"))
	require.Equal(t, true, evalOK(t, "!''"))
	require.Equal(t, true, evalOK(t, "!null"))
	require.Equal(t, false, evalOK(t, "!'x'"))
	require.Equal(t, true, evalOK(t, "!true == false"))
	require.Equal(t, true, evalOK(t, "!(1 == 2)"))
}

func TestPropertyAndIndex(t *testing.T) {
	require.Equal(t, "main", evalOK(t, "github.ref_name"))
	require.Equal(t, "main", evalOK(t, "github['ref_name']"))
	require.Equal(t, "v1.2.3", evalOK(t, "needs.build.outputs.tag"))
	require.Equal(t, "bar", evalOK(t, "env.FOO"))
	require.Equal(t, "bar", evalOK(t, "env.foo"), "property lookup is case-insensitive")
	require.Equal(t, "bar", evalOK(t, "ENV.FOO"), "context lookup is case-insensitive")
	require.Equal(t, 20.0, evalOK(t, "numbers[1]"))
	require.Equal(t, "first", evalOK(t, "github.event.commits[0].message"))

	// Missing and out-of-range are null, never an error.
	require.Nil(t, evalOK(t, "github.nope"))
	require.Nil(t, evalOK(t, "github.nope.deeper.still"))
	require.Nil(t, evalOK(t, "numbers[99]"))
	require.Nil(t, evalOK(t, "numbers[-1]"))
	require.Nil(t, evalOK(t, "numbers['x']"))
	require.Nil(t, evalOK(t, "github['nope']"))
	require.Nil(t, evalOK(t, "nothing.anything"))
}

func TestWildcard(t *testing.T) {
	v := evalOK(t, "github.event.commits.*.message")
	require.Equal(t, filtered{"first", "second"}, v)

	v = evalOK(t, "github.event.commits[*].id")
	require.Equal(t, filtered{"aaa", "bbb", "ccc"}, v)

	// Object wildcard yields values, in key order.
	v = evalOK(t, "needs.build.outputs.*")
	require.Equal(t, filtered{"v1.2.3"}, v)

	require.Equal(t, true, evalOK(t, "contains(github.event.commits.*.message, 'second')"))
	require.Equal(t, "first,second", evalOK(t, "join(github.event.commits.*.message)"))

	// Indexing a filtered array applies the index to each ELEMENT, so this is
	// not "first": a string has no element 0.
	require.Equal(t, filtered{}, evalOK(t, "github.event.commits.*.message[0]"))
	// A wildcard on a non-collection is an empty array, never null.
	require.Equal(t, filtered{}, evalOK(t, "github.ref_name.*"))
	require.Equal(t, filtered{}, evalOK(t, "github.nope.*"))
}

func TestFunctions(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"contains('Hello world', 'llo')", true},
		{"contains('Hello world', 'LLO')", true},
		{"contains('Hello', 'x')", false},
		{"contains(fromJSON('[1,2,3]'), 2)", true},
		{"contains(fromJSON('[1,2,3]'), 9)", false},
		{"contains(fromJSON('[\"a\"]'), 'A')", true},
		{"startsWith('Hello', 'he')", true},
		{"startswith('Hello', 'lo')", false},
		{"endsWith('Hello', 'LO')", true},
		{"format('{0} and {1}', 'a', 'b')", "a and b"},
		{"format('{{ literal }}')", "{ literal }"},
		{"format('{0}{{{1}}}', 'x', 'y')", "x{y}"},
		{"format('{0}', 1)", "1"},
		{"format('{0}', true)", "true"},
		{"format('{0}', null)", ""},
		{"join(fromJSON('[\"a\",\"b\"]'))", "a,b"},
		{"join(fromJSON('[\"a\",\"b\"]'), '-')", "a-b"},
		{"join('solo', '-')", "solo"},
		{"join(fromJSON('[1,2]'), '')", "12"},
		{"fromJSON('3') == 3", true},
		{"fromJSON('{\"a\":1}').a", 1.0},
		{"toJSON(fromJSON('[1,2]'))", "[\n  1,\n  2\n]"},
		{"toJSON('x')", "\"x\""},
		{"toJSON(null)", "null"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			require.Equal(t, c.want, evalOK(t, c.src))
		})
	}
}

func TestStatusFunctions(t *testing.T) {
	e := New(testCtx())
	// The default is "nothing has failed yet".
	require.Equal(t, true, mustEval(t, e, "success()"))
	require.Equal(t, false, mustEval(t, e, "failure()"))
	require.Equal(t, false, mustEval(t, e, "cancelled()"))
	require.Equal(t, true, mustEval(t, e, "always()"))

	f := e.WithStatus(Status{Failure: true})
	require.Equal(t, false, mustEval(t, f, "success()"))
	require.Equal(t, true, mustEval(t, f, "failure()"))
	require.Equal(t, true, mustEval(t, f, "always()"))

	c := e.WithStatus(Status{Cancelled: true})
	require.Equal(t, true, mustEval(t, c, "cancelled()"))
	// WithStatus must not mutate the receiver.
	require.Equal(t, true, mustEval(t, e, "success()"))
}

func mustEval(t *testing.T, e *Evaluator, src string) any {
	t.Helper()
	v, err := e.Eval(src)
	require.NoError(t, err)
	return v
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"foo", "unrecognized named-value: 'foo'"},
		{"foo.bar", "unrecognized named-value: 'foo'"},
		{"nosuchfn()", "unrecognized function: 'nosuchfn'"},
		{"contains('a')", "contains() expects 2 argument(s), got 1"},
		{"always(1)", "always() expects 0 argument(s), got 1"},
		{"join()", "join() expects 1 to 2 arguments, got 0"},
		{"format('{0}')", "references {0} but only 0 argument(s) were supplied"},
		{"format('{x}', 1)", "is not a number"},
		{"format('{0', 1)", "unclosed '{'"},
		{"format('a}b')", "unescaped '}'"},
		{"fromJSON('not json')", "could not parse"},
		{"fromJSON('')", "empty string"},
		{"'unterminated", "unterminated string"},
		{"1 +", "unexpected character"},
		{"a | b", "did you mean"},
		{"(1", "expected \")\""},
		{"github.", "expected a property name"},
		{"", "empty expression"},
		{"1 2", "unexpected \"2\""},
		{"numbers[0", "expected \"]\""},
		{"0x", "malformed hex number"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := New(testCtx()).Eval(c.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}
}
