package expr

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
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
		{"TRUE", true},
		{"false", false},
		{"null", nil},
		{"NULL", nil},
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
	require.Equal(t, "first", evalOK(t, "github.event.commits.*.message[0]"))
	require.Nil(t, evalOK(t, "github.ref_name.*"))
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

func TestEvalString(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"plain text", "plain text"},
		{"${{ github.ref_name }}", "main"},
		{"build-${{ github.ref_name }}-${{ github.run_number }}", "build-main-42"},
		{"${{ null }}", ""},
		{"${{ true }}", "true"},
		{"${{ false }}", "false"},
		{"${{ 3 }}", "3"},
		{"${{ 3.0 }}", "3"},
		{"${{ 3.5 }}", "3.5"},
		{"${{ 1e3 }}", "1000"},
		{"${{ 'a' }}${{ 'b' }}", "ab"},
		{"${{ fromJSON('[1,2]') }}", "[1,2]"},
		{"${{ fromJSON('{\"a\":1}') }}", `{"a":1}`},
		{"${{ github.event.commits.*.message }}", `["first","second"]`},
		{"${{ format('}}') }}", "}"},
		{"${{ 'a}}b' }}", "a}}b"},
		{"a ${{ 'b' }} c", "a b c"},
		{"${{env.FOO}}", "bar"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, err := New(testCtx()).EvalString(c.raw)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

func TestEvalStringErrors(t *testing.T) {
	_, err := New(testCtx()).EvalString("${{ github.ref_name }")
	require.ErrorContains(t, err, "unterminated expression")

	_, err = New(testCtx()).EvalString("${{ nope.x }}")
	require.ErrorContains(t, err, "unrecognized named-value: 'nope'")
}

func TestEvalBool(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"false", false},
		{"success()", true},
		{"always()", true},
		{"${{ always() }}", true},
		{"github.ref_name == 'main'", true},
		{"${{ github.ref_name == 'nope' }}", false},
		{"github.event_name == 'push' && !cancelled()", true},
		{"env.EMPTY", false},
		{"env.FOO", true},
		{"inputs.debug", true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, err := New(testCtx()).EvalBool(c.raw)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}

	_, err := New(testCtx()).EvalBool("nope.x")
	require.ErrorContains(t, err, "unrecognized named-value")
}

func TestEvalExpr(t *testing.T) {
	got, err := New(testCtx()).EvalExpr(model.NewExpr("tag-${{ needs.build.outputs.tag }}"))
	require.NoError(t, err)
	require.Equal(t, "tag-v1.2.3", got)
}

func TestNormalizeCallerTypes(t *testing.T) {
	c := Context{
		"a": map[string]string{"k": "v"},
		"b": []string{"x", "y"},
		"c": int64(7),
		"d": float32(1.5),
		"e": map[string]any{"n": uint8(3)},
	}
	e := New(c)
	require.Equal(t, "v", mustEval(t, e, "a.k"))
	require.Equal(t, "y", mustEval(t, e, "b[1]"))
	require.Equal(t, 7.0, mustEval(t, e, "c"))
	require.Equal(t, true, mustEval(t, e, "d == 1.5"))
	require.Equal(t, 3.0, mustEval(t, e, "e.n"))
}

func TestHashFilesNeedsFilesystem(t *testing.T) {
	_, err := New(testCtx()).Eval("hashFiles('**/go.sum')")
	require.ErrorContains(t, err, "needs a workspace filesystem")
}

func TestHashFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"ws/go.sum":            {Data: []byte("alpha")},
		"ws/sub/go.sum":        {Data: []byte("beta")},
		"ws/sub/deep/go.sum":   {Data: []byte("gamma")},
		"ws/other.txt":         {Data: []byte("ignored")},
		"ws/vendor/go.sum":     {Data: []byte("vendored")},
		"outside/go.sum":       {Data: []byte("outside the workspace")},
		"ws/nested/pkg/x.json": {Data: []byte("{}")},
	}
	e := New(testCtx()).WithFS(fsys, "ws")

	got, err := e.Eval("hashFiles('**/go.sum')")
	require.NoError(t, err)
	want := expectHash(t, fsys, "ws/go.sum", "ws/sub/deep/go.sum", "ws/sub/go.sum", "ws/vendor/go.sum")
	require.Equal(t, want, got)

	// Exclusion.
	got, err = e.Eval("hashFiles('**/go.sum', '!vendor/**')")
	require.NoError(t, err)
	require.Equal(t, expectHash(t, fsys, "ws/go.sum", "ws/sub/deep/go.sum", "ws/sub/go.sum"), got)

	// A non-recursive pattern matches only the top level.
	got, err = e.Eval("hashFiles('go.sum')")
	require.NoError(t, err)
	require.Equal(t, expectHash(t, fsys, "ws/go.sum"), got)

	// Several patterns union.
	got, err = e.Eval("hashFiles('go.sum', 'sub/go.sum')")
	require.NoError(t, err)
	require.Equal(t, expectHash(t, fsys, "ws/go.sum", "ws/sub/go.sum"), got)

	// No match is an empty string, as in GHA.
	got, err = e.Eval("hashFiles('**/nothing.lock')")
	require.NoError(t, err)
	require.Equal(t, "", got)

	// Root defaulting to the whole filesystem.
	all := New(testCtx()).WithFS(fsys, "")
	got, err = all.Eval("hashFiles('**/go.sum')")
	require.NoError(t, err)
	require.Equal(t, expectHash(t, fsys,
		"outside/go.sum", "ws/go.sum", "ws/sub/deep/go.sum", "ws/sub/go.sum", "ws/vendor/go.sum"), got)
}

func TestHashFilesErrors(t *testing.T) {
	e := New(testCtx()).WithFS(fstest.MapFS{"a.txt": {Data: []byte("x")}}, "")
	_, err := e.Eval("hashFiles('')")
	require.ErrorContains(t, err, "empty pattern")
	_, err = e.Eval("hashFiles('!x')")
	require.ErrorContains(t, err, "only exclusion patterns")
	_, err = New(testCtx()).WithFS(fstest.MapFS{}, "missing").Eval("hashFiles('*')")
	require.ErrorContains(t, err, "could not walk")
}

// expectHash recomputes the documented digest independently of the
// implementation: sha256 over the concatenated per-file sha256 digests, in
// sorted path order.
func expectHash(t *testing.T, fsys fstest.MapFS, paths ...string) string {
	t.Helper()
	overall := sha256.New()
	for _, p := range paths {
		f, ok := fsys[p]
		require.True(t, ok, "test fixture missing %s", p)
		d := sha256.Sum256(f.Data)
		overall.Write(d[:])
	}
	return hex.EncodeToString(overall.Sum(nil))
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/main.go", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/d/c", true},
		{"a/**/c", "a/b/d", false},
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"**", "any/depth/file", true},
	}
	for _, c := range cases {
		t.Run(c.pat+"~"+c.name, func(t *testing.T) {
			require.Equal(t, c.want, globMatch(c.pat, c.name))
		})
	}
}

func TestStringifyNumbers(t *testing.T) {
	require.Equal(t, "3", formatNumber(3))
	require.Equal(t, "-3", formatNumber(-3))
	require.Equal(t, "0.5", formatNumber(0.5))
	require.Equal(t, "NaN", formatNumber(nan()))
	require.Equal(t, "Infinity", formatNumber(inf(1)))
	require.Equal(t, "-Infinity", formatNumber(inf(-1)))
}

func nan() float64      { return math.NaN() }
func inf(s int) float64 { return math.Inf(s) }
