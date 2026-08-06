package disk_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
)

func newStore(t *testing.T) (*disk.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := disk.New(root)
	require.NoError(t, err)
	return s, root
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	_, err := disk.New("")
	assert.Error(t, err)
}

func TestPutGetStatDelete(t *testing.T) {
	ctx := context.Background()
	s, root := newStore(t)

	size, digest, err := s.Put(ctx, "runs/7/artifact.zip", strings.NewReader("payload"))
	require.NoError(t, err)
	assert.Equal(t, int64(7), size)
	assert.Len(t, digest, 64)
	assert.Equal(t, root, s.Root())

	info, err := s.Stat(ctx, "runs/7/artifact.zip")
	require.NoError(t, err)
	assert.Equal(t, int64(7), info.Size)
	assert.Equal(t, "runs/7/artifact.zip", info.Key)
	assert.False(t, info.ModTime.IsZero())

	rc, err := s.Get(ctx, "runs/7/artifact.zip")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "payload", string(got))

	require.NoError(t, s.Delete(ctx, "runs/7/artifact.zip"))
	_, err = s.Stat(ctx, "runs/7/artifact.zip")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	// The now-empty run directory is pruned rather than left behind.
	_, err = os.Stat(filepath.Join(root, "runs", "7"))
	assert.True(t, os.IsNotExist(err))
}

func TestPutIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, root := newStore(t)

	_, _, err := s.Put(ctx, "k", io.MultiReader(strings.NewReader("abc"), errReader{}))
	require.Error(t, err)

	_, err = s.Stat(ctx, "k")
	assert.ErrorIs(t, err, blob.ErrNotFound, "a failed Put must leave no object")

	entries, err := os.ReadDir(filepath.Join(root, "_tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries, "the temp file must be cleaned up")
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, assertErr }

var assertErr = io.ErrUnexpectedEOF

func TestKeysCannotEscapeRoot(t *testing.T) {
	ctx := context.Background()
	s, root := newStore(t)

	outside := filepath.Join(filepath.Dir(root), "escaped.txt")
	for _, key := range []string{
		"../escaped.txt",
		"a/../../escaped.txt",
		"/etc/passwd",
		"a\x00b",
		`..\escaped.txt`,
		"_tmp/x",
		"_tmp",
	} {
		_, _, err := s.Put(ctx, key, strings.NewReader("x"))
		require.Error(t, err, "key %q must be rejected", key)

		_, err = s.Stat(ctx, key)
		require.Error(t, err)
		_, err = s.Get(ctx, key)
		require.Error(t, err)
		require.Error(t, s.Delete(ctx, key))
		_, err = s.PutAt(ctx, key, 0, strings.NewReader("x"))
		require.Error(t, err)
	}
	_, err := os.Stat(outside)
	assert.True(t, os.IsNotExist(err), "nothing may be written outside the root")
}

func TestPutAt(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	n, err := s.PutAt(ctx, "chunked", 4, strings.NewReader("efgh"))
	require.NoError(t, err)
	assert.Equal(t, int64(4), n)

	_, err = s.PutAt(ctx, "chunked", 0, strings.NewReader("abcd"))
	require.NoError(t, err)

	rc, err := s.Get(ctx, "chunked")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "abcdefgh", string(got))

	_, err = s.PutAt(ctx, "chunked", -1, strings.NewReader("x"))
	assert.Error(t, err)
}

func TestGetRange(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, _, err := s.Put(ctx, "k", bytes.NewReader([]byte("0123456789")))
	require.NoError(t, err)

	read := func(off, length int64) string {
		rc, err := s.GetRange(ctx, "k", off, length)
		require.NoError(t, err)
		defer rc.Close()
		b, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(b)
	}
	assert.Equal(t, "234", read(2, 3))
	assert.Equal(t, "56789", read(5, -1))
	assert.Equal(t, "89", read(8, 100), "a range past the end is clamped")

	_, err = s.GetRange(ctx, "k", 99, 1)
	assert.Error(t, err)
	_, err = s.GetRange(ctx, "k", -1, 1)
	assert.Error(t, err)
	_, err = s.GetRange(ctx, "missing", 0, 1)
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestMissingObjectsReportNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, err := s.Get(ctx, "nope")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = s.Stat(ctx, "nope")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	assert.ErrorIs(t, s.Delete(ctx, "nope"), blob.ErrNotFound)
}

func TestStatOnDirectoryIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, _, err := s.Put(ctx, "dir/file", strings.NewReader("x"))
	require.NoError(t, err)
	_, err = s.Stat(ctx, "dir")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestCancelledContextIsRefused(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.Put(ctx, "k", strings.NewReader("x"))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.PutAt(ctx, "k", 0, strings.NewReader("x"))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.Get(ctx, "k")
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.GetRange(ctx, "k", 0, 1)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.Stat(ctx, "k")
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, s.Delete(ctx, "k"), context.Canceled)
}
