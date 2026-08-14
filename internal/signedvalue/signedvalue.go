// Package signedvalue is the envelope for a value this instance hands out and
// later takes back: a dashboard session cookie, a runner's session token.
//
// It exists so there is one implementation of "verify before you read". Two
// copies of an HMAC envelope drift, and the copy that does not get the fix is
// the one an attacker uses.
package signedvalue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Key signs and opens envelopes. It is the raw HMAC key.
type Key []byte

// ErrBadSignature means the value was not produced by this key. It is
// deliberately the same error for a forged signature and a mangled one: the
// caller has no use for the difference, and neither does whoever sent it.
var ErrBadSignature = errors.New("signedvalue: signature does not verify")

// Sign wraps a payload. The result is URL-safe and cookie-safe.
func (k Key) Sign(payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + k.mac(encoded)
}

// Open verifies and returns the payload.
//
// Nothing inside the payload is parsed here, and callers must not parse it
// before Open returns: a field read out of an unverified envelope is a field
// an attacker chose.
func (k Key) Open(value string) ([]byte, error) {
	encoded, sig, ok := strings.Cut(value, ".")
	if !ok {
		return nil, ErrBadSignature
	}
	if !hmac.Equal([]byte(sig), []byte(k.mac(encoded))) {
		return nil, ErrBadSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("signedvalue: payload is not base64: %w", err)
	}
	return payload, nil
}

func (k Key) mac(encoded string) string {
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
