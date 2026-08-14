package runnerapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// host is a runner host with a real keypair, so the tests exercise the
// signatures rather than a stub that always verifies.
type host struct {
	key         ed25519.PrivateKey
	fingerprint string
}

func newHost(t *testing.T) *host {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &host{key: key, fingerprint: enrol.Fingerprint(pub)}
}

func (h *host) enrolRequest(t *testing.T, at time.Time) protocol.EnrolRequest {
	t.Helper()
	nonce, err := enrol.NewNonce()
	require.NoError(t, err)
	pub, _ := h.key.Public().(ed25519.PublicKey)
	return protocol.EnrolRequest{
		APIVersion: protocol.APIVersion, PublicKey: enrol.EncodePublicKey(pub),
		Name: "basement-box", OS: "linux", Arch: "amd64", Labels: []string{"self-hosted", "linux"},
		Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(h.key, enrol.PurposeEnrol, h.fingerprint, at, nonce),
	}
}

func (h *host) sessionRequest(t *testing.T, at time.Time) protocol.SessionRequest {
	t.Helper()
	nonce, err := enrol.NewNonce()
	require.NoError(t, err)
	return protocol.SessionRequest{
		APIVersion: protocol.APIVersion, Fingerprint: h.fingerprint, RunnerID: "runner-1",
		Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(h.key, enrol.PurposeSession, h.fingerprint, at, nonce),
	}
}

// postAnon is an unauthenticated call, which is what enrolment and session
// exchange are: a host has no token until it has done both.
func (h *harness) postAnon(t *testing.T, path string, body any, out any) int {
	t.Helper()
	saved := h.token
	h.token = ""
	defer func() { h.token = saved }()
	return h.post(t, path, body, out)
}

func TestEnrol_RecordsAPendingHost(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)

	var out protocol.EnrolResponse
	code := h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), &out)
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, box.fingerprint, out.Fingerprint)
	assert.Equal(t, string(model.RunnerHostPending), out.State)
	assert.Contains(t, out.Message, box.fingerprint, "the message is what an operator reads off the runner's logs")

	stored, err := h.st.GetRunnerHost(t.Context(), box.fingerprint)
	require.NoError(t, err)
	assert.Equal(t, "basement-box", stored.Name)
	assert.Equal(t, model.RunnerHostPending, stored.State)
}

// Without proof of possession, anybody could enrol a public key they do not
// hold and leave the operator approving a machine they do not control.
func TestEnrol_RefusesAnUnprovenKey(t *testing.T) {
	h := newHarness(t)
	box, impostor := newHost(t), newHost(t)

	req := box.enrolRequest(t, time.Now())
	// The impostor's signature over the victim's fingerprint.
	req.Signature = enrol.Sign(impostor.key, enrol.PurposeEnrol, box.fingerprint, time.Now(), req.Nonce)

	code := h.postAnon(t, protocol.PathEnrol, req, nil)
	assert.Equal(t, http.StatusUnauthorized, code)

	_, err := h.st.GetRunnerHost(t.Context(), box.fingerprint)
	require.Error(t, err, "a refused enrolment must leave nothing behind")
}

// This is the property the whole scheme rests on: enrolling grants nothing.
func TestSession_RefusesAHostNobodyApproved(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))

	var out protocol.SessionResponse
	code := h.postAnon(t, protocol.PathSession, box.sessionRequest(t, time.Now()), &out)

	assert.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, out.Token)
}

func TestSession_IssuesATokenOnceApproved(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))
	_, err := h.st.SetRunnerHostState(t.Context(), box.fingerprint, model.RunnerHostApproved, "PazerOP", "", time.Now())
	require.NoError(t, err)

	var out protocol.SessionResponse
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathSession, box.sessionRequest(t, time.Now()), &out))
	require.NotEmpty(t, out.Token)
	assert.True(t, out.ExpiresAt.After(time.Now()))

	// The token it just issued opens the rest of the surface.
	saved := h.token
	h.token = out.Token
	defer func() { h.token = saved }()
	code := h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1", Labels: []string{"linux"},
	}, nil)
	assert.Equal(t, http.StatusOK, code)

	seen, err := h.st.GetRunnerHost(t.Context(), box.fingerprint)
	require.NoError(t, err)
	assert.False(t, seen.LastSeenAt.IsZero(), "an approved host that checked in should show as seen")
}

func TestSession_RefusesAWrongSignatureAndAnUnknownFingerprint(t *testing.T) {
	h := newHarness(t)
	box, impostor := newHost(t), newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))
	_, err := h.st.SetRunnerHostState(t.Context(), box.fingerprint, model.RunnerHostApproved, "PazerOP", "", time.Now())
	require.NoError(t, err)

	// Somebody else asking for a token against an approved fingerprint.
	req := impostor.sessionRequest(t, time.Now())
	req.Fingerprint = box.fingerprint
	assert.Equal(t, http.StatusUnauthorized, h.postAnon(t, protocol.PathSession, req, nil))

	assert.Equal(t, http.StatusUnauthorized,
		h.postAnon(t, protocol.PathSession, impostor.sessionRequest(t, time.Now()), nil))
}

