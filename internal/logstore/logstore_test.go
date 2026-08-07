package logstore_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/logstore"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func newLog(t *testing.T, bs blob.Store, mutate func(*logstore.Options)) *logstore.Log {
	t.Helper()
	o := logstore.Options{Blob: bs}
	if mutate != nil {
		mutate(&o)
	}
	l, err := logstore.New(o)
	require.NoError(t, err)
	return l
}

func newBlob(t *testing.T) *disk.Store {
	t.Helper()
	s, err := disk.New(t.TempDir())
	require.NoError(t, err)
	return s
}

func lines(texts ...string) []model.LogLine {
	out := make([]model.LogLine, len(texts))
	for i, s := range texts {
		out[i] = model.LogLine{Stream: "stdout", Text: s}
	}
	return out
}

func TestNewRequiresBlobStore(t *testing.T) {
	_, err := logstore.New(logstore.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blob store is required")
}

func TestAppendAssignsMonotonicSeq(t *testing.T) {
	ctx := context.Background()
	l := newLog(t, newBlob(t), nil)

	last, err := l.Append(ctx, 5, 1, lines("a", "b"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), last)

	// A runner that guesses its own sequence numbers cannot corrupt the anchor.
	last, err = l.Append(ctx, 5, 1, []model.LogLine{{Seq: 999, Text: "c"}})
	require.NoError(t, err)
	assert.Equal(t, int64(3), last)

	got, err := l.Read(ctx, 5, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, line := range got {
		assert.Equal(t, int64(i+1), line.Seq)
		assert.False(t, line.Timestamp.IsZero(), "an unstamped line gets the store's clock")
	}
}

func TestEachAttemptKeepsItsOwnLog(t *testing.T) {
	ctx := context.Background()
	bs := newBlob(t)
	l := newLog(t, bs, nil)

	_, err := l.Append(ctx, 5, 1, lines("attempt one output"))
	require.NoError(t, err)
	require.NoError(t, l.Finalize(ctx, 5, 1))

	_, err = l.Append(ctx, 5, 2, lines("attempt two output"))
	require.NoError(t, err)
	require.NoError(t, l.Finalize(ctx, 5, 2))

	first, err := l.Read(ctx, 5, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "attempt one output", first[0].Text)

	second, err := l.Read(ctx, 5, 2, 0, 10)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "attempt two output", second[0].Text)

	assert.NotEqual(t, l.SealedKey(5, 1), l.SealedKey(5, 2))
}

func TestSealedLogSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	bs := newBlob(t)

	before := newLog(t, bs, nil)
	_, err := before.Append(ctx, 9, 1, lines("built ok", "done"))
	require.NoError(t, err)
	require.NoError(t, before.Finalize(ctx, 9, 1))

	// A fresh Log has no memory at all: everything must come from the blob.
	after := newLog(t, bs, nil)
	got, err := after.Read(ctx, 9, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "built ok", got[0].Text)
	assert.Equal(t, int64(2), got[1].Seq)
}

func TestANSIIsPreserved(t *testing.T) {
	ctx := context.Background()
	bs := newBlob(t)
	l := newLog(t, bs, nil)
	const coloured = "\x1b[31mFAILED\x1b[0m: 3 tests"

	_, err := l.Append(ctx, 1, 1, lines(coloured))
	require.NoError(t, err)

	live, err := l.Read(ctx, 1, 1, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, coloured, live[0].Text)

	require.NoError(t, l.Finalize(ctx, 1, 1))
	sealed, err := l.Read(ctx, 1, 1, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, coloured, sealed[0].Text, "colour must survive sealing; the UI renders it")
}

