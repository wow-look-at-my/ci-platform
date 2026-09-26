package enrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
)

func testKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key, Fingerprint(pub)
}

// The fingerprint is a path segment in the approval API and a string an
// operator compares by eye.
func TestFingerprint_IsStableAndURLSafe(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	fp := Fingerprint(pub)
	assert.Equal(t, fp, Fingerprint(pub), "the same key must always name itself the same way")
	assert.True(t, strings.HasPrefix(fp, "SHA256:"), fp)
	assert.NotContains(t, fp, "/")
	assert.NotContains(t, fp, "+")
	assert.NotContains(t, fp, "=")
	assert.Equal(t, fp, (&url.URL{Path: "/x/" + fp}).EscapedPath()[3:], "needs escaping in a URL")

	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	assert.NotEqual(t, fp, Fingerprint(other))
}

func TestPublicKeyRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	got, err := ParsePublicKey(EncodePublicKey(pub))
	require.NoError(t, err)
	assert.Equal(t, pub, got)

	_, err = ParsePublicKey("not base64!")
	require.ErrorContains(t, err, "not base64")

	_, err = ParsePublicKey("dG9vLXNob3J0")
	require.ErrorContains(t, err, "bytes, want 32")
}

// The key is the machine's identity: generated once, kept, and readable by
// nobody else.
func TestLoadOrCreateKey_CreatesThenLoadsTheSameKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "host.key")

	first, created, err := LoadOrCreateKey(path)
	require.NoError(t, err)
	assert.True(t, created)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a private key other users can read is not private")

	second, created, err := LoadOrCreateKey(path)
	require.NoError(t, err)
	assert.False(t, created, "a restart must not look like a new machine")
	assert.Equal(t, first, second)
	assert.Equal(t, Fingerprint(first.Public().(ed25519.PublicKey)),
		Fingerprint(second.Public().(ed25519.PublicKey)))
}

// A key file that cannot be read has to stop the runner, not quietly become a
// new identity that needs approving again.
func TestLoadOrCreateKey_RefusesAFileItCannotRead(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"garbage.key":   "this is not a PEM file",
		"wrongtype.key": "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
	} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, _, err := LoadOrCreateKey(path)
		require.Error(t, err, name)
	}
}

func TestSignAndVerify(t *testing.T) {
	key, fp := testKey(t)
	pub, _ := key.Public().(ed25519.PublicKey)
	at := time.Now()
	nonce, err := NewNonce()
	require.NoError(t, err)

	sig := Sign(key, PurposeSession, fp, at, nonce)
	require.NoError(t, Verify(pub, PurposeSession, fp, at, nonce, sig))

	// Every field the control plane acts on is inside the signature.
	assert.Error(t, Verify(pub, PurposeEnrol, fp, at, nonce, sig), "purpose is not covered")
	assert.Error(t, Verify(pub, PurposeSession, "SHA256:somebody-else", at, nonce, sig))
	assert.Error(t, Verify(pub, PurposeSession, fp, at.Add(time.Second), nonce, sig))
	assert.Error(t, Verify(pub, PurposeSession, fp, at, "another-nonce", sig))
	assert.ErrorContains(t, Verify(pub, PurposeSession, fp, at, nonce, "not base64!"), "not base64")

	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	assert.Error(t, Verify(otherPub, PurposeSession, fp, at, nonce, sig), "another key's signature verified")
}

func TestNewNonce_IsDifferentEveryTime(t *testing.T) {
	seen := set.New[string]()
	for range 100 {
		n, err := NewNonce()
		require.NoError(t, err)
		require.False(t, seen.Contains(n), "a nonce repeated")
		seen.Add(n)
	}
}

func testSessions(t *testing.T, now *time.Time) *Sessions {
	t.Helper()
	s, err := NewSessions([]byte("a-session-key"), 10*time.Minute, func() time.Time { return *now })
	require.NoError(t, err)
	return s
}

func TestNewSessions_RequiresAKey(t *testing.T) {
	_, err := NewSessions(nil, time.Minute, nil)
	require.ErrorContains(t, err, "session signing key is required")

	s, err := NewSessions([]byte("k"), 0, nil)
	require.NoError(t, err)
	assert.Equal(t, DefaultSessionTTL, s.TTL(), "an unset TTL takes the default rather than never expiring")
}

func TestSessionToken_RoundTripsAndExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := testSessions(t, &now)

	token, minted := s.Mint("SHA256:abcd", "runner-1")
	got, err := s.Parse(token)
	require.NoError(t, err)
	assert.Equal(t, "SHA256:abcd", got.Fingerprint)
	assert.Equal(t, "runner-1", got.RunnerID)
	assert.Equal(t, minted.Expires.Unix(), got.Expires.Unix())

	now = now.Add(11 * time.Minute)
	_, err = s.Parse(token)
	require.ErrorContains(t, err, "expired")
}

// A token is a signed statement. Editing the host it names must not survive.
func TestSessionToken_RejectsAnythingItDidNotSign(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := testSessions(t, &now)
	other, err := NewSessions([]byte("another-instances-key"), time.Minute, func() time.Time { return now })
	require.NoError(t, err)

	forged, _ := other.Mint("SHA256:abcd", "runner-1")
	for _, tc := range []string{"", "garbage", "a.b", forged} {
		_, err := s.Parse(tc)
		assert.Error(t, err, "accepted %q", tc)
	}
}

// A nonce with no expiry needs an unbounded record of every nonce ever seen; a
// timestamp with no nonce lets a captured request be replayed for the whole
// window. Both halves are checked here because both are load-bearing.
func TestCheckFreshness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := testSessions(t, &now)

	require.NoError(t, s.CheckFreshness("SHA256:abcd", now, "nonce-1"))
	require.ErrorContains(t, s.CheckFreshness("SHA256:abcd", now, "nonce-1"), "already been used")

	// The same nonce from a different host is a different request.
	require.NoError(t, s.CheckFreshness("SHA256:efgh", now, "nonce-1"))

	require.ErrorContains(t, s.CheckFreshness("SHA256:abcd", now, ""), "no nonce")
	require.ErrorContains(t, s.CheckFreshness("SHA256:abcd", now.Add(-MaxClockSkew-time.Second), "old"), "clock")
	require.ErrorContains(t, s.CheckFreshness("SHA256:abcd", now.Add(MaxClockSkew+time.Second), "future"), "clock")
}

// Nonces are remembered only as long as one could still be replayed; keeping
// them forever would be a leak with no security value.
func TestCheckFreshness_ForgetsNoncesItCanNoLongerAccept(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := testSessions(t, &now)
	require.NoError(t, s.CheckFreshness("SHA256:abcd", now, "nonce-1"))

	now = now.Add(3 * MaxClockSkew)
	require.NoError(t, s.CheckFreshness("SHA256:abcd", now, "nonce-2"))

	s.mu.Lock()
	remembered := len(s.seen)
	s.mu.Unlock()
	assert.Equal(t, 1, remembered, "a nonce that can no longer pass the window is still being remembered")
}