// A signature is a bearer object for as long as it verifies, so replaying a
// captured one has to fail.
func TestSession_RefusesAReplayedRequest(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))
	_, err := h.st.SetRunnerHostState(t.Context(), box.fingerprint, model.RunnerHostApproved, "PazerOP", "", time.Now())
	require.NoError(t, err)

	req := box.sessionRequest(t, time.Now())
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathSession, req, nil))

	code := h.postAnon(t, protocol.PathSession, req, nil)
	assert.Equal(t, http.StatusBadRequest, code, "the same signed request was accepted twice")
}

// A captured request is worth nothing once the window closes, which is what
// bounds how long a replay is even worth attempting.
func TestSession_RefusesAStaleRequest(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))

	stale := time.Now().Add(-enrol.MaxClockSkew - time.Minute)
	assert.Equal(t, http.StatusBadRequest, h.postAnon(t, protocol.PathSession, box.sessionRequest(t, stale), nil))

	future := time.Now().Add(enrol.MaxClockSkew + time.Minute)
	assert.Equal(t, http.StatusBadRequest, h.postAnon(t, protocol.PathSession, box.sessionRequest(t, future), nil))
}

// Enrolment answers without a credential, and a signature over a key the sender
// generated proves only that they generated it. Without a cap, anything that
// can reach this route fills the table and the approval page.
func TestEnrol_RefusesANewHostOnceTooManyAreWaiting(t *testing.T) {
	h := newHarness(t)
	h.srv.opts.MaxPendingHosts = 2

	for range 2 {
		require.Equal(t, http.StatusOK,
			h.postAnon(t, protocol.PathEnrol, newHost(t).enrolRequest(t, time.Now()), nil))
	}

	flood := newHost(t)
	code := h.postAnon(t, protocol.PathEnrol, flood.enrolRequest(t, time.Now()), nil)
	assert.Equal(t, http.StatusTooManyRequests, code)
	_, err := h.st.GetRunnerHost(t.Context(), flood.fingerprint)
	require.Error(t, err, "a refused enrolment must leave no row behind")

	// Approving clears the queue, and the next host is let in: the cap counts
	// what is waiting, not what exists.
	hosts, err := h.st.ListRunnerHosts(t.Context())
	require.NoError(t, err)
	for _, existing := range hosts {
		if existing.State != model.RunnerHostPending {
			continue
		}
		_, err := h.st.SetRunnerHostState(t.Context(), existing.Fingerprint,
			model.RunnerHostApproved, "PazerOP", "", time.Now())
		require.NoError(t, err)
	}
	assert.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, flood.enrolRequest(t, time.Now()), nil))
}

// A runner restarting must not be turned away because somebody else flooded the
// queue: it already has a row, so the cap does not apply to it.
func TestEnrol_LetsAKnownHostBackInWhenTheQueueIsFull(t *testing.T) {
	h := newHarness(t)
	h.srv.opts.MaxPendingHosts = 1
	known := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, known.enrolRequest(t, time.Now()), nil))

	assert.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, known.enrolRequest(t, time.Now()), nil))
	assert.Equal(t, http.StatusTooManyRequests,
		h.postAnon(t, protocol.PathEnrol, newHost(t).enrolRequest(t, time.Now()), nil))
}

// A signature made to enrol must not be usable to get a token, or an operator
// approving a host would be handing out access retroactively to whoever
// captured the enrolment.
func TestSession_RefusesAnEnrolSignature(t *testing.T) {
	h := newHarness(t)
	box := newHost(t)
	require.Equal(t, http.StatusOK, h.postAnon(t, protocol.PathEnrol, box.enrolRequest(t, time.Now()), nil))
	_, err := h.st.SetRunnerHostState(t.Context(), box.fingerprint, model.RunnerHostApproved, "PazerOP", "", time.Now())
	require.NoError(t, err)

	at := time.Now()
	nonce, err := enrol.NewNonce()
	require.NoError(t, err)
	req := protocol.SessionRequest{
		Fingerprint: box.fingerprint, RunnerID: "runner-1", Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(box.key, enrol.PurposeEnrol, box.fingerprint, at, nonce),
	}
	assert.Equal(t, http.StatusUnauthorized, h.postAnon(t, protocol.PathSession, req, nil))
}

// Revocation that waits for a token to expire is not revocation, so the state
// is re-read on every authenticated call.
func TestAuthenticated_StopsAHostRevokedMidSession(t *testing.T) {
	h := newHarness(t)
	code := h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1",
	}, nil)
	require.Equal(t, http.StatusOK, code)

	_, err := h.st.SetRunnerHostState(t.Context(), testFingerprint, model.RunnerHostRevoked, "PazerOP", "stolen", time.Now())
	require.NoError(t, err)

	code = h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1",
	}, nil)
	assert.Equal(t, http.StatusForbidden, code, "a revoked host kept working on the token it already held")
}

// A runner's labels decide which jobs it is offered, so a host cannot widen its
// own reach by claiming labels the operator did not allow it.
func TestRegister_DropsLabelsTheHostIsNotApprovedFor(t *testing.T) {
	h := newHarness(t)
	_, err := h.st.SetRunnerHostLabels(t.Context(), testFingerprint, []string{"self-hosted", "linux"})
	require.NoError(t, err)

	code := h.post(t, protocol.PathRegister, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1",
		Labels: []string{"self-hosted", "linux", "production-secrets"},
	}, nil)
	require.Equal(t, http.StatusOK, code)

	rn, err := h.st.GetRunner(t.Context(), "runner-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"self-hosted", "linux"}, rn.Labels)
	assert.NotContains(t, rn.Labels, "production-secrets")
}
