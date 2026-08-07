package expr

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// The cases here are ported from two independent implementations: nektos/act's
// pkg/exprparser/interpreter_test.go and rhysd/actionlint's expr_parser_test.go
// / expr_lexer_test.go. Where the two disagree with GitHub's own runner, the
// runner wins and the expectation carries a comment naming the file that
// settles it (actions-runner/src/Sdk/DTExpressions2/...).

type corpusCase struct {
	src  string
	want any
}

func runCorpus(t *testing.T, ctx Context, cases []corpusCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			got, err := New(ctx).Eval(c.src)
			require.NoError(t, err, "evaluating %q", c.src)
			if f, ok := c.want.(float64); ok && math.IsNaN(f) {
				g, ok := got.(float64)
				require.True(t, ok, "expected a number, got %T", got)
				require.True(t, math.IsNaN(g), "expected NaN, got %v", g)
				return
			}
			require.Equal(t, c.want, got)
		})
	}
}

func corpusCtx() Context {
	return Context{
		"github": map[string]any{
			"action": "push",
			"event": map[string]any{
				"commits": []any{
					map[string]any{"author": map[string]any{"username": "someone"}},
					map[string]any{"author": map[string]any{"username": "someone-else"}},
				},
			},
		},
		"env":      map[string]any{"TEST": "value"},
		"job":      map[string]any{"status": "success"},
		"runner":   map[string]any{"os": "Linux", "temp": "/tmp"},
		"secrets":  map[string]any{"name": "value"},
		"vars":     map[string]any{"name": "value"},
		"strategy": map[string]any{"fail-fast": true},
		"matrix":   map[string]any{"os": "Linux"},
		"inputs":   map[string]any{"name": "value"},
		"steps": map[string]any{
			"step-id": map[string]any{
				"outputs":    map[string]any{"name": "value"},
				"outcome":    "success",
				"conclusion": "success",
			},
			"step-id2": map[string]any{
				"outputs":    map[string]any{},
				"outcome":    "failure",
				"conclusion": "skipped",
			},
		},
		"needs": map[string]any{
			"job-id": map[string]any{
				"outputs": map[string]any{"output-name": "value"},
				"result":  "success",
			},
			"another-job-id": map[string]any{
				"outputs": map[string]any{"output-name": "value"},
				"result":  "success",
			},
		},
	}
}

func TestCorpusLiterals(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"123", 123.0},
		{"-9.7", -9.7},
		{"0xff", 255.0},
		{"-2.99e-2", -2.99e-2},
		{"'foo'", "foo"},
		{"'it''s foo'", "it's foo"},
		// NaN and Infinity are number literals (LexicalAnalyzer.cs).
		{"NaN", math.NaN()},
		{"Infinity", math.Inf(1)},
	})
}

func TestCorpusOperators(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"(false || (false || true))", true},
		{"github.action", "push"},
		{"github['action']", "push"},
		{"github.action[0]", nil},
		{"github.action['0']", nil},
		{"fromJSON('[0,1]')[1]", 1.0},
		{"fromJSON('[0,1]')[2]", nil},
		{"fromJSON('[0,1]')[34553]", nil},
		{"fromJSON('[0,1]')[-1]", nil},
		{"fromJSON('[0,1]')[-34553]", nil},
		// act expects null; Index.cs floors a fractional index instead.
		{"fromJSON('[0,1]')[1.1]", 1.0},
		// act disabled this case; Index.cs derives the integer index from
		// ConvertToNumber, so a numeric string indexes an array.
		{"fromJSON('[0,1]')['1']", 1.0},
		// act expects "someone"; Index.cs applies an index to each ELEMENT of a
		// filtered array, and a string has no element 0.
		{"(github.event.commits.*.author.username)[0]", filtered{}},
		{"!true", false},
		{"1 < 2", true},
		{"'b' <= 'a'", false},
		{"1 > 2", false},
		{"'b' >= 'a'", true},
		{"'a' == 'a'", true},
		{"'a' != 'a'", false},
		{"true && false", false},
		{"true || false", true},
		{"fromJSON('{}') && true", true},
		{"github.event.commits[0].author.username != github.event.commits[1].author.username", true},
		{"github.event.commits[0].author.username1 != github.event.commits[1].author.username", true},
		{"github.event.commits[0].author.username != github.event.commits[1].author.username1", true},
		// act expects true; both sides are null and AbstractEqual returns true
		// for Null/Null (EvaluationResult.cs), so they are NOT unequal.
		{"github.event.commits[0].author.username1 != github.event.commits[1].author.username2", false},
		// act errors on map-vs-map; EvaluationResult.AbstractEqual compares
		// containers by reference, so two different maps are simply unequal.
		{"secrets != env", true},
		{"secrets == secrets", true},
	})
}

