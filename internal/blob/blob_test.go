package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
)

func TestValidateKeyRejectsEscapes(t *testing.T) {
	bad := map[string]string{
		"empty":         "",
		"absolute":      "/etc/passwd",
		"parent":        "a/../../etc/passwd",
		"bare parent":   "..",
		"dot":           "a/./b",
		"nul":           "a\x00b",
		"backslash":     `a\b`,
		"double slash":  "a//b",
		"trailing":      "a/b/",
		"drive letter":  "c:/windows",
		"space padding": "a/ b",
		"too long":      strings.Repeat("a", 1025),
	}
	for name, key := range bad {
		t.Run(name, func(t *testing.T) {
			err := blob.ValidateKey(key)
			require.Error(t, err, "key %q must be rejected", key)
			assert.ErrorIs(t, err, blob.ErrBadKey)
		})
	}

	for _, key := range []string{"sha256/abc", "artifacts/12/name.zip", "a", "a/b/c.txt"} {
		assert.NoError(t, blob.ValidateKey(key), "key %q must be accepted", key)
	}
}

func TestContentKey(t *testing.T) {
	assert.Equal(t, "sha256/deadbeef", blob.ContentKey("deadbeef"))
}

func TestDigestOf(t *testing.T) {
	s := newDiskStore(t)
	ctx := context.Background()
	body := []byte("hello ci-platform")
	_, put, err := s.Put(ctx, "a/b", bytes.NewReader(body))
	require.NoError(t, err)

	want := sha256.Sum256(body)
	assert.Equal(t, hex.EncodeToString(want[:]), put)

	digest, size, err := blob.DigestOf(ctx, s, "a/b")
	require.NoError(t, err)
	assert.Equal(t, put, digest)
	assert.Equal(t, int64(len(body)), size)

	_, _, err = blob.DigestOf(ctx, s, "missing")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func newDiskStore(t *testing.T) *disk.Store {
	t.Helper()
	s, err := disk.New(t.TempDir())
	require.NoError(t, err)
	return s
}

// noPutAtStore wraps a store and refuses ranged writes, standing in for the S3
// driver so the staged-chunk path is exercised without a network.
type noPutAtStore struct {
	blob.Store
	putAtCalls int
}

func (n *noPutAtStore) PutAt(context.Context, string, int64, io.Reader) (int64, error) {
	n.putAtCalls++
	return 0, blob.ErrUnsupported
}

func TestChunkedUploadDirectPath(t *testing.T) {
	ctx := context.Background()
	s := newDiskStore(t)
	u, err := blob.NewChunkedUpload(s, "up/direct")
	require.NoError(t, err)

	require.NoError(t, u.WriteRange(ctx, 5, strings.NewReader("world")))
	require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("hello")))
	assert.False(t, u.Staged())

	size, digest, err := u.Commit(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(10), size)

	want := sha256.Sum256([]byte("helloworld"))
	assert.Equal(t, hex.EncodeToString(want[:]), digest)

	rc, err := s.Get(ctx, "up/direct")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "helloworld", string(got))
}

