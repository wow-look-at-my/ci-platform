package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An include-only key contributes a name segment, and its position comes from
// the YAML key order. Alphabetical fallback would rename every leg and stop
// matching the branch protection rule keyed on the name.
func TestMatrixOrderCarriesIncludeOnlyKeysInYAMLOrder(t *testing.T) {
	src := "on: push\njobs:\n  publish:\n    runs-on: x\n    strategy:\n      matrix:\n" +
		"        image: [claude-host/agent-host]\n" +
		"        include:\n" +
		"          - image: claude-host/agent-host\n            dockerfile: Dockerfile\n" +
		"            zzz-late: v\n" +
		"    steps:\n      - run: y\n"
	w, err := Parse("ci.yml", []byte(src))
	require.NoError(t, err)

	m := w.Jobs["publish"].Strategy.Matrix
	require.NotNil(t, m)
	assert.Equal(t, []string{"image", "dockerfile", "zzz-late"}, m.Order,
		"dimensions first, then include-only keys in first-appearance order")
}
