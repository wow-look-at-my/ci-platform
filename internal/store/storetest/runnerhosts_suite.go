// Runner host enrolment and approval: the rules that decide whether a machine
// may take jobs, so both stores have to agree on them exactly.
package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func (f *fixture) enrol(fingerprint, name string, labels []string) *model.RunnerHost {
	f.t.Helper()
	h, err := f.s.EnrolRunnerHost(f.ctx, &model.RunnerHost{
		Fingerprint: fingerprint, PublicKey: "cHVibGljLWtleQ==", Name: name,
		OS: "linux", Arch: "amd64", Version: "v1", Labels: labels,
		EnrolledFrom: "192.168.1.84", EnrolledAt: nowUTC(),
	})
	require.NoError(f.t, err)
	return h
}

func testRunnerHostsEnrolAndApprove(t *testing.T, f *fixture) {
	h := f.enrol("SHA256:aaaa", "basement-box", []string{"self-hosted", "linux"})

	// A newly enrolled host is inert until somebody says otherwise.
	assert.Equal(t, model.RunnerHostPending, h.State)
	assert.False(t, h.Approved())
	assert.Empty(t, h.ApprovedBy)

	got, err := f.s.GetRunnerHost(f.ctx, "SHA256:aaaa")
	require.NoError(t, err)
	assert.Equal(t, "basement-box", got.Name)
	assert.Equal(t, []string{"self-hosted", "linux"}, got.Labels)
	assert.Equal(t, "192.168.1.84", got.EnrolledFrom)

	at := nowUTC()
	approved, err := f.s.SetRunnerHostState(f.ctx, "SHA256:aaaa", model.RunnerHostApproved, "PazerOP", "the basement box", at)
	require.NoError(t, err)
	assert.True(t, approved.Approved())
	assert.Equal(t, "PazerOP", approved.ApprovedBy)
	assert.WithinDuration(t, at, approved.ApprovedAt, time.Second)
	assert.Equal(t, "the basement box", approved.Note)
}

// Approval is the action that hands out access, so it cannot be anonymous.
func testRunnerHostsApprovalNamesSomebody(t *testing.T, f *fixture) {
	f.enrol("SHA256:bbbb", "box", nil)

	_, err := f.s.SetRunnerHostState(f.ctx, "SHA256:bbbb", model.RunnerHostApproved, "", "", nowUTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must name who approved it")

	_, err = f.s.SetRunnerHostState(f.ctx, "SHA256:bbbb", "sort-of-approved", "PazerOP", "", nowUTC())
	require.Error(t, err, "an unknown state must not be storable")
}

// Re-enrolling is what a restarting host does on every boot. If it could write,
// restarting would clear a revocation and swap the key behind an approved
// fingerprint -- both silent escalations.
func testRunnerHostsReEnrolCannotResetApproval(t *testing.T, f *fixture) {
	f.enrol("SHA256:cccc", "box", []string{"linux"})
	_, err := f.s.SetRunnerHostState(f.ctx, "SHA256:cccc", model.RunnerHostRevoked, "PazerOP", "stolen laptop", nowUTC())
	require.NoError(t, err)

	again, err := f.s.EnrolRunnerHost(f.ctx, &model.RunnerHost{
		Fingerprint: "SHA256:cccc", PublicKey: "YW4tYXR0YWNrZXJz", Name: "definitely-fine",
		Labels: []string{"linux", "gpu"}, EnrolledAt: nowUTC(),
	})
	require.NoError(t, err)

	assert.Equal(t, model.RunnerHostRevoked, again.State, "re-enrolling cleared a revocation")
	assert.Equal(t, "cHVibGljLWtleQ==", again.PublicKey, "re-enrolling replaced the approved key")
	assert.Equal(t, "box", again.Name)
	assert.Equal(t, []string{"linux"}, again.Labels)
}

// Revoking clears the approval fields: a row that still reads "approved by X"
// about a revoked host misleads whoever reads the list next.
func testRunnerHostsRevokeClearsApproval(t *testing.T, f *fixture) {
	f.enrol("SHA256:dddd", "box", nil)
	_, err := f.s.SetRunnerHostState(f.ctx, "SHA256:dddd", model.RunnerHostApproved, "PazerOP", "", nowUTC())
	require.NoError(t, err)

	revoked, err := f.s.SetRunnerHostState(f.ctx, "SHA256:dddd", model.RunnerHostRevoked, "PazerOP", "decommissioned", nowUTC())
	require.NoError(t, err)
	assert.Equal(t, model.RunnerHostRevoked, revoked.State)
	assert.Empty(t, revoked.ApprovedBy)
	assert.True(t, revoked.ApprovedAt.IsZero())
	assert.Equal(t, "decommissioned", revoked.Note)
	assert.False(t, revoked.Approved())
}

func testRunnerHostsLabelsAndTouch(t *testing.T, f *fixture) {
	f.enrol("SHA256:eeee", "box", []string{"linux", "gpu"})

	updated, err := f.s.SetRunnerHostLabels(f.ctx, "SHA256:eeee", []string{"linux"})
	require.NoError(t, err)
	assert.Equal(t, []string{"linux"}, updated.Labels)
	assert.True(t, updated.AllowsLabel("LINUX"), "GitHub labels are matched case-insensitively")
	assert.False(t, updated.AllowsLabel("gpu"), "a label the operator removed is not claimable")

	seen := nowUTC()
	require.NoError(t, f.s.TouchRunnerHost(f.ctx, "SHA256:eeee", seen))
	got, err := f.s.GetRunnerHost(f.ctx, "SHA256:eeee")
	require.NoError(t, err)
	assert.WithinDuration(t, seen, got.LastSeenAt, time.Second)
}

// Pending hosts sort first because they are the ones an operator has to act on.
func testRunnerHostsListOrdersPendingFirst(t *testing.T, f *fixture) {
	f.enrol("SHA256:1111", "first", nil)
	f.enrol("SHA256:2222", "second", nil)
	f.enrol("SHA256:3333", "third", nil)
	_, err := f.s.SetRunnerHostState(f.ctx, "SHA256:1111", model.RunnerHostApproved, "PazerOP", "", nowUTC())
	require.NoError(t, err)
	_, err = f.s.SetRunnerHostState(f.ctx, "SHA256:2222", model.RunnerHostRevoked, "PazerOP", "", nowUTC())
	require.NoError(t, err)

	hosts, err := f.s.ListRunnerHosts(f.ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 3)
	assert.Equal(t, "SHA256:3333", hosts[0].Fingerprint, "pending first")
	assert.Equal(t, "SHA256:1111", hosts[1].Fingerprint, "then approved")
	assert.Equal(t, "SHA256:2222", hosts[2].Fingerprint, "then revoked")
}

func testRunnerHostsNotFound(t *testing.T, f *fixture) {
	_, err := f.s.GetRunnerHost(f.ctx, "SHA256:nobody")
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.s.SetRunnerHostState(f.ctx, "SHA256:nobody", model.RunnerHostApproved, "PazerOP", "", nowUTC())
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.s.SetRunnerHostLabels(f.ctx, "SHA256:nobody", []string{"linux"})
	require.ErrorIs(t, err, store.ErrNotFound)

	require.ErrorIs(t, f.s.TouchRunnerHost(f.ctx, "SHA256:nobody", nowUTC()), store.ErrNotFound)
}
