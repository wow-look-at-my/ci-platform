package plan

import (
	"strings"
	"testing"

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
		{int(3), "3"},
		{int32(4), "4"},
		{int64(5), "5"},
		{uint64(6), "6"},
		{float32(1.5), "1.5"},
		{float64(2.25), "2.25"},
		{float64(3), "3"},
		{[]any{1, "a"}, `[1,"a"]`},
		{map[string]any{"k": 1}, `{"k":1}`},
	} {
		if got := RenderValue(tc.in); got != tc.want {
			t.Fatalf("RenderValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEvalBoolRejectsAMisspelledLiteral(t *testing.T) {
	if _, err := EvalBool(nil, model.NewExpr("ture"), false); err == nil {
		t.Fatal("a misspelled boolean must not silently read as false")
	}
	got, err := EvalBool(nil, model.Expr{}, true)
	if err != nil || !got {
		t.Fatalf("an absent expression takes the default: %v %v", got, err)
	}
	if _, err := EvalBool(nil, model.NewExpr("${{ x }}"), false); err == nil {
		t.Fatal("an expression with no evaluator must be an error, not false")
	}
	for raw, want := range map[string]bool{" TRUE ": true, "False": false} {
		got, err := EvalBool(nil, model.NewExpr(raw), false)
		if err != nil || got != want {
			t.Fatalf("EvalBool(%q) = %v, %v", raw, got, err)
		}
	}
}

func TestEvalIntAndStringMap(t *testing.T) {
	ev := newFakeFactory(map[string]any{"vars.n": "12", "vars.bad": []any{1}})(map[string]any{}, Status{})
	n, err := EvalInt(ev, model.NewExpr("${{ vars.n }}"), 0)
	if err != nil || n != 12 {
		t.Fatalf("EvalInt = %d, %v", n, err)
	}
	if _, err := EvalInt(ev, model.NewExpr("${{ vars.bad }}"), 0); err == nil {
		t.Fatal("a non-number expression must be an error")
	}
	if _, err := EvalInt(nil, model.NewExpr("nine"), 0); err == nil {
		t.Fatal("a non-numeric literal must be an error")
	}
	if _, err := EvalInt(nil, model.NewExpr("${{ x }}"), 0); err == nil {
		t.Fatal("an expression with no evaluator must be an error")
	}

	out, err := EvalStringMap(ev, map[string]model.Expr{"A": model.NewExpr("1"), "B": model.NewExpr("${{ vars.n }}")})
	if err != nil {
		t.Fatal(err)
	}
	if out["A"] != "1" || out["B"] != "12" {
		t.Fatalf("EvalStringMap = %v", out)
	}
	if out, err := EvalStringMap(ev, nil); out != nil || err != nil {
		t.Fatalf("an empty map stays nil: %v %v", out, err)
	}
	if _, err := EvalStringMap(ev, map[string]model.Expr{"A": model.NewExpr("${{ nope }}")}); err == nil {
		t.Fatal("an unresolvable value must surface")
	}
}

func TestExpandMatrixNeedsAnEvaluatorForExpressions(t *testing.T) {
	_, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ fromJSON(x) }}")}, nil)
	if err == nil || !strings.Contains(err.Error(), "evaluator") {
		t.Fatalf("want an evaluator error, got %v", err)
	}
	_, err = ExpandMatrix(&model.Matrix{
		Dimensions: map[string][]any{"os": {"${{ vars.x }}"}},
		Order:      []string{"os"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "evaluator") {
		t.Fatalf("want an evaluator error, got %v", err)
	}
}

func TestFromJSONMatrixRejectsMalformedShapes(t *testing.T) {
	ev := newFakeFactory(map[string]any{
		"bad-dim":     map[string]any{"os": "ubuntu"},
		"bad-include": map[string]any{"include": "nope"},
		"bad-entry":   map[string]any{"include": []any{"nope"}},
	})(map[string]any{}, Status{})
	for _, raw := range []string{"bad-dim", "bad-include", "bad-entry"} {
		if _, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ " + raw + " }}")}, ev); err == nil {
			t.Fatalf("%s: want an error", raw)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu, 1.24)"})
}

func TestEmptyIncludeEntryIsAnError(t *testing.T) {
	_, err := ExpandMatrix(&model.Matrix{Include: []map[string]any{{}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("want an empty-include error, got %v", err)
	}
}

func TestStringDimensionsAreAccepted(t *testing.T) {
	ev := newFakeFactory(map[string]any{"list": []string{"a", "b"}})(map[string]any{}, Status{})
	legs, err := ExpandMatrix(&model.Matrix{
		Dimensions: map[string][]any{"x": {"${{ list }}"}},
		Order:      []string{"x"},
	}, ev)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(a)", "(b)"})
}
