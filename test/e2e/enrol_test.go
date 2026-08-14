package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// post sends an unauthenticated request, which is what a runner host has until
// it has been approved.
func (c *controlPlane) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(c.URL+path, "application/json", bytes.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (c *controlPlane) postAs(t *testing.T, path, token string, body any) int {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, c.URL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// operatorPost is an approve or revoke, the way the signed-in dashboard sends it.
func (c *controlPlane) operatorPost(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, c.URL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	req.Header.Set("X-CI-Actor", "PazerOP")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// The runner story end to end, against the shipped binary: a machine generates
// a key, enrols, is refused, is approved by an operator, and only then can take
// work. Every step here is one an operator actually performs.
func TestRunnerHostIsInertUntilAnOperatorApprovesIt(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fingerprint := enrol.Fingerprint(pub)

	signed := func(purpose string) (int64, string, string) {
		at := time.Now()
		nonce, err := enrol.NewNonce()
		require.NoError(t, err)
		return at.Unix(), nonce, enrol.Sign(key, purpose, fingerprint, at, nonce)
	}

	// 1. Enrol. Nothing secret crosses the wire: only the public half and a
	//    signature proving this machine holds the other one.
	ts, nonce, sig := signed(enrol.PurposeEnrol)
	code, body := cp.post(t, protocol.PathEnrol, protocol.EnrolRequest{
		APIVersion: protocol.APIVersion, PublicKey: enrol.EncodePublicKey(pub),
		Name: "basement-box", OS: "linux", Arch: "amd64", Labels: []string{"self-hosted", "linux"},
		Timestamp: ts, Nonce: nonce, Signature: sig,
	})
	require.Equal(t, http.StatusOK, code, string(body))

	var enrolled protocol.EnrolResponse
	require.NoError(t, json.Unmarshal(body, &enrolled))
	assert.Equal(t, fingerprint, enrolled.Fingerprint)
	assert.Equal(t, "pending", enrolled.State)
	assert.Contains(t, enrolled.Message, fingerprint)

	// 2. Enrolling granted nothing.
	ts, nonce, sig = signed(enrol.PurposeSession)
	code, body = cp.post(t, protocol.PathSession, protocol.SessionRequest{
		APIVersion: protocol.APIVersion, Fingerprint: fingerprint, RunnerID: "runner-1",
		Timestamp: ts, Nonce: nonce, Signature: sig,
	})
	require.Equal(t, http.StatusForbidden, code, "an unapproved host got a session")
	assert.Contains(t, string(body), fingerprint, "the refusal names what to approve")

	// 3. The operator sees it waiting.
	resp := cp.get(t, "/api/v1/runner-hosts")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		PendingCount int `json:"pending_count"`
		Hosts        []struct {
			Fingerprint string `json:"fingerprint"`
			State       string `json:"state"`
			Name        string `json:"name"`
		} `json:"hosts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	assert.Equal(t, 1, list.PendingCount)
	require.Len(t, list.Hosts, 1)
	assert.Equal(t, fingerprint, list.Hosts[0].Fingerprint)
	assert.Equal(t, "basement-box", list.Hosts[0].Name)

	// 4. The operator approves the fingerprint.
	code, body = cp.operatorPost(t, "/api/v1/runner-hosts/"+fingerprint+"/approve",
		map[string]any{"note": "the basement box"})
	require.Equal(t, http.StatusOK, code, string(body))
	assert.Contains(t, string(body), `"approved_by":"PazerOP"`, "approval records who granted it")

	// 5. Now the same signature exchange works, and the token it returns opens
	//    the runner surface.
	ts, nonce, sig = signed(enrol.PurposeSession)
	code, body = cp.post(t, protocol.PathSession, protocol.SessionRequest{
		APIVersion: protocol.APIVersion, Fingerprint: fingerprint, RunnerID: "runner-1",
		Timestamp: ts, Nonce: nonce, Signature: sig,
	})
	require.Equal(t, http.StatusOK, code, string(body))
	var session protocol.SessionResponse
	require.NoError(t, json.Unmarshal(body, &session))
	require.NotEmpty(t, session.Token)

	code = cp.postAs(t, protocol.PathRegister, session.Token, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1",
		Labels: []string{"self-hosted", "linux"}, Capacity: 1,
	})
	assert.Equal(t, http.StatusOK, code, "an approved host could not register")

	// 6. Revoking takes effect on the token already issued, rather than waiting
	//    for it to expire.
	code, body = cp.operatorPost(t, "/api/v1/runner-hosts/"+fingerprint+"/revoke",
		map[string]any{"note": "stolen laptop"})
	require.Equal(t, http.StatusOK, code, string(body))

	code = cp.postAs(t, protocol.PathRegister, session.Token, protocol.RegisterRequest{
		APIVersion: protocol.APIVersion, RunnerID: "runner-1",
	})
	assert.Equal(t, http.StatusForbidden, code, "a revoked host kept the token it already held")
}

// A stranger who reaches the control plane can enrol a key -- that route cannot
// require a credential -- and it has to be worth nothing to them.
func TestEnrolFromAnybodyGrantsNothing(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fingerprint := enrol.Fingerprint(pub)
	at := time.Now()
	nonce, err := enrol.NewNonce()
	require.NoError(t, err)

	code, _ := cp.post(t, protocol.PathEnrol, protocol.EnrolRequest{
		APIVersion: protocol.APIVersion, PublicKey: enrol.EncodePublicKey(pub),
		Name: "a-total-stranger", Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(key, enrol.PurposeEnrol, fingerprint, at, nonce),
	})
	require.Equal(t, http.StatusOK, code)

	// No token, and no way to reach the runner surface without one.
	assert.Equal(t, http.StatusUnauthorized,
		cp.postAs(t, protocol.PathRegister, "not-a-token", protocol.RegisterRequest{
			APIVersion: protocol.APIVersion, RunnerID: "runner-1",
		}))

	// Replaying the enrolment cannot flip the state either.
	code, body := cp.post(t, protocol.PathEnrol, protocol.EnrolRequest{
		APIVersion: protocol.APIVersion, PublicKey: enrol.EncodePublicKey(pub),
		Name: "definitely-approved", Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(key, enrol.PurposeEnrol, fingerprint, at, nonce),
	})
	assert.Equal(t, http.StatusBadRequest, code, "a replayed signed request was accepted")
	assert.Contains(t, string(body), "already been used")
}
