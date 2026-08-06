package plan

import (
	"strings"
	"testing"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func legSuffixes(legs []Leg) []string {
	out := make([]string, 0, len(legs))
	for _, l := range legs {
		out = append(out, l.Suffix())
	}
	return out
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %q, want %q\nfull: %v", i, got[i], want[i], got)
		}
	}
}

func TestCartesianProductFollowsDeclarationOrder(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{
			"os": {"ubuntu", "windows"},
			"go": {"1.22", "1.23"},
		},
		Order: []string{"os", "go"},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{
		"(ubuntu, 1.22)", "(ubuntu, 1.23)", "(windows, 1.22)", "(windows, 1.23)",
	})
	if legs[0].Key() != "os=ubuntu,go=1.22" {
		t.Fatalf("matrix key: %q", legs[0].Key())
	}
}

func TestExcludeIsAPartialMatch(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{
			"os": {"ubuntu", "windows", "macos"},
			"go": {"1.22", "1.23"},
		},
		Order:   []string{"os", "go"},
		Exclude: []map[string]any{{"os": "windows"}, {"os": "macos", "go": "1.22"}},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu, 1.22)", "(ubuntu, 1.23)", "(macos, 1.23)"})
}

func TestExcludeUnknownKeyMatchesNothing(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}},
		Order:      []string{"os"},
		Exclude:    []map[string]any{{"arch": "arm64"}},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)"})
}

// The include semantics GitHub documents, using GitHub's own example, which is
// the single most commonly-hit corner of matrix expansion.
func TestIncludeMergesOrAppendsLikeGitHub(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{
			"fruit":  {"apple", "pear"},
			"animal": {"cat", "dog"},
		},
		Order: []string{"fruit", "animal"},
		Include: []map[string]any{
			{"color": "green"},
			{"color": "pink", "animal": "cat"},
			{"fruit": "apple", "shape": "circle"},
			{"fruit": "banana"},
			{"fruit": "banana", "animal": "cat"},
		},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{
		"(apple, cat, pink, circle)",
		"(apple, dog, green, circle)",
		"(pear, cat, pink)",
		"(pear, dog, green)",
		"(banana)",
		"(banana, cat)",
	})
}

func TestIncludeNeverOverwritesAnOriginalDimension(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
		Order:      []string{"os"},
		// os=macos matches no combination, so it becomes its own leg rather
		// than rewriting ubuntu or windows.
		Include: []map[string]any{{"os": "macos", "extra": "yes"}},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)", "(windows)", "(macos, yes)"})
}

func TestIncludeOnlyMatrix(t *testing.T) {
	m := &model.Matrix{Include: []map[string]any{
		{"image": "claude-host/agent-host", "dockerfile": "Dockerfile"},
		{"image": "claude-host/base", "dockerfile": "Dockerfile.base"},
	}}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{
		"(Dockerfile, claude-host/agent-host)",
		"(Dockerfile.base, claude-host/base)",
	})
}

func TestExcludeThenIncludeOrdering(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu", "windows"}},
		Order:      []string{"os"},
		Exclude:    []map[string]any{{"os": "windows"}},
		// windows was excluded, so this include has nothing to merge into and
		// is appended as its own leg.
		Include: []map[string]any{{"os": "windows", "note": "readded"}},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)", "(windows, readded)"})
}

func TestAppendedLegIsNotACandidateForLaterIncludes(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"fruit": {"apple"}},
		Order:      []string{"fruit"},
		Include: []map[string]any{
			{"fruit": "banana"},
			{"fruit": "banana", "animal": "cat"},
		},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(apple)", "(banana)", "(banana, cat)"})
}

func TestMatrixValueRendering(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{
			"n":   {1, 2.5},
			"b":   {true},
			"obj": {map[string]any{"a": 1}},
			"arr": {[]any{"x", "y"}},
		},
		Order: []string{"n", "b", "obj", "arr"},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{
		`(1, true, {"a":1}, ["x","y"])`,
		`(2.5, true, {"a":1}, ["x","y"])`,
	})
}

func TestNumericValuesCompareAcrossTypes(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"n": {1, 2}},
		Order:      []string{"n"},
		Exclude:    []map[string]any{{"n": float64(2)}},
	}
	legs, err := ExpandMatrix(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(1)"})
}

func TestMatrixFromJSONExpression(t *testing.T) {
	ev := newFakeFactory(map[string]any{
		"fromJSON(needs.setup.outputs.matrix)": map[string]any{
			"os":      []any{"ubuntu", "windows"},
			"exclude": []any{map[string]any{"os": "windows"}},
		},
	})(map[string]any{}, Status{Success: true})
	m := &model.Matrix{FromExpr: model.NewExpr("${{ fromJSON(needs.setup.outputs.matrix) }}")}
	legs, err := ExpandMatrix(m, ev)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)"})
}

func TestMatrixFromJSONMustBeAnObject(t *testing.T) {
	ev := newFakeFactory(map[string]any{"fromJSON(x)": []any{1, 2}})(map[string]any{}, Status{})
	_, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ fromJSON(x) }}")}, ev)
	if err == nil || !strings.Contains(err.Error(), "not an object") {
		t.Fatalf("want an object-shape error, got %v", err)
	}
}

func TestDimensionValueExpressionSplicesAList(t *testing.T) {
	ev := newFakeFactory(map[string]any{"fromJSON(vars.oses)": []any{"a", "b"}})(map[string]any{}, Status{})
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"${{ fromJSON(vars.oses) }}", "c"}},
		Order:      []string{"os"},
	}
	legs, err := ExpandMatrix(m, ev)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, legSuffixes(legs), []string{"(a)", "(b)", "(c)"})
}

func TestMatrixOrderMustCoverEveryDimension(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}, "go": {"1.22"}},
		Order:      []string{"os"},
	}
	if _, err := ExpandMatrix(m, nil); err == nil {
		t.Fatal("want an error when the declared order omits a dimension")
	}
}

func TestEmptyDimensionIsAnError(t *testing.T) {
	m := &model.Matrix{Dimensions: map[string][]any{"os": {}}, Order: []string{"os"}}
	if _, err := ExpandMatrix(m, nil); err == nil {
		t.Fatal("want an error for a dimension with no values")
	}
}

func TestEmptyExcludeEntryIsAnError(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}},
		Order:      []string{"os"},
		Exclude:    []map[string]any{{}},
	}
	if _, err := ExpandMatrix(m, nil); err == nil {
		t.Fatal("want an error for an empty exclude entry")
	}
}

func TestNilMatrixMeansOneLeg(t *testing.T) {
	legs, err := ExpandMatrix(nil, nil)
	if err != nil || legs != nil {
		t.Fatalf("got %v, %v", legs, err)
	}
}
