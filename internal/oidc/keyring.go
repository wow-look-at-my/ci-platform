package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
)

// KeyBits is the RSA modulus size. RS256 with 2048 bits is what GitHub's own
// OIDC keys use, and what every verifier already accepts.
const KeyBits = 2048

// StoredKey is one signing key as persisted.
type StoredKey struct {
	KID        string    `json:"kid"`
	PrivatePEM string    `json:"private_pem"`
	CreatedAt  time.Time `json:"created_at"`
	// RetiredAt is when the key stopped signing. A retired key stays in JWKS
	// until every token it signed has expired.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// KeyStore persists the keyring. Keys must survive a restart: regenerating
// them in memory would invalidate every token already issued and every
// verifier's cached JWKS, silently, which is the failure mode this platform
// exists to avoid.
type KeyStore interface {
	Load(ctx context.Context) ([]StoredKey, error)
	Save(ctx context.Context, keys []StoredKey) error
}

// FileKeyStore persists the keyring to a JSON file with 0600 permissions.
type FileKeyStore struct{ path string }

// NewFileKeyStore stores keys at path.
func NewFileKeyStore(path string) (*FileKeyStore, error) {
	if path == "" {
		return nil, errors.New("oidc: key file path is required")
	}
	return &FileKeyStore{path: path}, nil
}

// Load reads the keyring, returning nil for a file that does not exist yet.
func (f *FileKeyStore) Load(context.Context) ([]StoredKey, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: read key file %s: %w", f.path, err)
	}
	var keys []StoredKey
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("oidc: key file %s is corrupt: %w", f.path, err)
	}
	return keys, nil
}

// Save writes the keyring atomically.
func (f *FileKeyStore) Save(_ context.Context, keys []StoredKey) error {
	b, err := json.MarshalIndent(keys, "", "\t")
	if err != nil {
		return fmt.Errorf("oidc: encode keyring: %w", err)
	}
	if dir := filepath.Dir(f.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("oidc: create key directory %s: %w", dir, err)
		}
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("oidc: write key file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("oidc: commit key file %s: %w", f.path, err)
	}
	return nil
}

// BlobKeyStore persists the keyring in the blob store, for a control plane
// with no durable local disk.
type BlobKeyStore struct {
	store blob.Store
	key   string
}

// NewBlobKeyStore stores keys at key in s.
func NewBlobKeyStore(s blob.Store, key string) (*BlobKeyStore, error) {
	if s == nil {
		return nil, errors.New("oidc: blob store is required")
	}
	if err := blob.ValidateKey(key); err != nil {
		return nil, err
	}
	return &BlobKeyStore{store: s, key: key}, nil
}

// Load reads the keyring, returning nil when it has not been written yet.
func (b *BlobKeyStore) Load(ctx context.Context) ([]StoredKey, error) {
	rc, err := b.store.Get(ctx, b.key)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: read keyring %s: %w", b.key, err)
	}
	defer rc.Close()
	var keys []StoredKey
	if err := json.NewDecoder(rc).Decode(&keys); err != nil {
		return nil, fmt.Errorf("oidc: keyring %s is corrupt: %w", b.key, err)
	}
	return keys, nil
}

// Save writes the keyring.
func (b *BlobKeyStore) Save(ctx context.Context, keys []StoredKey) error {
	body, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("oidc: encode keyring: %w", err)
	}
	if _, _, err := b.store.Put(ctx, b.key, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("oidc: write keyring %s: %w", b.key, err)
	}
	return nil
}

// KeyringOptions configures a Keyring.
type KeyringOptions struct {
	// TokenTTL is how long an issued token stays valid. A retired key is served
	// in JWKS for this long after retirement, then dropped: dropping it sooner
	// would break tokens still in flight, later would serve a key nothing can
	// use.
	TokenTTL time.Duration
	Now      func() time.Time
}

// Keyring holds the active signing key plus retired keys still in JWKS.
type Keyring struct {
	store    KeyStore
	tokenTTL time.Duration
	now      func() time.Time

	mu     sync.RWMutex
	keys   []keyEntry
	active *keyEntry
}

type keyEntry struct {
	kid       string
	priv      *rsa.PrivateKey
	createdAt time.Time
	retiredAt *time.Time
}

