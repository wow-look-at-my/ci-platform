package operatorauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/signedvalue"
)

// Method is how somebody proved they are an operator.
type Method string

const (
	// MethodGitHub is a session from "Sign in with GitHub". It carries the
	// account that signed in, so a cancel can name a person.
	MethodGitHub Method = "github"
	// MethodToken is a session from the shared operator credential. It carries
	// no account, because the credential names nobody.
	MethodToken Method = "token"
)

// Identity is who a request is acting as.
type Identity struct {
	// Login is the GitHub account, empty for a token session.
	Login   string
	Method  Method
	Expires time.Time
}

// Actor is the name to record for an action taken by this identity.
func (i Identity) Actor() string {
	if i.Login != "" {
		return i.Login
	}
	return "operator"
}

// signer mints and verifies session cookies.
//
// The cookie is a signed statement, not a lookup key: with one instance and one
// SQLite file, a session table would add a write to every request and a
// migration to every schema change, and buy nothing that a short TTL and a
// rotated signing key do not already give.
//
// The consequence, stated because it is a real one: a GitHub session cannot be
// revoked before it expires except by rotating the signing key, which ends
// every session at once. Removing somebody from the admin list stops them
// signing in again, and the middleware re-checks that list on every request, so
// that particular case is covered.
type signer struct {
	key signedvalue.Key
	// tokenKey signs sessions minted from the shared operator credential, and
	// is derived from that credential. Rotating the credential changes this key,
	// so the sessions it handed out stop verifying -- which is what somebody
	// rotating a leaked credential is expecting to happen.
	tokenKey signedvalue.Key
	ttl      time.Duration
	now      func() time.Time
}

// tokenSigningKey derives the key that signs token sessions from the operator
// credential, so a rotated credential cannot be traded for a session that
// outlives it.
func tokenSigningKey(sessionKey []byte, token string) signedvalue.Key {
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte("ci-platform/operator-token-session/v1"))
	mac.Write([]byte(token))
	return mac.Sum(nil)
}

// keyFor picks the signing key for a method.
func (s *signer) keyFor(method Method) signedvalue.Key {
	if method == MethodToken {
		return s.tokenKey
	}
	return s.key
}

func (s *signer) mint(login string, method Method) (string, Identity) {
	id := Identity{Login: login, Method: method, Expires: s.now().Add(s.ttl)}
	payload := fmt.Sprintf("v1|%s|%s|%d", method, login, id.Expires.Unix())
	return s.keyFor(method).Sign([]byte(payload)), id
}

// parse verifies the signature before it reads anything out of the payload, so
// no field of a forged cookie is ever acted on.
//
// Which key signed it is not known until the payload is read, and the payload
// must not be read until a signature verifies, so both are tried. Neither
// answers differently on a wrong guess, so nothing is learned from the order.
func (s *signer) parse(raw string) (Identity, error) {
	payload, err := s.key.Open(raw)
	if err != nil {
		payload, err = s.tokenKey.Open(raw)
	}
	if err != nil {
		return Identity{}, err
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 4 || parts[0] != "v1" {
		return Identity{}, errors.New("operatorauth: session payload is not v1")
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return Identity{}, fmt.Errorf("operatorauth: session expiry is not a timestamp: %w", err)
	}
	id := Identity{Login: parts[2], Method: Method(parts[1]), Expires: time.Unix(exp, 0)}
	if !s.now().Before(id.Expires) {
		return Identity{}, errors.New("operatorauth: session has expired")
	}
	return id, nil
}
