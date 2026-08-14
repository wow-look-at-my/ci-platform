package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

func testCredentials(t *testing.T, baseURL string) *Credentials {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	c, err := NewCredentials(CredentialsConfig{BaseURL: baseURL, Key: key, RunnerID: "runner-1"})
	require.NoError(t, err)
	return c
}

func TestNewCredentials_RequiresAURLAndAKey(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = NewCredentials(CredentialsConfig{Key: key})
	require.ErrorContains(t, err, "control plane URL is required")

	_, err = NewCredentials(CredentialsConfig{BaseURL: "http://x"})
	require.ErrorContains(t, err, "host key is required")
}

// The token is cached until it is close to expiring: re-signing on every call
// would put a round trip in front of every log flush.
func TestToken_IsCachedAndRenewedBeforeItExpires(t *testing.T) {
	var exchanges int
	expiry := time.Now().Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, protocol.PathSession, r.URL.Path)
		exchanges++
		_ = json.NewEncoder(w).Encode(protocol.SessionResponse{
			Token: "session-token", ExpiresAt: expiry, Labels: []string{"linux"},
		})
	}))
	defer srv.Close()

	c := testCredentials(t, srv.URL)
	for range 3 {
		token, err := c.Token(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "session-token", token)
	}
	assert.Equal(t, 1, exchanges, "a cached token was re-signed for nothing")
	assert.Equal(t, []string{"linux"}, c.Labels())

	// A control plane that discarded its signing key answers 401; dropping the
	// cached token is what makes the next call recover.
	c.Invalidate()
	_, err := c.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, exchanges)
}

// A token that expires mid-job would fail the log flush or the completion
// report, which is the one moment a runner cannot afford to lose access.
func TestToken_RenewsWhenTheCachedOneIsNearlyDone(t *testing.T) {
	var exchanges int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges++
		_ = json.NewEncoder(w).Encode(protocol.SessionResponse{
			Token: "session-token", ExpiresAt: time.Now().Add(30 * time.Second),
		})
	}))
	defer srv.Close()

	c := testCredentials(t, srv.URL)
	_, err := c.Token(context.Background())
	require.NoError(t, err)
	_, err = c.Token(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, exchanges, "a token inside the renewal margin was reused")
}

// A host waiting to be approved is the normal state of a machine somebody just
// set up, so it is a distinct error the caller waits out rather than a fault.
func TestToken_ReportsNotApprovedDistinctly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"this host is pending; an operator has to approve SHA256:abcd"}`))
	}))
	defer srv.Close()

	_, err := testCredentials(t, srv.URL).Token(context.Background())
	require.ErrorIs(t, err, ErrNotApproved)
	assert.Contains(t, err.Error(), "SHA256:abcd", "the message names the fingerprint to approve")
}

func TestEnrol_SendsASignatureOverItsOwnFingerprint(t *testing.T) {
	var got protocol.EnrolRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(protocol.EnrolResponse{Fingerprint: "SHA256:x", State: "pending"})
	}))
	defer srv.Close()

	c := testCredentials(t, srv.URL)
	resp, err := c.Enrol(context.Background(), "basement-box", "linux", "amd64", "v1", []string{"linux"})
	require.NoError(t, err)
	assert.Equal(t, "pending", resp.State)

	pub, err := enrol.ParsePublicKey(got.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, c.Fingerprint(), enrol.Fingerprint(pub))
	require.NoError(t, enrol.Verify(pub, enrol.PurposeEnrol, c.Fingerprint(),
		time.Unix(got.Timestamp, 0), got.Nonce, got.Signature))
	assert.NotEmpty(t, got.Nonce)
}

// An assignment carries the job's secrets, so a runner talking in the clear
// over a real network has to say so rather than leave it to be noticed.
func TestPlaintextWarning(t *testing.T) {
	assert.NotEmpty(t, PlaintextWarning("http://192.168.1.84:8080"))
	assert.NotEmpty(t, PlaintextWarning("http://ci.example.com"))

	assert.Empty(t, PlaintextWarning("https://ci.pazer.build"))
	assert.Empty(t, PlaintextWarning("http://localhost:8080"), "there is no network to sniff")
	assert.Empty(t, PlaintextWarning("http://127.0.0.1:8080"))
	assert.Empty(t, PlaintextWarning("http://ci.localhost:8080"))
	assert.Empty(t, PlaintextWarning("://nonsense"))
}
