package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// TokenSource supplies the bearer token for each control-plane call.
//
// It is an interface because the token is short-lived by design: a runner holds
// a key, not a password, and trades a signature for a few minutes of access
// whenever it needs one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Invalidate discards the cached token after the control plane rejects it,
	// so the next call signs a fresh one instead of replaying a dead one.
	Invalidate()
}

// StaticToken is a TokenSource holding one value, for tests.
type StaticToken string

// Token returns the fixed value.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// Invalidate does nothing: there is nothing to refresh.
func (s StaticToken) Invalidate() {}

// PlaintextWarning returns a sentence to log when a runner is about to talk to
// the control plane in the clear, and "" when it is not.
//
// It is a warning rather than a refusal because a LAN deployment pointing
// straight at the coordinator is a legitimate, wanted setup. What is not
// legitimate is doing it without knowing: an assignment carries the job's
// secrets and its token, so on a network somebody else is on, "plain HTTP"
// means "those secrets are readable".
//
// Loopback is exempt: there is no network to sniff.
func PlaintextWarning(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost") {
		return ""
	}
	return "connecting to the control plane over plain HTTP: a job's secrets and its token travel " +
		"unencrypted over this network. That is fine on a network you trust and nowhere else; " +
		"point this runner at the https public URL instead if it is not."
}

// ErrNotApproved is what a host gets until an operator approves its
// fingerprint. It is a distinct error because it is not a fault: it is the
// normal state of a machine somebody just set up, and the runner waits it out
// rather than crash-looping.
var ErrNotApproved = errors.New("agent: this runner host has not been approved yet")

// Credentials proves this host's identity with its Ed25519 key and keeps a
// session token fresh.
type Credentials struct {
	baseURL     string
	key         ed25519.PrivateKey
	fingerprint string
	runnerID    string
	http        *http.Client
	now         func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
	labels  []string
}

// CredentialsConfig configures the token source.
type CredentialsConfig struct {
	BaseURL string
	Key     ed25519.PrivateKey
	// RunnerID names which of the host's runners this is.
	RunnerID string
	HTTP     *http.Client
	Now      func() time.Time
}

// NewCredentials builds the token source.
func NewCredentials(cfg CredentialsConfig) (*Credentials, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("agent: control plane URL is required")
	}
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("agent: a host key is required; generate one with runner-host or ci-runner enrol")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	pub, ok := cfg.Key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("agent: host key is not an Ed25519 key")
	}
	return &Credentials{
		baseURL:     strings.TrimSuffix(cfg.BaseURL, "/"),
		key:         cfg.Key,
		fingerprint: enrol.Fingerprint(pub),
		runnerID:    cfg.RunnerID,
		http:        cfg.HTTP,
		now:         cfg.Now,
	}, nil
}

// Fingerprint is what an operator approves.
func (c *Credentials) Fingerprint() string { return c.fingerprint }

// Labels are what the operator allows this host to serve, learned on the last
// session exchange. Empty means the operator set no restriction.
func (c *Credentials) Labels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.labels...)
}

// Enrol offers this host's public key. It is safe to call on every start: the
// control plane records the first sighting and returns the existing state
// afterwards, so restarting neither resets an approval nor creates a duplicate.
func (c *Credentials) Enrol(ctx context.Context, name, os, arch, version string, labels []string) (protocol.EnrolResponse, error) {
	pub, _ := c.key.Public().(ed25519.PublicKey)
	at := c.now()
	nonce, err := enrol.NewNonce()
	if err != nil {
		return protocol.EnrolResponse{}, err
	}
	req := protocol.EnrolRequest{
		APIVersion: protocol.APIVersion,
		PublicKey:  enrol.EncodePublicKey(pub),
		Name:       name, OS: os, Arch: arch, Version: version, Labels: labels,
		Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(c.key, enrol.PurposeEnrol, c.fingerprint, at, nonce),
	}
	var out protocol.EnrolResponse
	if err := c.call(ctx, protocol.PathEnrol, req, &out); err != nil {
		return protocol.EnrolResponse{}, err
	}
	return out, nil
}

// renewBefore is how much of a token's life is left when it is replaced. A
// token that expires mid-job would fail a log flush or a completion report,
// which is the one moment a runner cannot afford to lose access.
const renewBefore = 2 * time.Minute

// Token returns a valid session token, exchanging a signature for a new one
// when the current one is close to expiring.
func (c *Credentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && c.now().Add(renewBefore).Before(c.expires) {
		defer c.mu.Unlock()
		return c.token, nil
	}
	c.mu.Unlock()

	at := c.now()
	nonce, err := enrol.NewNonce()
	if err != nil {
		return "", err
	}
	req := protocol.SessionRequest{
		APIVersion: protocol.APIVersion, Fingerprint: c.fingerprint, RunnerID: c.runnerID,
		Timestamp: at.Unix(), Nonce: nonce,
		Signature: enrol.Sign(c.key, enrol.PurposeSession, c.fingerprint, at, nonce),
	}
	var out protocol.SessionResponse
	if err := c.call(ctx, protocol.PathSession, req, &out); err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expires, c.labels = out.Token, out.ExpiresAt, out.Labels
	return c.token, nil
}

// Invalidate drops the cached token so the next Token call signs a new one.
func (c *Credentials) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expires = "", time.Time{}
}

// call posts a signed request. It does not retry: the caller decides how to
// wait, and a signed request has a timestamp that would go stale in a retry
// loop anyway.
func (c *Credentials) call(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CI-Api-Version", protocol.APIVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %s: %w", path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("agent: %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s", ErrNotApproved, message(payload))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent: %s answered %s: %s", path, resp.Status, message(payload))
	}
	return json.Unmarshal(payload, out)
}

// message pulls the human sentence out of an error body, falling back to the
// raw body so a failure is never reported as an empty string.
func message(payload []byte) string {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil {
		if body.Message != "" {
			return body.Message
		}
		if body.Error != "" {
			return body.Error
		}
	}
	return strings.TrimSpace(string(payload))
}
