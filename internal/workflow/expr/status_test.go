package expr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReferencesStatusFunction(t *testing.T) {
	tests := []struct {
		src  string
		want bool
	}{
		{"success()", true},
		{"always()", true},
		{"failure()", true},
		{"cancelled()", true},
		{"ALWAYS()", true},
		{"${{ always() }}", true},
		{"!cancelled() && github.ref == 'x'", true},
		{"github.event_name == 'push' || failure()", true},
		{"toJSON(success())", true},
		{"github.ref == 'refs/heads/main'", false},
		{"${{ github.ref == 'refs/heads/main' }}", false},
		{"", false},
		// A status-function name inside a string literal is not a call, and is
		// exactly what a raw-text scan would get wrong.
		{"contains(github.event.head_commit.message, 'success()')", false},
		{"'always()' == github.ref", false},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			got, err := ReferencesStatusFunction(tc.src)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := ReferencesStatusFunction("${{ unterminated")
	assert.Error(t, err)
	_, err = ReferencesStatusFunction("!!!")
	assert.Error(t, err)
}