// NewKeyring loads the keyring, generating and persisting a first key when the
// store is empty. A key that cannot be persisted is a startup failure: an
// in-memory fallback would invalidate every token on the next restart without
// anyone being told.
func NewKeyring(ctx context.Context, store KeyStore, opts KeyringOptions) (*Keyring, error) {
	if store == nil {
		return nil, errors.New("oidc: a KeyStore is required; keys must outlive the process")
	}
	if opts.TokenTTL <= 0 {
		return nil, errors.New("oidc: KeyringOptions.TokenTTL must be positive")
	}
	k := &Keyring{store: store, tokenTTL: opts.TokenTTL, now: opts.Now}
	if k.now == nil {
		k.now = time.Now
	}

	stored, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, sk := range stored {
		e, err := decodeKey(sk)
		if err != nil {
			return nil, err
		}
		k.keys = append(k.keys, e)
	}
	for i := range k.keys {
		if k.keys[i].retiredAt == nil {
			k.active = &k.keys[i]
		}
	}
	if k.active == nil {
		if err := k.rotate(ctx); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func decodeKey(sk StoredKey) (keyEntry, error) {
	block, _ := pem.Decode([]byte(sk.PrivatePEM))
	if block == nil {
		return keyEntry{}, fmt.Errorf("oidc: key %s is not valid PEM", sk.KID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return keyEntry{}, fmt.Errorf("oidc: key %s: %w", sk.KID, err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return keyEntry{}, fmt.Errorf("oidc: key %s is a %T, not an RSA key", sk.KID, parsed)
	}
	return keyEntry{kid: sk.KID, priv: priv, createdAt: sk.CreatedAt, retiredAt: sk.RetiredAt}, nil
}

// Rotate generates a new active key, retires the current one, and drops keys
// whose tokens can no longer be valid.
func (k *Keyring) Rotate(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.rotate(ctx)
}

func (k *Keyring) rotate(ctx context.Context) error {
	priv, err := rsa.GenerateKey(rand.Reader, KeyBits)
	if err != nil {
		return fmt.Errorf("oidc: generate signing key: %w", err)
	}
	now := k.now()
	if k.active != nil {
		retired := now
		k.active.retiredAt = &retired
	}
	k.keys = append(k.keys, keyEntry{kid: thumbprint(&priv.PublicKey), priv: priv, createdAt: now})
	k.active = &k.keys[len(k.keys)-1]

	// A key is only dropped once nothing it signed can still be valid.
	kept := k.keys[:0]
	for _, e := range k.keys {
		if e.retiredAt != nil && now.Sub(*e.retiredAt) > k.tokenTTL {
			continue
		}
		kept = append(kept, e)
	}
	k.keys = kept
	for i := range k.keys {
		if k.keys[i].retiredAt == nil {
			k.active = &k.keys[i]
		}
	}

	if err := k.store.Save(ctx, k.stored()); err != nil {
		return fmt.Errorf("oidc: persist keyring: %w", err)
	}
	return nil
}

func (k *Keyring) stored() []StoredKey {
	out := make([]StoredKey, 0, len(k.keys))
	for _, e := range k.keys {
		der, err := x509.MarshalPKCS8PrivateKey(e.priv)
		if err != nil {
			// MarshalPKCS8PrivateKey cannot fail for an RSA key produced by
			// GenerateKey or parsed by decodeKey.
			continue
		}
		out = append(out, StoredKey{
			KID:        e.kid,
			PrivatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
			CreatedAt:  e.createdAt,
			RetiredAt:  e.retiredAt,
		})
	}
	return out
}

// Active returns the key new tokens are signed with.
func (k *Keyring) Active() (*rsa.PrivateKey, string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active.priv, k.active.kid
}

// JWK is one public key in the JWKS document.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWKS is the document served at /.well-known/jwks.json.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKS returns every key a verifier may still need: the active one and each
// retired key whose tokens have not all expired.
func (k *Keyring) JWKS() JWKS {
	k.mu.RLock()
	defer k.mu.RUnlock()
	now := k.now()
	out := JWKS{Keys: make([]JWK, 0, len(k.keys))}
	for _, e := range k.keys {
		if e.retiredAt != nil && now.Sub(*e.retiredAt) > k.tokenTTL {
			continue
		}
		out.Keys = append(out.Keys, JWK{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: e.kid,
			N:   base64.RawURLEncoding.EncodeToString(e.priv.PublicKey.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(e.priv.PublicKey.E)).Bytes()),
		})
	}
	return out
}

// thumbprint derives a kid from the public key, so the same key always has the
// same id no matter how many times it is loaded.
func thumbprint(pub *rsa.PublicKey) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	sum := sha256.Sum256([]byte(`{"e":"` + e + `","kty":"RSA","n":"` + n + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
