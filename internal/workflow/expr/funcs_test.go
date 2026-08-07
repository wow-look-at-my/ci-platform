// Function and coercion behaviour, split from the lexer and parser cases.
package expr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"math"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

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

func TestValidateSyntaxOnly(t *testing.T) {
	// Validate checks syntax without resolving names, so an unknown context is
	// fine here but a malformed body is not.
	require.NoError(t, Validate("plain text"))
	require.NoError(t, Validate("a ${{ nosuchcontext.x }} b"))
	require.NoError(t, Validate("${{ format('{0}', 1) }}${{ 2 }}"))
	require.ErrorContains(t, Validate("${{ 1 == }}"), "unexpected end of expression")
	require.ErrorContains(t, Validate("ok ${{ github. }}"), "expected a property name")
	require.ErrorContains(t, Validate("${{ 1 "), "unterminated expression")
}

func TestValidateCondition(t *testing.T) {
	require.NoError(t, ValidateCondition(""))
	require.NoError(t, ValidateCondition("   "))
	require.NoError(t, ValidateCondition("always()"))
	require.NoError(t, ValidateCondition("${{ always() }}"))
	require.NoError(t, ValidateCondition("nosuchcontext.x == 1"))
	require.ErrorContains(t, ValidateCondition("a &&"), "unexpected end of expression")
	require.ErrorContains(t, ValidateCondition("${{ a && }}"), "unexpected end of expression")
	// A mixed template is validated per ${{ }} body.
	require.NoError(t, ValidateCondition("${{ a }} extra"))
	require.ErrorContains(t, ValidateCondition("${{ a"), "unterminated expression")
}

func TestFilteredArrayIndexing(t *testing.T) {
	ctx := Context{"data": map[string]any{
		"rows": []any{
			map[string]any{"cells": []any{"a1", "a2"}, "id": 1},
			map[string]any{"cells": []any{"b1"}, "id": 2},
			"not a collection",
		},
	}}
	e := New(ctx)
	// A string index reaches into each object element.
	require.Equal(t, filtered{1.0, 2.0}, mustEval(t, e, "data.rows.*.id"))
	// An integer index reaches into each array element.
	require.Equal(t, filtered{"a1", "b1"}, mustEval(t, e, "data.rows.*.cells[0]"))
	require.Equal(t, filtered{"a2"}, mustEval(t, e, "data.rows.*.cells[1]"))
	// A second wildcard flattens the nested arrays.
	require.Equal(t, filtered{"a1", "a2", "b1"}, mustEval(t, e, "data.rows.*.cells.*"))
	// The whole object's values, per element.
	require.Equal(t, filtered{[]any{"a1", "a2"}, 1.0, []any{"b1"}, 2.0}, mustEval(t, e, "data.rows.*.*"))
}

func TestObjectIndexedByNonString(t *testing.T) {
	e := New(Context{"m": map[string]any{"0": "zero", "true": "yes", "": "blank"}})
	require.Equal(t, "zero", mustEval(t, e, "m[0]"))
	require.Equal(t, "yes", mustEval(t, e, "m[true]"))
	require.Equal(t, "blank", mustEval(t, e, "m[null]"))
	require.Nil(t, mustEval(t, e, "m[fromJSON('[1]')]"))
}

func TestNormalizeUnusualTypes(t *testing.T) {
	s := "pointed at"
	e := New(Context{
		"ptr":   &s,
		"nilp":  (*string)(nil),
		"chan":  make(chan int),
		"badky": map[int]string{1: "x"},
		"arr":   [2]int{4, 5},
	})
	require.Equal(t, "pointed at", mustEval(t, e, "ptr"))
	require.Nil(t, mustEval(t, e, "nilp"))
	require.Equal(t, 4.0, mustEval(t, e, "arr[0]"))
	// Anything with no expression-language equivalent becomes its printed form
	// rather than silently vanishing.
	require.IsType(t, "", mustEval(t, e, "chan"))
	require.IsType(t, "", mustEval(t, e, "badky"))
}

func TestJSONNumberContextValue(t *testing.T) {
	e := New(Context{"n": json.Number("42"), "bad": json.Number("nope")})
	require.Equal(t, 42.0, mustEval(t, e, "n"))
	require.Equal(t, "nope", mustEval(t, e, "bad"))
}

func TestToJSONShape(t *testing.T) {
	e := New(Context{"m": map[string]any{"b": 1, "a": []any{true, nil}}})
	got, err := e.Eval("toJSON(m)")
	require.NoError(t, err)
	require.Equal(t, "{\n  \"a\": [\n    true,\n    null\n  ],\n  \"b\": 1\n}", got)

	// NaN and Infinity have no JSON spelling and are written as null.
	got, err = e.Eval("toJSON(NaN)")
	require.NoError(t, err)
	require.Equal(t, "null", got)
}

func TestFromJSONTruncatesLongInputInErrors(t *testing.T) {
	long := strings.Repeat("x", 200)
	_, err := New(testCtx()).Eval("fromJSON('" + long + "')")
	require.ErrorContains(t, err, "...")
	require.Less(t, len(err.Error()), 200)
}

func TestHashFilesUnreadableFile(t *testing.T) {
	e := New(testCtx()).WithFS(brokenFS{}, "")
	_, err := e.Eval("hashFiles('*.txt')")
	require.ErrorContains(t, err, "could not read")
}

// brokenFS lists one file and then refuses to open it, which is what a
// permissions problem looks like from hashFiles' side.
type brokenFS struct{}

func (brokenFS) Open(name string) (fs.File, error) {
	if name == "." {
		return fstest.MapFS{"a.txt": {Data: []byte("x")}}.Open(".")
	}
	return nil, fs.ErrPermission
}
