package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

func TestKeyStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "started-keys")

	s, err := OpenKeyStore(path)
	require.NoError(t, err)
	assert.False(t, s.Started("1/2/1"))
	require.NoError(t, s.MarkStarted("1/2/1"))
	assert.True(t, s.Started("1/2/1"))
	require.NoError(t, s.MarkStarted("1/2/1"), "recording a key twice is not an error")
	assert.Equal(t, 1, s.Len())
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "closing twice is safe")

	// A crash and restart must not forget what already ran.
	s2, err := OpenKeyStore(path)
	require.NoError(t, err)
	defer s2.Close()
	assert.True(t, s2.Started("1/2/1"))
	assert.False(t, s2.Started("1/2/2"))
}

func TestKeyStoreRejectsAnEmptyKey(t *testing.T) {
	s, err := OpenKeyStore(filepath.Join(t.TempDir(), "keys"))
	require.NoError(t, err)
	defer s.Close()
	require.Error(t, s.MarkStarted("  "), "an empty key would make every job look already-started")
}

func TestKeyStoreReportsAnUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: opening it must fail loudly.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "keys"), 0o755))
	_, err := OpenKeyStore(filepath.Join(dir, "keys"))
	require.Error(t, err)
}

func TestLogSinkBatchesBySize(t *testing.T) {
	cp := newFakeControlPlane()
	s := NewLogSink(LogSinkConfig{
		Client: cp, RunnerID: "r", JobID: 1, Attempt: 1,
		MaxLines: 3, Interval: time.Hour, // only the size threshold can fire
	})

	for i := 0; i < 5; i++ {
		s.Line(1, "stdout", "", "line")
	}
	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.logs, 1, "a batch ships when it fills, not per line")
		assert.Len(t, f.logs[0].Lines, 3)
	})

	require.NoError(t, s.Close(context.Background()))
	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.logs, 2, "the remainder ships on close")
		assert.Len(t, f.logs[1].Lines, 2)
	})
}

func TestLogSinkFlushesOnInterval(t *testing.T) {
	cp := newFakeControlPlane()
	s := NewLogSink(LogSinkConfig{
		Client: cp, RunnerID: "r", JobID: 1, Attempt: 1,
		MaxLines: 1000, Interval: 2 * time.Millisecond,
	})
	s.Start(context.Background())
	defer s.Close(context.Background())

	s.Line(1, "stdout", "", "eventually")
	require.Eventually(t, func() bool {
		var n int
		cp.snapshot(func(f *fakeControlPlane) { n = len(f.logs) })
		return n > 0
	}, time.Second, time.Millisecond, "a quiet build must not sit on its logs")
}

func TestLogSinkAssignsSequenceAndMetadata(t *testing.T) {
	cp := newFakeControlPlane()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	s := NewLogSink(LogSinkConfig{
		Client: cp, RunnerID: "r", JobID: 5, Attempt: 2,
		MaxLines: 100, Interval: time.Hour, Now: func() time.Time { return now },
	})
	s.Line(3, "stderr", "Compile", "boom")
	s.Line(3, "stdout", "Compile", "ok")
	assert.Equal(t, int64(2), s.Seq())
	require.NoError(t, s.Flush(context.Background()))

	cp.snapshot(func(f *fakeControlPlane) {
		require.Len(t, f.logs, 1)
		b := f.logs[0]
		assert.Equal(t, int64(5), b.JobID)
		assert.Equal(t, 2, b.Attempt)
		require.Len(t, b.Lines, 2)
		assert.Equal(t, int64(1), b.Lines[0].Seq)
		assert.Equal(t, int64(2), b.Lines[1].Seq)
		assert.Equal(t, 3, b.Lines[0].StepNumber)
		assert.Equal(t, "stderr", b.Lines[0].Stream)
		assert.Equal(t, "Compile", b.Lines[0].Group)
		assert.Equal(t, now, b.Lines[0].Timestamp)
	})
}

func TestLogSinkMasksBeforeBuffering(t *testing.T) {
	cp := newFakeControlPlane()
	m := mask.New()
	m.Add("top-secret-value")
	s := NewLogSink(LogSinkConfig{
		Client: cp, RunnerID: "r", JobID: 1, Attempt: 1,
		Masker: m, MaxLines: 100, Interval: time.Hour,
	})
	s.Line(1, "stdout", "", "the key is top-secret-value")
	require.NoError(t, s.Flush(context.Background()))

	text := cp.logText()
	assert.NotContains(t, text, "top-secret-value")
	assert.Contains(t, text, "***")
}

func TestLogSinkSurfacesDeliveryFailures(t *testing.T) {
	cp := newFakeControlPlane()
	cp.logErr = errors.New("HTTP 503")
	var reported error
	s := NewLogSink(LogSinkConfig{
		Client: cp, RunnerID: "r", JobID: 1, Attempt: 1,
		MaxLines: 100, Interval: time.Hour,
		OnError: func(err error) { reported = err },
	})
	s.Line(1, "stdout", "", "x")
	require.Error(t, s.Flush(context.Background()))
	require.Error(t, reported, "a dropped log batch is never silent")
	require.Error(t, s.Err())
}

func TestLogSinkFlushWithNothingBufferedIsANoop(t *testing.T) {
	cp := newFakeControlPlane()
	s := NewLogSink(LogSinkConfig{Client: cp, RunnerID: "r"})
	require.NoError(t, s.Flush(context.Background()))
	cp.snapshot(func(f *fakeControlPlane) { assert.Empty(t, f.logs) })
	require.NoError(t, s.Close(context.Background()))
	require.NoError(t, s.Close(context.Background()), "closing twice is safe")
}
