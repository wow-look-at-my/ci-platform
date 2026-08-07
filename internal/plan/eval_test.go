package plan

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func TestRenderValueCoversTheTypesYAMLAndJSONProduce(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "true"},
		{3, "3"},
		{int32(4), "4"},
		{int64(5), "5"},
		{uint64(6), "6"},
		{float32(1.5), "1.5"},
		{2.25, "2.25"},
		{float64(3), "3"},
		{[]any{1, "a"}, `[1,"a"]`},
		{map[string]any{"k": 1}, `{"k":1}`},
	} {
		got := RenderValue(tc.in)
		require.Equal(t, tc.want, got)

	}
}

func TestEvalBoolRejectsAMisspelledLiteral(t *testing.T) {
	_, err := EvalBool(nil, model.NewExpr("ture"), false)
	require.NotNil(t, err)

	got, err := EvalBool(nil, model.Expr{}, true)
	require.False(t, err != nil || !got)

	_, err = EvalBool(nil, model.NewExpr("${{ x }}"), false)
	require.NotNil(t, err)

	for raw, want := range map[string]bool{" TRUE ": true, "False": false} {
		got, err := EvalBool(nil, model.NewExpr(raw), false)
		require.False(t, err != nil || got != want)

	}
}

func TestEvalIntAndStringMap(t *testing.T) {
	ev := newFakeFactory(map[string]any{"vars.n": "12", "vars.bad": []any{1}})(map[string]any{}, Status{})
	n, err := EvalInt(ev, model.NewExpr("${{ vars.n }}"), 0)
	require.False(t, err != nil || n != 12)

	_, err = EvalInt(ev, model.NewExpr("${{ vars.bad }}"), 0)
	require.NotNil(t, err)

	_, err = EvalInt(nil, model.NewExpr("nine"), 0)
	require.NotNil(t, err)

	_, err = EvalInt(nil, model.NewExpr("${{ x }}"), 0)
	require.NotNil(t, err)

	out, err := EvalStringMap(ev, map[string]model.Expr{"A": model.NewExpr("1"), "B": model.NewExpr("${{ vars.n }}")})
	require.Nil(t, err)

	require.False(t, out["A"] != "1" || out["B"] != "12")

	out, err = EvalStringMap(ev, nil)
	require.False(t, out != nil || err != nil)

	_, err = EvalStringMap(ev, map[string]model.Expr{"A": model.NewExpr("${{ nope }}")})
	require.NotNil(t, err)

}

func TestExpandMatrixNeedsAnEvaluatorForExpressions(t *testing.T) {
	_, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ fromJSON(x) }}")}, nil)
	require.False(t, err == nil || !strings.Contains(err.Error(), "evaluator"))

	_, err = ExpandMatrix(&model.Matrix{
		Dimensions: map[string][]any{"os": {"${{ vars.x }}"}},
		Order:      []string{"os"},
	}, nil)
	require.False(t, err == nil || !strings.Contains(err.Error(), "evaluator"))

}

func TestFromJSONMatrixRejectsMalformedShapes(t *testing.T) {
	ev := newFakeFactory(map[string]any{
		"bad-dim":     map[string]any{"os": "ubuntu"},
		"bad-include": map[string]any{"include": "nope"},
		"bad-entry":   map[string]any{"include": []any{"nope"}},
	})(map[string]any{}, Status{})
	for _, raw := range []string{"bad-dim", "bad-include", "bad-entry"} {
		_, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ " + raw + " }}")}, ev)
		require.NotNil(t, err)

	}
}

func TestFromJSONMatrixHonoursDeclaredOrder(t *testing.T) {
	ev := newFakeFactory(map[string]any{
		"m": map[string]any{"os": []any{"ubuntu"}, "go": []any{"1.24"}},
	})(map[string]any{}, Status{})
	legs, err := ExpandMatrix(&model.Matrix{
		FromExpr: model.NewExpr("${{ m }}"),
		Order:    []string{"os", "go"},
	}, ev)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(ubuntu, 1.24)"})
}

func TestEmptyIncludeEntryIsAnError(t *testing.T) {
	_, err := ExpandMatrix(&model.Matrix{Include: []map[string]any{{}}}, nil)
	require.False(t, err == nil || !strings.Contains(err.Error(), "include"))

}

func TestStringDimensionsAreAccepted(t *testing.T) {
	ev := newFakeFactory(map[string]any{"list": []string{"a", "b"}})(map[string]any{}, Status{})
	legs, err := ExpandMatrix(&model.Matrix{
		Dimensions: map[string][]any{"x": {"${{ list }}"}},
		Order:      []string{"x"},
	}, ev)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(a)", "(b)"})
}
