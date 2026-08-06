// Package blob is the byte store behind artifacts, caches, and sealed job logs.
//
// Keys are path-shaped and validated: a key can never escape the driver's root.
// Immutable content is stored under "sha256/<hex>" (see ContentKey); mutable
// per-run paths are the caller's business.
//
// Drivers live in blob/disk (single node) and blob/s3 (S3-compatible).
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned by Get, GetRange, Stat, and Delete for a key that
	// does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrUnsupported is returned by a driver that cannot perform an optional
	// operation. The caller must handle it, never treat it as success.
	ErrUnsupported = errors.New("blob: operation not supported by this driver")
	// ErrBadKey is returned for a key that is empty, absolute, contains NUL, or
	// contains a ".." element.
	ErrBadKey = errors.New("blob: invalid key")
)

// Info is what a driver knows about a stored object without reading it. There
// is deliberately no digest field: neither driver tracks the sha256 of an
// object it did not hash itself, and an S3 ETag is an MD5, so reporting one
// here would be a lie. Use DigestOf when the digest must be known.
type Info struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Store is content storage. Every method returns a named error; no method
// reports success for work it did not do.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (size int64, digest string, err error)
	// PutAt writes a byte range, for chunked/resumable uploads. Drivers that
	// cannot write at an offset return ErrUnsupported.
	PutAt(ctx context.Context, key string, offset int64, r io.Reader) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetRange reads length bytes from off; a negative length reads to the end.
	GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (Info, error)
	Delete(ctx context.Context, key string) error
	// SignedURL is optional; drivers that cannot sign return ErrUnsupported and
	// the caller proxies the bytes instead.
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ContentPrefix is the namespace for content-addressed keys.
const ContentPrefix = "sha256/"

// ContentKey builds the immutable key for a hex sha256 digest.
func ContentKey(hexDigest string) string { return ContentPrefix + hexDigest }

// maxKeyLen bounds a key so a disk driver cannot be handed a path the
// filesystem will reject halfway through a write.
const maxKeyLen = 1024

// ValidateKey rejects every key that could escape a driver's root or name a
// path component the driver reserves.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrBadKey)
	case len(key) > maxKeyLen:
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrBadKey, len(key), maxKeyLen)
	case strings.ContainsRune(key, 0):
		return fmt.Errorf("%w: contains NUL", ErrBadKey)
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrBadKey, key)
	case strings.Contains(key, `\`):
		return fmt.Errorf("%w: %q contains a backslash", ErrBadKey, key)
	case strings.HasSuffix(key, "/"):
		return fmt.Errorf("%w: %q ends in a separator", ErrBadKey, key)
	}
	if len(key) > 1 && key[1] == ':' {
		return fmt.Errorf("%w: %q looks like a drive-letter path", ErrBadKey, key)
	}
	for _, el := range strings.Split(key, "/") {
		switch el {
		case "":
			return fmt.Errorf("%w: %q has an empty path element", ErrBadKey, key)
		case ".", "..":
			return fmt.Errorf("%w: %q has a %q element", ErrBadKey, key, el)
		}
		if strings.TrimSpace(el) != el {
			return fmt.Errorf("%w: %q has an element with leading or trailing space", ErrBadKey, key)
		}
	}
	return nil
}

// DigestOf streams an object and returns its hex sha256 and size. It is the
// only honest way to learn the digest of bytes the store did not hash on Put,
// which is every chunked upload.
func DigestOf(ctx context.Context, s Store, key string) (string, int64, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
