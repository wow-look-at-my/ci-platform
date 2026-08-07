package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func lines(n int) []model.LogLine {
	out := make([]model.LogLine, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, model.LogLine{Seq: int64(i), Timestamp: testNow, StepNumber: 2, Stream: "stdout", Text: "line " + itoa(int64(i))})
	}
	return out
}

func TestLogPaging(t *testing.T) {
	h := newHarness(t)
	h.logs.lines = lines(10)

	w := h.do(t, "GET", "/api/v1/jobs/201/logs?limit=4", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[LogPageDTO](t, w)
	assert.Equal(t, int64(201), got.JobID)
	assert.Equal(t, 2, got.Attempt)
	require.Len(t, got.Lines, 4)
	assert.Equal(t, int64(1), got.Lines[0].Seq)
	assert.Equal(t, int64(5), got.NextSeq)

	w = h.do(t, "GET", "/api/v1/jobs/201/logs?from_seq=5&limit=4", "")
	got = decode[LogPageDTO](t, w)
	require.Len(t, got.Lines, 4)
	assert.Equal(t, int64(5), got.Lines[0].Seq)
	assert.Equal(t, int64(9), got.NextSeq)

	// Field names the UI parses.
	raw := decode[map[string]any](t, w)
	first := raw["lines"].([]any)[0].(map[string]any)
	for _, k := range []string{"seq", "ts", "step", "stream", "text"} {
		assert.Contains(t, first, k)
	}
}

func TestLogPagingRejectsBadParams(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"?from_seq=-1", "?limit=0", "?limit=999999", "?from_seq=x"} {
		w := h.do(t, "GET", "/api/v1/jobs/201/logs"+q, "")
		assert.Equal(t, http.StatusBadRequest, w.Code, q)
	}
}

func TestLogReadErrorIsNotAnEmptyLog(t *testing.T) {
	h := newHarness(t)
	h.logs.err = errors.New("disk on fire")
	w := h.do(t, "GET", "/api/v1/jobs/201/logs", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, decode[errorBody](t, w).Message, "disk on fire")
}

func TestLogsWithoutASourceFailLoud(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Logs = nil })
	for _, target := range []string{"/api/v1/jobs/201/logs", "/api/v1/jobs/201/logs/raw", "/api/v1/jobs/201/logs/stream"} {
		w := h.do(t, "GET", target, "")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, target)
		assert.Equal(t, "no_log_source", decode[errorBody](t, w).Error, target)
	}
}

func TestRawLogDownload(t *testing.T) {
	h := newHarness(t)
	h.logs.lines = lines(3)
	w := h.do(t, "GET", "/api/v1/jobs/201/logs/raw", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "job-201-attempt-2.log")
	assert.Equal(t, "line 1\nline 2\nline 3\n", w.Body.String())
}

func TestLogsUnknownJob(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/jobs/999/logs", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// sseRun drives the stream handler on a real socket so heartbeats and
// disconnects behave as they do in production.
func sseRun(t *testing.T, h *harness, target string, lastEventID string, read func(*bufReader)) {
	t.Helper()
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+target, nil)
	require.NoError(t, err)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	read(newBufReader(t, resp.Body))
}

func TestSSEDeliversLinesThenEOF(t *testing.T) {
	h := newHarness(t)
	h.logs.lines = lines(3)
	sseRun(t, h, "/api/v1/jobs/201/logs/stream", "", func(r *bufReader) {
		body := r.readUntil("event: eof")
		assert.Contains(t, body, "id: 1\nevent: log\n")
		assert.Contains(t, body, `"text":"line 1"`)
		assert.Contains(t, body, "id: 3\nevent: log\n")
	})
}

func TestSSEResumesFromLastEventID(t *testing.T) {
	h := newHarness(t)
	h.logs.lines = lines(5)
	sseRun(t, h, "/api/v1/jobs/201/logs/stream", "3", func(r *bufReader) {
		body := r.readUntil("event: eof")
		assert.NotContains(t, body, `"text":"line 3"`, "a resumed stream must not repeat the last-seen line")
		assert.NotContains(t, body, `"text":"line 1"`)
		assert.Contains(t, body, `"text":"line 4"`)
		assert.Contains(t, body, `"text":"line 5"`)
		assert.Contains(t, body, "from=4", "the stream preamble reports where it resumed")
	})
}

func TestSSERejectsUnparseableLastEventID(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/v1/jobs/201/logs/stream", "")
	require.Equal(t, http.StatusOK, w.Code)

	r := httptest.NewRequest("GET", "/api/v1/jobs/201/logs/stream", nil)
	r.Header.Set("Last-Event-ID", "not-a-number")
	w = httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSSESendsHeartbeatsOnAnIdleStream(t *testing.T) {
	h := newHarness(t)
	// A live channel nobody writes to: the stream stays open with nothing to
	// send, which is exactly when a proxy would close it.
	h.logs.live = make(chan model.LogLine)
	defer close(h.logs.live)

	sseRun(t, h, "/api/v1/jobs/201/logs/stream", "", func(r *bufReader) {
		body := r.readUntil(": heartbeat")
		assert.Contains(t, body, ": heartbeat")
	})
}

func TestSSEStreamsLiveLines(t *testing.T) {
	h := newHarness(t)
	live := make(chan model.LogLine)
	h.logs.live = live
	go func() {
		live <- model.LogLine{Seq: 9, Timestamp: testNow, Stream: "stderr", Text: "later"}
		close(live)
	}()
	sseRun(t, h, "/api/v1/jobs/201/logs/stream", "", func(r *bufReader) {
		body := r.readUntil("event: eof")
		assert.Contains(t, body, "id: 9\n")
		assert.Contains(t, body, `"text":"later"`)
	})
}

func TestSSESubscribeErrorSurfaces(t *testing.T) {
	h := newHarness(t)
	h.logs.err = errors.New("no such attempt")
	w := h.do(t, "GET", "/api/v1/jobs/201/logs/stream", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "log_subscribe_error", decode[errorBody](t, w).Error)
}

// bufReader accumulates a stream until a marker appears, failing the test
// rather than hanging forever.
type bufReader struct {
	t   *testing.T
	src interface{ Read([]byte) (int, error) }
	buf strings.Builder
}

func newBufReader(t *testing.T, src interface{ Read([]byte) (int, error) }) *bufReader {
	return &bufReader{t: t, src: src}
}

func (b *bufReader) readUntil(marker string) string {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type res struct {
		n   int
		err error
	}
	for {
		if strings.Contains(b.buf.String(), marker) {
			return b.buf.String()
		}
		if ctx.Err() != nil {
			b.t.Fatalf("timed out waiting for %q; got:\n%s", marker, b.buf.String())
		}
		p := make([]byte, 4096)
		ch := make(chan res, 1)
		go func() {
			n, err := b.src.Read(p)
			ch <- res{n, err}
		}()
		select {
		case r := <-ch:
			b.buf.Write(p[:r.n])
			if r.err != nil {
				if strings.Contains(b.buf.String(), marker) {
					return b.buf.String()
				}
				b.t.Fatalf("stream ended before %q; got:\n%s", marker, b.buf.String())
			}
		case <-ctx.Done():
			b.t.Fatalf("timed out reading for %q; got:\n%s", marker, b.buf.String())
		}
	}
}