func TestAppendAfterFinalizeIsRefused(t *testing.T) {
	ctx := context.Background()
	l := newLog(t, newBlob(t), nil)
	_, err := l.Append(ctx, 1, 1, lines("a"))
	require.NoError(t, err)
	require.NoError(t, l.Finalize(ctx, 1, 1))

	// The attempt is evicted from memory on seal, so this append is the
	// post-restart case too: the refusal has to come from the sealed object
	// existing, not from in-process state.
	_, err = l.Append(ctx, 1, 1, lines("sneaky"))
	assert.ErrorIs(t, err, logstore.ErrSealed)

	got, err := l.Read(ctx, 1, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].Text, "a sealed log is never rewritten")
}

func TestAppendFailsLoudlyWhenSealCheckFails(t *testing.T) {
	l := newLog(t, failOnStat{newBlob(t)}, nil)
	_, err := l.Append(context.Background(), 1, 1, lines("a"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage unreachable",
		"an unreadable blob store must not be read as 'not sealed'")
}

type failOnStat struct{ blob.Store }

func (failOnStat) Stat(context.Context, string) (blob.Info, error) {
	return blob.Info{}, errStat{}
}

type errStat struct{}

func (errStat) Error() string { return "storage unreachable" }

func TestAppendToSealedInMemoryAttemptIsRefused(t *testing.T) {
	ctx := context.Background()
	bs := failOnPut{newBlob(t)}
	l := newLog(t, bs, nil)
	_, err := l.Append(ctx, 1, 1, lines("a"))
	require.NoError(t, err)

	// The seal fails, so the attempt stays live and says why.
	err = l.Finalize(ctx, 1, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blob store is on fire")

	_, err = l.Append(ctx, 1, 1, lines("still accepted"))
	require.NoError(t, err, "a failed seal must not silently discard later output")
}

type failOnPut struct{ blob.Store }

func (failOnPut) Put(context.Context, string, io.Reader) (int64, string, error) {
	return 0, "", errBlob
}

var errBlob = errBlobType{}

type errBlobType struct{}

func (errBlobType) Error() string { return "blob store is on fire" }

func TestReadPagination(t *testing.T) {
	ctx := context.Background()
	l := newLog(t, newBlob(t), nil)
	_, err := l.Append(ctx, 1, 1, lines("a", "b", "c", "d"))
	require.NoError(t, err)

	page, err := l.Read(ctx, 1, 1, 0, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "a", page[0].Text)

	page, err = l.Read(ctx, 1, 1, page[1].Seq, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "c", page[0].Text)

	_, err = l.Read(ctx, 1, 1, 0, 0)
	assert.Error(t, err, "a zero limit is a caller bug, not an empty page")
}

func TestReadUnknownAttempt(t *testing.T) {
	ctx := context.Background()
	l := newLog(t, newBlob(t), nil)
	_, err := l.Read(ctx, 404, 1, 0, 10)
	assert.ErrorIs(t, err, logstore.ErrNotFound)

	_, err = l.Read(ctx, 0, 1, 0, 10)
	assert.Error(t, err)
	_, err = l.Read(ctx, 1, 0, 0, 10)
	assert.Error(t, err)
}

func TestSubscribeReplaysThenStreamsLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l := newLog(t, newBlob(t), nil)

	_, err := l.Append(ctx, 3, 1, lines("before-1", "before-2"))
	require.NoError(t, err)

	ch, err := l.Subscribe(ctx, 3, 1, 1)
	require.NoError(t, err)

	first := <-ch
	assert.Equal(t, "before-2", first.Text, "fromSeq skips what the client already has")

	_, err = l.Append(ctx, 3, 1, lines("live-1"))
	require.NoError(t, err)
	assert.Equal(t, "live-1", (<-ch).Text)

	require.NoError(t, l.Finalize(ctx, 3, 1))
	_, open := <-ch
	assert.False(t, open, "finalizing closes live subscribers")
}

func TestSlowSubscriberIsDroppedLoudly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l := newLog(t, newBlob(t), func(o *logstore.Options) { o.SubscriberBuffer = 4 })

	ch, err := l.Subscribe(ctx, 4, 1, 0)
	require.NoError(t, err)

	// Never read while the writer floods far past the ring.
	for i := 0; i < 200; i++ {
		_, err := l.Append(ctx, 4, 1, lines("spam"))
		require.NoError(t, err)
	}

	var last model.LogLine
	var got int
	for line := range ch {
		last = line
		got++
	}
	assert.Less(t, got, 200, "the subscriber must be cut off, not buffered without bound")
	assert.Equal(t, logstore.FellBehindText, last.Text)
	assert.Equal(t, "platform", last.Stream)

	// The job's own log is intact regardless of what the watcher missed.
	all, err := l.Read(ctx, 4, 1, 0, 1000)
	require.NoError(t, err)
	assert.Len(t, all, 200)
}

