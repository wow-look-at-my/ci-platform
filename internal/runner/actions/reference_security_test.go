package actions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A traversal in any element walks out of the action cache: Owner and Repo
// become directory names, and Path is joined onto the extracted directory
// before that directory is copied into the job's sandbox.
func TestParseReferenceRefusesPathTraversal(t *testing.T) {
	bad := []string{
		"some/action/../../../../etc@v1",
		"some/action/..@v1",
		"../evil/action@v1",
		"some/../action@v1",
		"some/action/./x@v1",
	}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseReference(raw)
			require.Error(t, err, "a traversal must be refused")
			assert.Contains(t, err.Error(), "path traversal")
		})
	}
}

func TestParseReferenceStillAcceptsOrdinaryRefs(t *testing.T) {
	for _, raw := range []string{
		"actions/checkout@v4",
		"actions/aws/ec2@main",
		"owner/repo/deep/path@0123456789abcdef0123456789abcdef01234567",
	} {
		_, err := ParseReference(raw)
		require.NoError(t, err, raw)
	}
}
