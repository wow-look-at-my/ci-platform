package storetest_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
	"github.com/wow-look-at-my/ci-platform/internal/store/sqlite"
)

// The two stores must agree on the key ResolveSecrets reads, or a secret stored
// against one and resolved by the other silently disappears.
func TestScopeKeyAgreesAcrossStores(t *testing.T) {
	cases := []struct {
		scope, owner, repo, env string
		want                    string
		wantErr                 string
	}{
		{scope: "org", owner: "acme", want: "acme"},
		{scope: "repo", owner: "acme", repo: "widget", want: "acme/widget"},
		{scope: "environment", owner: "acme", repo: "widget", env: "prod", want: "acme/widget/prod"},
		{scope: "org", wantErr: "needs an owner"},
		{scope: "repo", owner: "acme", wantErr: "needs an owner and a repo"},
		{scope: "environment", owner: "acme", repo: "widget", wantErr: "needs an owner, a repo and an environment"},
		{scope: "galaxy", owner: "acme", wantErr: "unknown scope"},
	}
	for _, tc := range cases {
		t.Run(tc.scope+"/"+tc.want+tc.wantErr, func(t *testing.T) {
			gotSQL, errSQL := sqlite.ScopeKey(tc.scope, tc.owner, tc.repo, tc.env)
			gotMem, errMem := mem.ScopeKey(tc.scope, tc.owner, tc.repo, tc.env)
			require.Equal(t, gotSQL, gotMem)
			if tc.wantErr != "" {
				require.ErrorContains(t, errSQL, tc.wantErr)
				require.ErrorContains(t, errMem, tc.wantErr)
				return
			}
			require.NoError(t, errSQL)
			require.NoError(t, errMem)
			require.Equal(t, tc.want, gotSQL)
		})
	}
}
