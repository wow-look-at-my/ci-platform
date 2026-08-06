package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// RunnerListDTO is the fleet page.
type RunnerListDTO struct {
	TotalCount int         `json:"total_count"`
	Online     int         `json:"online"`
	Busy       int         `json:"busy"`
	Idle       int         `json:"idle"`
	Offline    int         `json:"offline"`
	Capacity   int         `json:"capacity"`
	Runners    []RunnerDTO `json:"runners"`
	At         time.Time   `json:"at"`
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.cfg.Store.ListRunners(r.Context())
	if err != nil {
		storeErr(w, "list runners", err)
		return
	}
	now := s.now()
	out := RunnerListDTO{TotalCount: len(runners), Runners: make([]RunnerDTO, 0, len(runners)), At: now}
	for _, rn := range runners {
		d := runnerDTO(rn, now)
		switch d.State {
		case "busy":
			out.Busy++
			out.Online++
		case "idle":
			out.Idle++
			out.Online++
		case "offline":
			out.Offline++
		}
		out.Capacity += d.Capacity
		out.Runners = append(out.Runners, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// QueueHistoryDTO is the queue-depth chart's data.
type QueueHistoryDTO struct {
	Since   time.Time           `json:"since"`
	Count   int                 `json:"count"`
	Samples []store.QueueSample `json:"samples"`
}

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request) {
	stats, err := s.cfg.Store.QueueStats(r.Context(), s.now())
	if err != nil {
		storeErr(w, "queue stats", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// parseSince accepts either an RFC3339 instant or a Go duration ("30m") meaning
// "that long ago".
func parseSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now.Add(-1 * time.Hour), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return time.Time{}, err
	}
	if d < 0 {
		d = -d
	}
	return now.Add(-d), nil
}

func (s *Server) getQueueHistory(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	since, err := parseSince(r.URL.Query().Get("since"), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"query parameter \"since\" must be an RFC3339 time or a duration like \"30m\": %v", err)
		return
	}
	samples, err := s.cfg.Store.QueueDepthHistory(r.Context(), since)
	if err != nil {
		storeErr(w, "queue depth history", err)
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	writeJSON(w, http.StatusOK, QueueHistoryDTO{Since: since, Count: len(samples), Samples: nonNil(samples)})
}
