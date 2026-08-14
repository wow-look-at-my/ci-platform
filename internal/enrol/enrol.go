// Package enrol is how a runner host proves which machine it is, without
// anybody having to invent a password.
//
// A host generates an Ed25519 keypair the first time it starts, keeps the
// private half on disk, and sends the public half once. The control plane
// records it as pending and does nothing else with it. An operator approves the
// fingerprint by eye, the same way they would an SSH host key, and only then
// can that host exchange a signature for a session token and start taking jobs.
//
// The properties this buys over a shared token: nothing secret is ever
// transmitted or typed, a compromised host is revoked on its own without
// re-keying the fleet, every request is attributable to one machine, and a host
// that has not been approved is inert rather than trusted-by-default.
//
// see docs/runners.md
package enrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pemType is the label in the on-disk key file.
const pemType = "CI PLATFORM RUNNER HOST KEY"

// SignaturePurpose prefixes every signed message. A signature made for one
// purpose must never verify for another, and the prefix is what stops a
// captured enrolment signature being replayed as a session request.
const (
	PurposeEnrol   = "ci-platform-runner-enrol"
	PurposeSession = "ci-platform-runner-session"
)

// MaxClockSkew bounds how far a signed request's timestamp may be from the
// control plane's clock. It is the replay window, so it is small; a host whose
// clock is further out than this fails loudly rather than intermittently.
const MaxClockSkew = 2 * time.Minute

// Fingerprint names a public key the way ssh does, so an operator comparing
// what the host printed with what the dashboard shows is doing a familiar job.
//
// The digest is base64url rather than standard base64: a fingerprint is a path
// segment in the approval API, and standard base64's "/" would split it in two.
// It differs from ssh's own output only in the two substituted characters, so
// it still reads as the thing it is.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "SHA256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParsePublicKey reads the base64 form a host sends over the wire.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("enrol: public key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("enrol: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey is the wire form.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// LoadOrCreateKey returns the host's key, generating and saving one the first
// time. The second result reports whether it was just created, so the caller
// can print the fingerprint an operator now has to approve.
//
// The file is written 0600 through a temp file and a rename, so a crash cannot
// leave a half-written key that reads as a different host.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, bool, error) {
	switch buf, err := os.ReadFile(path); {
	case err == nil:
		key, err := decodeKey(buf)
		if err != nil {
			return nil, false, fmt.Errorf("enrol: %s: %w", path, err)
		}
		return key, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("enrol: read %s: %w", path, err)
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("enrol: generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("enrol: create key directory: %w", err)
	}
	block := &pem.Block{Type: pemType, Bytes: key.Seed()}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, false, fmt.Errorf("enrol: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, false, fmt.Errorf("enrol: install %s: %w", path, err)
	}
	return key, true, nil
}

func decodeKey(buf []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(buf)
	if block == nil {
		return nil, errors.New("not a PEM file")
	}
	if block.Type != pemType {
		return nil, fmt.Errorf("PEM block is %q, want %q", block.Type, pemType)
	}
	if len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("key seed is %d bytes, want %d", len(block.Bytes), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(block.Bytes), nil
}

// Message is what a host signs. Every field the control plane will act on is in
// here: change any of them and the signature stops verifying.
//
// The nonce makes each message unique, and the timestamp bounds how long a
// captured one is worth anything. Neither is sufficient alone -- a nonce with
// no expiry needs an unbounded record of every nonce ever seen, and a timestamp
// with no nonce lets a captured message be replayed for the whole skew window.
func Message(purpose, fingerprint string, at time.Time, nonce string) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%d\n%s", purpose, fingerprint, at.UTC().Unix(), nonce)
}

// Sign produces the signature for a message.
func Sign(key ed25519.PrivateKey, purpose, fingerprint string, at time.Time, nonce string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, Message(purpose, fingerprint, at, nonce)))
}

// Verify checks a signature against a public key.
func Verify(pub ed25519.PublicKey, purpose, fingerprint string, at time.Time, nonce, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return fmt.Errorf("enrol: signature is not base64: %w", err)
	}
	if !ed25519.Verify(pub, Message(purpose, fingerprint, at, nonce), sig) {
		return errors.New("enrol: signature does not verify against the enrolled public key")
	}
	return nil
}

// NewNonce returns a fresh nonce for a signed request.
func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("enrol: generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
