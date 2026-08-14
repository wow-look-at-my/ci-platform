package enrol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/signedvalue"
)

// Session is what a runner holds after proving it owns an approved key. It is
// short-lived: an approval that cannot be withdrawn until a token expires is
// not really a revocation, so the window is minutes, not days.
type Session struct {
	// Fingerprint names the approved host.
	Fingerprint string
	// RunnerID names which of that host's runners this is, so a host running
	// several does not have to share one identity.
	RunnerID string
	Expires  time.Time
}

// DefaultSessionTTL is how long a runner session token is good for. A runner
// renews well before this, and a revoked host stops being able to renew
// immediately, so this is the longest a revoked host keeps working.
const DefaultSessionTTL = 10 * time.Minute

// Sessions mints and verifies runner session tokens, and remembers the nonces
// it has seen so a captured signed request cannot be used twice.
type Sessions struct {
	key signedvalue.Key
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// NewSessions builds the minter. now is injectable for tests.
func NewSessions(key []byte, ttl time.Duration, now func() time.Time) (*Sessions, error) {
	if len(key) == 0 {
		return nil, errors.New("enrol: a session signing key is required")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Sessions{key: key, ttl: ttl, now: now, seen: map[string]time.Time{}}, nil
}

// TTL is how long the tokens this mints last.
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Mint issues a token for an approved host.
func (s *Sessions) Mint(fingerprint, runnerID string) (string, Session) {
	sess := Session{Fingerprint: fingerprint, RunnerID: runnerID, Expires: s.now().Add(s.ttl)}
	payload := fmt.Sprintf("v1|%s|%s|%d", fingerprint, runnerID, sess.Expires.Unix())
	return s.key.Sign([]byte(payload)), sess
}

// Parse verifies a token and returns the session it carries.
func (s *Sessions) Parse(token string) (Session, error) {
	payload, err := s.key.Open(strings.TrimSpace(token))
	if err != nil {
		return Session{}, err
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 4 || parts[0] != "v1" {
		return Session{}, errors.New("enrol: session token payload is not v1")
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return Session{}, fmt.Errorf("enrol: session token expiry is not a timestamp: %w", err)
	}
	sess := Session{Fingerprint: parts[1], RunnerID: parts[2], Expires: time.Unix(exp, 0)}
	if !s.now().Before(sess.Expires) {
		return Session{}, errors.New("enrol: session token has expired")
	}
	return sess, nil
}

// CheckFreshness rejects a signed request whose timestamp is outside the skew
// window, or whose nonce has already been used inside it.
//
// Both halves are required. Without the window, every nonce ever issued would
// have to be remembered forever. Without the nonce, anything that captured one
// request could replay it until the window closed.
func (s *Sessions) CheckFreshness(fingerprint string, at time.Time, nonce string) error {
	if nonce == "" {
		return errors.New("enrol: request carries no nonce")
	}
	now := s.now()
	if skew := now.Sub(at); skew > MaxClockSkew || skew < -MaxClockSkew {
		return fmt.Errorf("enrol: request timestamp is %s away from this server's clock (limit %s); "+
			"check the clock on the runner host", skew.Round(time.Second), MaxClockSkew)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(now)
	key := fingerprint + "\n" + nonce
	if _, dup := s.seen[key]; dup {
		return errors.New("enrol: this signed request has already been used")
	}
	s.seen[key] = now
	return nil
}

// evictLocked drops nonces older than the window they were checked against.
// Once a nonce's timestamp can no longer pass CheckFreshness, remembering it
// proves nothing.
func (s *Sessions) evictLocked(now time.Time) {
	for k, at := range s.seen {
		if now.Sub(at) > 2*MaxClockSkew {
			delete(s.seen, k)
		}
	}
}
