// Package disk is the single-node blob driver: one file per key under a root
// directory, written temp-then-rename so a reader never sees a partial object.
package disk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
)

// tmpDir holds in-progress writes. It is inside the root so a rename is never
// a cross-device copy, and it is reserved: no key may start with it.
const tmpDir = "_tmp"

// Store is a blob.Store backed by a directory.
type Store struct {
	root string
}

var _ blob.Store = (*Store)(nil)

// New prepares root, creating it if needed.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("blob/disk: root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob/disk: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, tmpDir), 0o755); err != nil {
		return nil, fmt.Errorf("blob/disk: create root %q: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// Root is the directory this store writes into.
func (s *Store) Root() string { return s.root }

// path maps a key to a file path that provably stays under the root.
func (s *Store) path(key string) (string, error) {
	if err := blob.ValidateKey(key); err != nil {
		return "", err
	}
	if key == tmpDir || strings.HasPrefix(key, tmpDir+"/") {
		return "", fmt.Errorf("%w: %q uses the reserved %q prefix", blob.ErrBadKey, key, tmpDir)
	}
	p := filepath.Join(s.root, filepath.FromSlash(key))
	// Belt and braces: ValidateKey already rejects traversal, but a mapping bug
	// here would be a directory-escape, so the result is checked too.
	rel, err := filepath.Rel(s.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the store root", blob.ErrBadKey, key)
	}
	return p, nil
}

// Put writes the whole object atomically and returns its size and hex sha256.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (int64, string, error) {
	p, err := s.path(key)
	if err != nil {
		return 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, "", fmt.Errorf("blob/disk: create directory for %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "put-")
	if err != nil {
		return 0, "", fmt.Errorf("blob/disk: create temp file for %s: %w", key, err)
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return 0, "", fmt.Errorf("blob/disk: write %s: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, "", fmt.Errorf("blob/disk: sync %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, "", fmt.Errorf("blob/disk: close %s: %w", key, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return 0, "", fmt.Errorf("blob/disk: commit %s: %w", key, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// PutAt writes a byte range into the object, creating it if needed. The object
// is visible while incomplete, so callers gate visibility on their own
// finalize step rather than on the file existing.
func (s *Store) PutAt(ctx context.Context, key string, offset int64, r io.Reader) (int64, error) {
	if offset < 0 {
		return 0, fmt.Errorf("blob/disk: negative offset %d for %s", offset, key)
	}
	p, err := s.path(key)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, fmt.Errorf("blob/disk: create directory for %s: %w", key, err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, fmt.Errorf("blob/disk: open %s: %w", key, err)
	}
	defer f.Close()
	n, err := io.Copy(io.NewOffsetWriter(f, offset), r)
	if err != nil {
		return 0, fmt.Errorf("blob/disk: write %s at %d: %w", key, offset, err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("blob/disk: sync %s: %w", key, err)
	}
	return n, nil
}

func (s *Store) open(key string) (*os.File, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("blob/disk: open %s: %w", key, err)
	}
	return f, nil
}

// Get streams the whole object.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.open(key)
}

// GetRange streams length bytes from off; a negative length reads to the end.
func (s *Store) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, fmt.Errorf("blob/disk: negative range offset %d for %s", off, key)
	}
	f, err := s.open(key)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("blob/disk: stat %s: %w", key, err)
	}
	if off > st.Size() {
		f.Close()
		return nil, fmt.Errorf("blob/disk: range offset %d is past the end of %s (%d bytes)", off, key, st.Size())
	}
	if length < 0 || off+length > st.Size() {
		length = st.Size() - off
	}
	return sectionReadCloser{SectionReader: io.NewSectionReader(f, off, length), f: f}, nil
}

type sectionReadCloser struct {
	*io.SectionReader
	f *os.File
}

func (s sectionReadCloser) Close() error { return s.f.Close() }

// Stat reports the object's size and modification time.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return blob.Info{}, err
	}
	p, err := s.path(key)
	if err != nil {
		return blob.Info{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return blob.Info{}, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	if err != nil {
		return blob.Info{}, fmt.Errorf("blob/disk: stat %s: %w", key, err)
	}
	if fi.IsDir() {
		return blob.Info{}, fmt.Errorf("%w: %s is a directory", blob.ErrNotFound, key)
	}
	return blob.Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Delete removes the object and any directories it leaves empty.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", blob.ErrNotFound, key)
		}
		return fmt.Errorf("blob/disk: delete %s: %w", key, err)
	}
	// Prune now-empty parents up to the root so an expired run leaves no tree.
	for dir := filepath.Dir(p); dir != s.root; dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break
		}
	}
	return nil
}

// SignedURL is not supported: a local directory has nothing to sign with, so
// the caller must proxy the bytes.
func (s *Store) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("%w: the disk driver cannot sign URLs; proxy the bytes instead", blob.ErrUnsupported)
}