func TestCorpusCoercion(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"!null", true},
		{"!-10", false},
		{"!0", true},
		{"!3.14", false},
		{"!''", true},
		{"!'abc'", false},
		{"!fromJSON('{}')", false},
		{"!fromJSON('[]')", false},
		{"null == 0", true},
		{"true == 1", true},
		{"'' == 0", true},
		{"'3' == 3", true},
		{"0 == null", true},
		{"1 == true", true},
		{"0 == ''", true},
		{"3 == '3'", true},
		{"'TEST' == 'test'", true},
		{"true > false", true},
		{"true >= false", true},
		{"true >= true", true},
		{"true != false", true},
		{"fromJSON('{}') < 2", false},
		{"fromJSON('{}') < fromJSON('[]')", false},
		{"fromJSON('{}') > fromJSON('[]')", false},
		// NaN is never equal to anything, itself included.
		{"NaN == NaN", false},
		{"NaN < 1", false},
		{"'0o17' == 15", true},
		{"'Infinity' == Infinity", true},
	})
}

// TestCorpusBooleanEvaluation is act's operand-returning table. Each row is
// `left OP right` and the expected result is one of the two OPERANDS.
func TestCorpusBooleanEvaluation(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"true && true", true},
		{"true && false", false},
		{"true && null", nil},
		{"true && -10", -10.0},
		{"true && 0", 0.0},
		{"true && 10", 10.0},
		{"true && 3.14", 3.14},
		{"true && 0.0", 0.0},
		{"true && Infinity", math.Inf(1)},
		{"true && NaN", math.NaN()},
		{"true && ''", ""},
		{"true && 'abc'", "abc"},
		{"false && true", false},
		{"false && null", false},
		{"false && -10", false},
		{"false && NaN", false},
		{"false && 'abc'", false},
		{"true || true", true},
		{"true || false", true},
		{"true || null", true},
		{"true || NaN", true},
		{"true || 'abc'", true},
		{"false || true", true},
		{"false || false", false},
		{"false || null", nil},
		{"false || -10", -10.0},
		{"false || 0", 0.0},
		{"false || 3.14", 3.14},
		{"false || Infinity", math.Inf(1)},
		{"false || NaN", math.NaN()},
		{"false || ''", ""},
		{"false || 'abc'", "abc"},
		{"null && true", nil},
		{"null && 'abc'", nil},
		{"null || true", true},
		{"null || false", false},
		{"null || null", nil},
		{"null || -10", -10.0},
		{"null || 0", 0.0},
		{"null || ''", ""},
		{"null || 'abc'", "abc"},
		{"-10 && true", true},
		{"-10 && 0", 0.0},
		{"-10 && ''", ""},
		{"-10 || true", -10.0},
		{"-10 || 'abc'", -10.0},
		{"0 && true", 0.0},
		{"0 && 'abc'", 0.0},
		{"0 || true", true},
		{"0 || null", nil},
		{"0 || 'abc'", "abc"},
		{"10 && true", true},
		{"10 && null", nil},
		{"10 && NaN", math.NaN()},
		{"10 || false", 10.0},
		{"NaN && true", math.NaN()},
		{"NaN && 'abc'", math.NaN()},
		{"NaN || true", true},
		{"NaN || null", nil},
		{"NaN || NaN", math.NaN()},
		{"NaN || 'abc'", "abc"},
		{"'' && true", ""},
		{"'' && 'abc'", ""},
		{"'' || true", true},
		{"'' || null", nil},
		{"'' || 0", 0.0},
		{"'' || 'abc'", "abc"},
		{"'abc' && true", true},
		{"'abc' && null", nil},
		{"'abc' && 0", 0.0},
		{"'abc' && ''", ""},
		{"'abc' || true", "abc"},
		{"'abc' || null", "abc"},
		{"'abc' || NaN", "abc"},
		{"0.0 && true", 0.0},
		{"-1.5 && true", true},
	})
}

