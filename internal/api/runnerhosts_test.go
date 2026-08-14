package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func hostFixture(state model.RunnerHostState) *model.RunnerHost {
	return &model.RunnerHost{
		Fingerprint: "SHA256:abcd", PublicKey: "cHVibGljLWtleQ==", State: state,
		Name: "basement-box", OS: "linux", Arch: "amd64", Labels: []string{"self-hosted", "linux"},
		EnrolledFrom: "192.168.1.84", EnrolledAt: testNow,
	}
}

// asOperator sends the request the way a signed-in browser would, so the action
// is attributed rather than anonymous.
func (h *harness) asOperator(t *testing.T, method, target, body, actor string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("X-CI-Actor", actor)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

func TestListRunnerHosts_CountsWhatIsWaiting(t *testing.T) {
	h := newHarness(t)
	h.st.hosts = []*model.RunnerHost{hostFixture(model.RunnerHostPending), {
		Fingerprint: "SHA256:efgh", State: model.RunnerHostApproved, Name: "attic-box",
	}}

	w := h.do(t, "GET", "/api/v1/runner-hosts", "")
	require.Equal(t, http.StatusOK, w.Code)

	out := decode[RunnerHostListDTO](t, w)
	assert.Equal(t, 2, out.TotalCount)
	assert.Equal(t, 1, out.PendingCount, "a host nobody approved is a runner that will never take a job")
	assert.Equal(t, "192.168.1.84", out.Hosts[0].EnrolledFrom)
	assert.Equal(t, []string{"self-hosted", "linux"}, out.Hosts[0].Labels)
}

// Approving a host is the one action that hands out execution on the fleet, so
// it records who did it rather than an anonymous state change.
func TestApproveRunnerHost_RecordsTheOperator(t *testing.T) {
	h := newHarness(t)
	h.st.hosts = []*model.RunnerHost{hostFixture(model.RunnerHostPending)}

	w := h.asOperator(t, "POST", "/api/v1/runner-hosts/SHA256:abcd/approve",
		`{"note":"the basement box"}`, "PazerOP")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	out := decode[RunnerHostDTO](t, w)
	assert.Equal(t, string(model.RunnerHostApproved), out.State)
	assert.Equal(t, "PazerOP", out.ApprovedBy)
	assert.Equal(t, "the basement box", out.Note)

	require.NotEmpty(t, h.st.events)
	last := h.st.events[len(h.st.events)-1]
	assert.Equal(t, "runner_host.approved", last.Kind)
	assert.Contains(t, last.Message, "PazerOP")
	assert.Equal(t, "SHA256:abcd", last.Detail["fingerprint"])
}

func TestRevokeRunnerHost(t *testing.T) {
	h := newHarness(t)
	h.st.hosts = []*model.RunnerHost{hostFixture(model.RunnerHostApproved)}

	w := h.asOperator(t, "POST", "/api/v1/runner-hosts/SHA256:abcd/revoke",
		`{"note":"decommissioned"}`, "PazerOP")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	out := decode[RunnerHostDTO](t, w)
	assert.Equal(t, string(model.RunnerHostRevoked), out.State)
	assert.Empty(t, out.ApprovedBy, "a revoked host must not still read as approved by somebody")
	assert.Equal(t, "runner_host.revoked", h.st.events[len(h.st.events)-1].Kind)
}

// An operator can narrow what a host serves at the moment they approve it.
func TestApproveRunnerHost_CanSetLabels(t *testing.T) {
	h := newHarness(t)
	h.st.hosts = []*model.RunnerHost{hostFixture(model.RunnerHostPending)}

	w := h.asOperator(t, "POST", "/api/v1/runner-hosts/SHA256:abcd/approve",
		`{"note":"","labels":["self-hosted"]}`, "PazerOP")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	out := decode[RunnerHostDTO](t, w)
	assert.Equal(t, []string{"self-hosted"}, out.Labels)
}

func TestRunnerHostActions_ReportAnUnknownFingerprint(t *testing.T) {
	h := newHarness(t)

	w := h.asOperator(t, "POST", "/api/v1/runner-hosts/SHA256:nobody/approve", "", "PazerOP")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "SHA256:nobody")
}

// The DTO carries a zero time as an absent field rather than as year one, which
// a UI would otherwise render as "55 years ago".
func TestRunnerHostDTO_OmitsTimesThatHaveNotHappened(t *testing.T) {
	h := runnerHostDTO(&model.RunnerHost{
		Fingerprint: "SHA256:abcd", State: model.RunnerHostPending, EnrolledAt: time.Now(),
	})
	assert.True(t, h.ApprovedAt.IsZero())
	assert.True(t, h.LastSeenAt.IsZero())
	assert.NotNil(t, h.Labels, "an empty label list is [] rather than null")
}
