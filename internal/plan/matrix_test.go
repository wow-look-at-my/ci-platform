package plan

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.Equal(t, len(want), len(got))

	for i := range got {
		require.Equal(t, want[i], got[i])

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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{
		"(ubuntu, 1.22)", "(ubuntu, 1.23)", "(windows, 1.22)", "(windows, 1.23)",
	})
	require.Equal(t, "os=ubuntu,go=1.22", legs[0].Key())

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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(ubuntu, 1.22)", "(ubuntu, 1.23)", "(macos, 1.23)"})
}

// GitHub errors on an exclude key that names no dimension rather than
// excluding nothing (MatrixExclude in the runner source).
func TestExcludeUnknownKeyIsAnError(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}},
		Order:      []string{"os"},
		Exclude:    []map[string]any{{"arch": "arm64"}},
	}
	_, err := ExpandMatrix(m, nil)
	require.False(t, err == nil || !strings.Contains(err.Error(), "does not match any key"))

}

// The include semantics GitHub documents, using GitHub's own example, which is
// the single most commonly-hit corner of matrix expansion.
//
// The values an include merges into a cross product combination land in the
// matrix context but NOT in the display name: the runner builds the name from
// the cross product vector before merging the extra values in.
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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{
		"(apple, cat)",
		"(apple, dog)",
		"(pear, cat)",
		"(pear, dog)",
		"(banana)",
		"(banana, cat)",
	})
	want := []map[string]any{
		{"fruit": "apple", "animal": "cat", "color": "pink", "shape": "circle"},
		{"fruit": "apple", "animal": "dog", "color": "green", "shape": "circle"},
		{"fruit": "pear", "animal": "cat", "color": "pink"},
		{"fruit": "pear", "animal": "dog", "color": "green"},
		{"fruit": "banana"},
		{"fruit": "banana", "animal": "cat"},
	}
	for i, w := range want {
		require.Equal(t, legs[i].Values, w)

	}
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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)", "(windows)", "(macos, yes)"})
}

// An include-only matrix, which is where the operator's example comes from.
// Matrix.Order carries the source key order for include-only keys; without it
// the suffix would be alphabetical and the check run name would not match.
func TestIncludeOnlyMatrix(t *testing.T) {
	m := &model.Matrix{
		Order: []string{"image", "dockerfile"},
		Include: []map[string]any{
			{"image": "claude-host/agent-host", "dockerfile": "Dockerfile"},
			{"image": "claude-host/base", "dockerfile": "Dockerfile.base"},
		},
	}
	legs, err := ExpandMatrix(m, nil)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{
		"(claude-host/agent-host, Dockerfile)",
		"(claude-host/base, Dockerfile.base)",
	})
}

func TestIncludeOnlyMatrixWithoutDeclaredOrderSortsKeys(t *testing.T) {
	m := &model.Matrix{Include: []map[string]any{
		{"image": "claude-host/agent-host", "dockerfile": "Dockerfile"},
	}}
	legs, err := ExpandMatrix(m, nil)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(Dockerfile, claude-host/agent-host)"})
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
	require.Nil(t, err)

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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(apple)", "(banana)", "(banana, cat)"})
}

// GitHub traverses a matrix value and emits every scalar leaf as its own name
// segment, so an object or list value does not render as JSON in the name.
func TestMatrixValueRendering(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{
			"n":   {1, 2.5},
			"b":   {true},
			"obj": {map[string]any{"a": 1, "b": ""}},
			"arr": {[]any{"x", "y"}},
		},
		Order: []string{"n", "b", "obj", "arr"},
	}
	legs, err := ExpandMatrix(m, nil)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{
		"(1, true, 1, x, y)",
		"(2.5, true, 1, x, y)",
	})
	// The leg identity keeps the whole value, so two legs differing only
	// inside an object are still distinguishable.
	require.Equal(t, `n=1,b=true,obj={"a":1,"b":""},arr=["x","y"]`, legs[0].Key())

}

func TestNameIsTruncatedLikeGitHub(t *testing.T) {
	long := strings.Repeat("x", 200)
	name, err := DisplayName("job", model.Expr{}, &Leg{
		Values: map[string]any{"v": long}, Order: []string{"v"}, NameKeys: []string{"v"},
	}, nil)
	require.Nil(t, err)

	require.False(t, len(name) != MaxJobNameLength || !strings.HasSuffix(name, "..."))

}

func TestLegWithNoScalarSegmentsGetsNoSuffix(t *testing.T) {
	name, err := DisplayName("job", model.Expr{}, &Leg{
		Values: map[string]any{"v": ""}, Order: []string{"v"}, NameKeys: []string{"v"},
	}, nil)
	require.Nil(t, err)

	require.Equal(t, "job", name)

}

func TestNumericValuesCompareAcrossTypes(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"n": {1, 2}},
		Order:      []string{"n"},
		Exclude:    []map[string]any{{"n": float64(2)}},
	}
	legs, err := ExpandMatrix(m, nil)
	require.Nil(t, err)

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
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(ubuntu)"})
}

func TestMatrixFromJSONMustBeAnObject(t *testing.T) {
	ev := newFakeFactory(map[string]any{"fromJSON(x)": []any{1, 2}})(map[string]any{}, Status{})
	_, err := ExpandMatrix(&model.Matrix{FromExpr: model.NewExpr("${{ fromJSON(x) }}")}, ev)
	require.False(t, err == nil || !strings.Contains(err.Error(), "not an object"))

}

func TestDimensionValueExpressionSplicesAList(t *testing.T) {
	ev := newFakeFactory(map[string]any{"fromJSON(vars.oses)": []any{"a", "b"}})(map[string]any{}, Status{})
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"${{ fromJSON(vars.oses) }}", "c"}},
		Order:      []string{"os"},
	}
	legs, err := ExpandMatrix(m, ev)
	require.Nil(t, err)

	assertStrings(t, legSuffixes(legs), []string{"(a)", "(b)", "(c)"})
}

func TestMatrixOrderMustCoverEveryDimension(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}, "go": {"1.22"}},
		Order:      []string{"os"},
	}
	_, err := ExpandMatrix(m, nil)
	require.NotNil(t, err)

}

func TestEmptyDimensionIsAnError(t *testing.T) {
	m := &model.Matrix{Dimensions: map[string][]any{"os": {}}, Order: []string{"os"}}
	_, err := ExpandMatrix(m, nil)
	require.NotNil(t, err)

}

func TestEmptyExcludeEntryIsAnError(t *testing.T) {
	m := &model.Matrix{
		Dimensions: map[string][]any{"os": {"ubuntu"}},
		Order:      []string{"os"},
		Exclude:    []map[string]any{{}},
	}
	_, err := ExpandMatrix(m, nil)
	require.NotNil(t, err)

}

func TestNilMatrixMeansOneLeg(t *testing.T) {
	legs, err := ExpandMatrix(nil, nil)
	require.False(t, err != nil || legs != nil)

}