func TestSubscribeToSealedAttemptReplaysAndCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bs := newBlob(t)
	l := newLog(t, bs, nil)
	_, err := l.Append(ctx, 6, 1, lines("x", "y"))
	require.NoError(t, err)
	require.NoError(t, l.Finalize(ctx, 6, 1))

	ch, err := l.Subscribe(ctx, 6, 1, 0)
	require.NoError(t, err)
	var texts []string
	for line := range ch {
		texts = append(texts, line.Text)
	}
	assert.Equal(t, []string{"x", "y"}, texts)

	_, err = l.Subscribe(ctx, 0, 1, 0)
	assert.Error(t, err)
}

// TestSubscribeBeforeFirstLine covers the normal UI case: the job page opens
// the moment the job starts, before any output exists.
func TestSubscribeBeforeFirstLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l := newLog(t, newBlob(t), nil)

	ch, err := l.Subscribe(ctx, 12, 1, 0)
	require.NoError(t, err)

	_, err = l.Append(ctx, 12, 1, lines("first output"))
	require.NoError(t, err)
	assert.Equal(t, "first output", (<-ch).Text)
}

func TestSubscribeStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := newLog(t, newBlob(t), nil)
	_, err := l.Append(context.Background(), 8, 1, lines("a"))
	require.NoError(t, err)

	ch, err := l.Subscribe(ctx, 8, 1, 0)
	require.NoError(t, err)
	cancel()
	for range ch { //nolint:revive // draining until close is the assertion
	}
}

func TestRaw(t *testing.T) {
	ctx := context.Background()
	bs := newBlob(t)
	l := newLog(t, bs, nil)
	_, err := l.Append(ctx, 2, 1, lines("line one", "\x1b[32mline two\x1b[0m"))
	require.NoError(t, err)

	rc, err := l.Raw(ctx, 2, 1)
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "line one\n\x1b[32mline two\x1b[0m\n", string(body))

	require.NoError(t, l.Finalize(ctx, 2, 1))
	rc, err = l.Raw(ctx, 2, 1)
	require.NoError(t, err)
	body, err = io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, 2, strings.Count(string(body), "\n"))

	_, err = l.Raw(ctx, 999, 1)
	assert.ErrorIs(t, err, logstore.ErrNotFound)
}

func TestFinalizeIsIdempotentAndValidates(t *testing.T) {
	ctx := context.Background()
	l := newLog(t, newBlob(t), nil)
	_, err := l.Append(ctx, 1, 1, lines("a"))
	require.NoError(t, err)

	require.NoError(t, l.Finalize(ctx, 1, 1))
	require.NoError(t, l.Finalize(ctx, 1, 1), "re-finalizing a sealed attempt is a no-op")

	assert.ErrorIs(t, l.Finalize(ctx, 777, 1), logstore.ErrNotFound)
	assert.Error(t, l.Finalize(ctx, 0, 1))
}

func TestCorruptSealedLogFailsLoudly(t *testing.T) {
	ctx := context.Background()
	bs := newBlob(t)
	l := newLog(t, bs, nil)
	_, _, err := bs.Put(ctx, l.SealedKey(11, 1), strings.NewReader("{not json}\n"))
	require.NoError(t, err)

	_, err = l.Read(ctx, 11, 1, 0, 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt")
}