func TestChunkedUploadStagedPath(t *testing.T) {
	ctx := context.Background()
	base := newDiskStore(t)
	s := &noPutAtStore{Store: base}
	u, err := blob.NewChunkedUpload(s, "up/staged")
	require.NoError(t, err)

	require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abc")))
	require.NoError(t, u.WriteRange(ctx, 6, strings.NewReader("ghi")))
	require.NoError(t, u.WriteRange(ctx, 3, strings.NewReader("def")))
	assert.True(t, u.Staged())
	assert.Equal(t, 1, s.putAtCalls, "PutAt is probed once, then never again")

	size, digest, err := u.Commit(ctx, 9)
	require.NoError(t, err)
	assert.Equal(t, int64(9), size)
	want := sha256.Sum256([]byte("abcdefghi"))
	assert.Equal(t, hex.EncodeToString(want[:]), digest)

	rc, err := base.Get(ctx, "up/staged")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "abcdefghi", string(got))

	// Staged parts are cleaned up once assembled.
	_, err = base.Stat(ctx, partKeyFor("up/staged", 0))
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestChunkedUploadRejectsGapsAndOverlaps(t *testing.T) {
	ctx := context.Background()
	t.Run("gap", func(t *testing.T) {
		u, err := blob.NewChunkedUpload(newDiskStore(t), "up/gap")
		require.NoError(t, err)
		require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abc")))
		require.NoError(t, u.WriteRange(ctx, 10, strings.NewReader("xyz")))
		_, _, err = u.Commit(ctx, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "gap")
		assert.Contains(t, err.Error(), "bytes 3-9")
	})
	t.Run("overlap", func(t *testing.T) {
		u, err := blob.NewChunkedUpload(newDiskStore(t), "up/overlap")
		require.NoError(t, err)
		require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abcdef")))
		require.NoError(t, u.WriteRange(ctx, 3, strings.NewReader("xyz")))
		_, _, err = u.Commit(ctx, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overlapping")
	})
	t.Run("declared size mismatch", func(t *testing.T) {
		u, err := blob.NewChunkedUpload(newDiskStore(t), "up/short")
		require.NoError(t, err)
		require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abc")))
		_, _, err = u.Commit(ctx, 99)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "declared as 99")
	})
	t.Run("no chunks", func(t *testing.T) {
		u, err := blob.NewChunkedUpload(newDiskStore(t), "up/empty")
		require.NoError(t, err)
		_, _, err = u.Commit(ctx, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no chunks")
	})
}

func TestChunkedUploadAbort(t *testing.T) {
	ctx := context.Background()
	s := newDiskStore(t)
	u, err := blob.NewChunkedUpload(s, "up/aborted")
	require.NoError(t, err)
	require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abc")))
	require.NoError(t, u.Abort(ctx))

	_, err = s.Stat(ctx, "up/aborted")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	_, _, err = u.Commit(ctx, -1)
	assert.Error(t, err, "a committed or aborted upload cannot be reused")
}

func TestChunkedUploadAbortStaged(t *testing.T) {
	ctx := context.Background()
	base := newDiskStore(t)
	s := &noPutAtStore{Store: base}
	u, err := blob.NewChunkedUpload(s, "up/aborted-staged")
	require.NoError(t, err)
	require.NoError(t, u.WriteRange(ctx, 0, strings.NewReader("abc")))
	require.NoError(t, u.Abort(ctx))

	_, err = base.Stat(ctx, partKeyFor("up/aborted-staged", 0))
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestChunkedUploadRejectsBadInput(t *testing.T) {
	_, err := blob.NewChunkedUpload(newDiskStore(t), "../escape")
	assert.ErrorIs(t, err, blob.ErrBadKey)

	u, err := blob.NewChunkedUpload(newDiskStore(t), "ok")
	require.NoError(t, err)
	assert.Error(t, u.WriteRange(context.Background(), -1, strings.NewReader("x")))
}

// failingStore reports a storage outage on every write, proving an upload
// fails with a named cause rather than silently producing an empty object.
type failingStore struct{ blob.Store }

func (failingStore) Put(context.Context, string, io.Reader) (int64, string, error) {
	return 0, "", errors.New("bucket unreachable")
}

func (failingStore) PutAt(context.Context, string, int64, io.Reader) (int64, error) {
	return 0, errors.New("bucket unreachable")
}

func TestChunkedUploadSurfacesStorageFailure(t *testing.T) {
	u, err := blob.NewChunkedUpload(failingStore{newDiskStore(t)}, "up/broken")
	require.NoError(t, err)
	err = u.WriteRange(context.Background(), 0, strings.NewReader("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket unreachable")
}

func TestDiskSignedURLIsUnsupported(t *testing.T) {
	_, err := newDiskStore(t).SignedURL(context.Background(), "k", time.Minute)
	assert.ErrorIs(t, err, blob.ErrUnsupported)
}

// partKeyFor mirrors the staging namespace ChunkedUpload uses, so the cleanup
// assertions above check the key that is actually written.
func partKeyFor(key string, off int64) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("_parts/%s/%020d", hex.EncodeToString(sum[:]), off)
}
