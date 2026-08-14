package operatorauth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSigner(now func() time.Time) *signer {
	if now == nil {
		now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	}
	return &signer{key: []byte("session-signing-key"), ttl: time.Hour, now: now}
}

func TestSession_RoundTrips(t *testing.T) {
	s := testSigner(nil)
	value, minted := s.mint("PazerOP", MethodGitHub)

	got, err := s.parse(value)
	require.NoError(t, err)
	assert.Equal(t, "PazerOP", got.Login)
	assert.Equal(t, MethodGitHub, got.Method)
	assert.Equal(t, minted.Expires.Unix(), got.Expires.Unix())
}

// A cookie is a statement this server signed. Editing any part of it, including
// the login it names, must not survive.
func TestSession_RejectsTampering(t *testing.T) {
	s := testSigner(nil)
	value, _ := s.mint("PazerOP", MethodGitHub)
	encoded, sig, _ := strings.Cut(value, ".")

	forged, _ := (&signer{key: []byte("attackers-key"), ttl: time.Hour, now: s.now}).mint("PazerOP", MethodGitHub)
	tests := []string{
		"",
		"garbage",
		encoded,                       // no signature
		encoded + ".",                 // empty signature
		encoded + "." + sig + "x",     // altered signature
		"AAAA." + sig,                 // altered payload
		forged,                        // signed with another key
	}
	for _, tc := range tests {
		_, err := s.parse(tc)
		assert.Error(t, err, "accepted %q", tc)
	}
}

func TestSession_Expires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := testSigner(func() time.Time { return now })
	value, _ := s.mint("PazerOP", MethodGitHub)

	_, err := s.parse(value)
	require.NoError(t, err)

	now = now.Add(time.Hour + time.Second)
	_, err = s.parse(value)
	require.ErrorContains(t, err, "expired")
}

func TestIdentity_ActorNamesAPersonOrTheSharedCredential(t *testing.T) {
	assert.Equal(t, "PazerOP", Identity{Login: "PazerOP", Method: MethodGitHub}.Actor())
	assert.Equal(t, "operator", Identity{Method: MethodToken}.Actor())
}