func TestCorpusContexts(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"github.action", "push"},
		{"github.event.commits[0].message", nil},
		{"fromjson('{\"commits\":[]}').commits[0].message", nil},
		// act expects null; a wildcard on a non-collection is an empty
		// filtered array (Index.cs EvaluateCore).
		{"github.event.pull_request.labels.*.name", filtered{}},
		{"env.TEST", "value"},
		{"job.status", "success"},
		{"steps.step-id.outputs.name", "value"},
		{"steps.step-id.conclusion", "success"},
		{"steps.step-id.conclusion && true", true},
		{"steps.step-id2.conclusion", "skipped"},
		{"steps.step-id.outcome", "success"},
		{"steps.step-id['outcome']", "success"},
		{"steps.step-id.outcome == 'success'", true},
		{"steps['step-id']['outcome'] && true", true},
		{"steps.step-id2.outcome", "failure"},
		{"runner.os", "Linux"},
		{"secrets.name", "value"},
		{"vars.name", "value"},
		{"strategy.fail-fast", true},
		{"matrix.os", "Linux"},
		{"needs.job-id.outputs.output-name", "value"},
		{"needs.job-id.result", "success"},
		{"contains(needs.*.result, 'success')", true},
		{"contains(needs.*.result, 'failure')", false},
		{"inputs.name", "value"},
		// act disabled these as "still too broken"; they work here.
		{"contains(steps.*.outcome, 'success')", true},
		{"contains(steps.*.outcome, 'failure')", true},
		{"contains(steps.*.outputs.name, 'value')", true},
	})
}

// TestCorpusFunctionKinds pins the rule that the reference functions require
// PRIMITIVE operands rather than stringifying a container to satisfy them.
func TestCorpusFunctionKinds(t *testing.T) {
	runCorpus(t, corpusCtx(), []corpusCase{
		{"contains(fromJSON('{\"a\":1}'), 'a')", false},
		{"startsWith(fromJSON('[1]'), '[')", false},
		{"endsWith('abc', fromJSON('[1]'))", false},
		{"join(fromJSON('[]'))", ""},
		{"join(fromJSON('{}'))", ""},
		{"join(3)", "3"},
		{"join(fromJSON('[1,2]'), fromJSON('[3]'))", "1,2"},
		// Null is primitive and converts to "", which every string contains.
		{"contains('abc', null)", true},
		{"contains('a,b', ',')", true},
	})
}

// TestCorpusParseErrors is ported from actionlint's lexer and parser tests.
func TestCorpusParseErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"1 == ", "unexpected end of expression"},
		{"(1", `expected ")"`},
		{"[1]", "unexpected"},
		{"'foo", "unterminated string"},
		{"a b", "unexpected"},
		{"a &", "did you mean"},
		{"a |", "did you mean"},
		{"a =", "did you mean"},
		{"github..sha", "expected a property name"},
		{"github.", "expected a property name"},
		{"env['a'", `expected "]"`},
		{"func(", "unexpected end of expression"},
		{"func(,)", "unexpected"},
		{"1 ~ 2", "unexpected character"},
		{"$foo", "unexpected character"},
		{"@", "unexpected character"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := New(corpusCtx()).Eval(c.src)
			require.Error(t, err, "expected %q to fail", c.src)
			require.Contains(t, err.Error(), c.want)
		})
	}
}
