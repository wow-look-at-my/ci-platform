package artifacts_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
)

// Every artifact call checks the scope it needs. A scope the minter withholds
// but no handler checks is a permission that exists only in the documentation:
// a fork PR's token is issued without cache write for a reason, and the same
// has to hold on the artifact side.
func TestEveryCallChecksItsScope(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	body := map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name": "scoped", "size": "1",
	}

	tests := []struct {
		method string
		needs  jobtoken.Scope
	}{
		{"CreateArtifact", jobtoken.ScopeArtifactsWrite},
		{"FinalizeArtifact", jobtoken.ScopeArtifactsWrite},
		{"DeleteArtifact", jobtoken.ScopeArtifactsWrite},
		{"ListArtifacts", jobtoken.ScopeArtifactsRead},
		{"GetSignedArtifactURL", jobtoken.ScopeArtifactsRead},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			// A token carrying everything except the one scope this call needs.
			var scopes []jobtoken.Scope
			for _, s := range jobtoken.DefaultScopes {
				if s != tc.needs {
					scopes = append(scopes, s)
				}
			}
			token, err := h.signer.MintJob(jobtoken.Job{
				RunID: runID, JobID: jobID, Attempt: attempt,
				RepoID: 9, Repo: "wow-look-at-my/ci-platform",
				Scopes: scopes, ExpiresAt: time.Now().Add(time.Hour),
			})
			require.NoError(t, err)

			resp := h.twirpAs(t, token, tc.method, body)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			assert.Contains(t, decode[map[string]string](t, resp)["msg"], string(tc.needs))
		})
	}
}
