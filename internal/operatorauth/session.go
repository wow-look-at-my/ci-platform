package operatorauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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
// The consequence, stated because it is a real one: a session cannot be revoked
// before it expires except by rotating the signing key, which ends every
// session at once. Removing somebody from the admin list stops them signing in
// again but does not cut a session already open.
type signer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// errNoSession distinguishes "no cookie" from "a cookie that does not verify".
var errNoSession = errors.New("operatorauth: no session")

func (s *signer) mint(login string, method Method) (string, Identity) {
	id := Identity{Login: login, Method: method, Expires: s.now().Add(s.ttl)}
	payload := fmt.Sprintf("v1|%s|%s|%d", method, login, id.Expires.Unix())
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + s.sign(encoded), id
}

func (s *signer) sign(encoded string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parse verifies the signature before it reads anything out of the payload, so
// no field of a forged cookie is ever acted on.
func (s *signer) parse(raw string) (Identity, error) {
	encoded, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return Identity{}, errNoSession
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(encoded))) {
		return Identity{}, errors.New("operatorauth: session signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Identity{}, fmt.Errorf("operatorauth: session payload is not base64: %w", err)
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
