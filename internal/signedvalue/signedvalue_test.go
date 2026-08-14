package signedvalue

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var key = Key("a-signing-key")

func TestRoundTrip(t *testing.T) {
	got, err := key.Open(key.Sign([]byte("hello")))
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got)
}

func TestSign_ProducesACookieSafeValue(t *testing.T) {
	value := key.Sign([]byte("a payload with spaces, commas; and =signs="))
	assert.NotContains(t, value, " ")
	assert.NotContains(t, value, ";")
	assert.NotContains(t, value, "=")
	assert.Equal(t, 1, strings.Count(value, "."))
}

func TestOpen_RejectsAnythingThisKeyDidNotSign(t *testing.T) {
	value := key.Sign([]byte("hello"))
	encoded, sig, _ := strings.Cut(value, ".")

	tests := map[string]string{
		"empty":               "",
		"no separator":        encoded,
		"empty signature":     encoded + ".",
		"altered signature":   encoded + "." + sig + "x",
		"altered payload":     base64.RawURLEncoding.EncodeToString([]byte("goodbye")) + "." + sig,
		"another key's value": Key("another-key").Sign([]byte("hello")),
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := key.Open(tc)
			require.ErrorIs(t, err, ErrBadSignature)
		})
	}
}

// A payload that verifies but is not base64 cannot happen from this Sign, so
// the error says what it is rather than pretending the signature was wrong.
func TestOpen_ReportsAMalformedPayloadSeparately(t *testing.T) {
	bad := "!!!not-base64!!!"
	_, err := key.Open(bad + "." + key.mac(bad))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBadSignature)
	assert.Contains(t, err.Error(), "not base64")
}
