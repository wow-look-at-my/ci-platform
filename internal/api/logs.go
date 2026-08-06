package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// defaultLogPage is how many lines a paged read returns without a limit.
const defaultLogPage = 2000

// maxLogPage bounds one read so a job with a million lines cannot be pulled in
// one request.
const maxLogPage = 20000

// LogPageDTO is one page of log lines. NextSeq is where the next page starts;
// it equals FromSeq when nothing was returned.
type LogPageDTO struct {
	JobID   int64        `json:"job_id"`
	Attempt int          `json:"attempt"`
	FromSeq int64        `json:"from_seq"`
	NextSeq int64        `json:"next_seq"`
	Count   int          `json:"count"`
	Lines   []LogLineDTO `json:"lines"`
}

// logSource returns the wired log source, or writes a 503 naming the gap.
func (s *Server) logSource(w http.ResponseWriter) (LogSource, bool) {
	if s.cfg.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_log_source",
			"no log source is wired into this API instance, so job logs cannot be served")
		return nil, false
	}
	return s.cfg.Logs, true
}

// jobAttempt resolves the job and which attempt's log was asked for.
func (s *Server) jobAttempt(w http.ResponseWriter, r *http.Request) (*model.Job, int, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, 0, false
	}
	job, err := s.cfg.Store.GetJob(r.Context(), id)
	if err != nil {
		storeErr(w, "get job", err)
		return nil, 0, false
	}
	attempt, err := intParam(r, "attempt", job.Attempt, 1, 1000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return nil, 0, false
	}
	return job, attempt, true
}

func (s *Server) getJobLogs(w http.ResponseWriter, r *http.Request) {
	job, attempt, ok := s.jobAttempt(w, r)
	if !ok {
		return
	}
	src, ok := s.logSource(w)
	if !ok {
		return
	}
	fromSeq, err := int64Param(r, "from_seq", 0, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	limit, err := intParam(r, "limit", defaultLogPage, 1, maxLogPage)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	lines, err := src.Read(r.Context(), job.ID, attempt, fromSeq, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "log_read_error", "read log for job %d attempt %d: %v", job.ID, attempt, err)
		return
	}
	out := LogPageDTO{JobID: job.ID, Attempt: attempt, FromSeq: fromSeq, NextSeq: fromSeq, Count: len(lines), Lines: make([]LogLineDTO, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, logLineDTO(l))
		if l.Seq >= out.NextSeq {
			out.NextSeq = l.Seq + 1
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) rawJobLogs(w http.ResponseWriter, r *http.Request) {
	job, attempt, ok := s.jobAttempt(w, r)
	if !ok {
		return
	}
	src, ok := s.logSource(w)
	if !ok {
		return
	}
	// The whole log is read in pages so one enormous job cannot be materialised
	// in memory in one call.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("job-%d-attempt-%d.log", job.ID, attempt)))
	var from int64
	for {
		lines, err := src.Read(r.Context(), job.ID, attempt, from, maxLogPage)
		if err != nil {
			// Headers are already out, so the only honest signal left is to
			// mark the body as truncated rather than end it silently.
			fmt.Fprintf(w, "\n[log read failed after seq %d: %v]\n", from, err)
			return
		}
		if len(lines) == 0 {
			return
		}
		for _, l := range lines {
			fmt.Fprintln(w, l.Text)
			if l.Seq >= from {
				from = l.Seq + 1
			}
		}
		if len(lines) < maxLogPage {
			return
		}
	}
}

// streamJobLogs is the live tail. It is resumable: a reconnecting browser
// sends Last-Event-ID with the last seq it rendered and the stream restarts at
// the next one. Heartbeat comments keep proxies from closing an idle tail.
func (s *Server) streamJobLogs(w http.ResponseWriter, r *http.Request) {
	job, attempt, ok := s.jobAttempt(w, r)
	if !ok {
		return
	}
	src, ok := s.logSource(w)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no_flush", "the HTTP stack does not support streaming responses")
		return
	}

	fromSeq, err := int64Param(r, "from_seq", 0, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	if last := strings.TrimSpace(r.Header.Get("Last-Event-ID")); last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "Last-Event-ID must be a log sequence number, got %q", last)
			return
		}
		fromSeq = n + 1
	}

	ch, err := src.Subscribe(r.Context(), job.ID, attempt, fromSeq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "log_subscribe_error", "subscribe to job %d attempt %d: %v", job.ID, attempt, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": stream job=%d attempt=%d from=%d\n\n", job.ID, attempt, fromSeq)
	flusher.Flush()

	ticker := time.NewTicker(s.cfg.SSEHeartbeat)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat %d\n\n", s.now().Unix())
			flusher.Flush()
		case line, open := <-ch:
			if !open {
				fmt.Fprint(w, "event: eof\ndata: {\"reason\":\"attempt finished\"}\n\n")
				flusher.Flush()
				return
			}
			buf, err := json.Marshal(logLineDTO(line))
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: {\"message\":\"could not encode line %d\"}\n\n", line.Seq)
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", line.Seq, buf)
			flusher.Flush()
		}
	}
}
