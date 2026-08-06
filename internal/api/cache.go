package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// cacheEventLimit bounds how much of the event log the stats are computed over.
const cacheEventLimit = 5000

// CacheKeyStats is the per-key hit/miss/evict breakdown.
type CacheKeyStats struct {
	Key       string  `json:"key"`
	Hits      int     `json:"hits"`
	Misses    int     `json:"misses"`
	Stores    int     `json:"stores"`
	Evictions int     `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
}

// CacheStats totals the event log.
type CacheStats struct {
	Hits      int     `json:"hits"`
	Misses    int     `json:"misses"`
	Stores    int     `json:"stores"`
	Evictions int     `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
	// EventsConsidered is how many events the rates were computed over, so a
	// rate from three events is not read as a rate from three thousand.
	EventsConsidered int `json:"events_considered"`
}

// CacheEntryDTO is one cache object.
type CacheEntryDTO struct {
	ID           int64     `json:"id"`
	Key          string    `json:"key"`
	Version      string    `json:"version,omitempty"`
	Ref          string    `json:"ref,omitempty"`
	SizeBytes    int64     `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"`
	Finalized    bool      `json:"finalized"`
}

// CacheDTO is the per-repo cache page.
type CacheDTO struct {
	Repository string          `json:"repository"`
	UsageBytes int64           `json:"usage_bytes"`
	TotalCount int             `json:"total_count"`
	Entries    []CacheEntryDTO `json:"entries"`
	// EntriesSource is "store" when the store enumerated live entries, or
	// "cache_events" when they were reconstructed from the event log. The
	// reconstruction is incomplete by construction, and saying so beats
	// rendering a short list as if it were the whole cache.
	EntriesSource   string          `json:"entries_source"`
	EntriesComplete bool            `json:"entries_complete"`
	Warning         string          `json:"warning,omitempty"`
	Stats           CacheStats      `json:"stats"`
	ByKey           []CacheKeyStats `json:"by_key"`
	Events          []model.CacheEvent `json:"events"`
}

func (s *Server) getRepoCache(w http.ResponseWriter, r *http.Request) {
	owner, name := r.PathValue("owner"), r.PathValue("repo")
	if owner == "" || name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "owner and repo path segments are required")
		return
	}
	repo, err := s.cfg.Store.GetRepoByName(r.Context(), owner, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "no such repository %q", owner+"/"+name)
			return
		}
		storeErr(w, "get repo", err)
		return
	}
	events, err := s.cfg.Store.ListCacheEvents(r.Context(), repo.ID, cacheEventLimit)
	if err != nil {
		storeErr(w, "list cache events", err)
		return
	}
	usage, err := s.cfg.Store.CacheUsage(r.Context(), repo.ID)
	if err != nil {
		storeErr(w, "cache usage", err)
		return
	}

	out := CacheDTO{
		Repository: repo.FullName(),
		UsageBytes: usage,
		Events:     nonNil(events),
	}
	out.Stats, out.ByKey = summariseCacheEvents(events)

	if lister, ok := s.cfg.Store.(CacheLister); ok {
		entries, err := lister.ListCacheEntries(r.Context(), repo.ID)
		if err != nil {
			storeErr(w, "list cache entries", err)
			return
		}
		out.EntriesSource, out.EntriesComplete = "store", true
		out.Entries = make([]CacheEntryDTO, 0, len(entries))
		for _, e := range entries {
			out.Entries = append(out.Entries, CacheEntryDTO{
				ID: e.ID, Key: e.Key, Version: e.Version, Ref: e.Ref, SizeBytes: e.SizeBytes,
				CreatedAt: e.CreatedAt, LastAccessed: e.LastAccessed, Finalized: e.Finalized,
			})
		}
	} else {
		out.EntriesSource, out.EntriesComplete = "cache_events", false
		out.Warning = "the store has no cache-entry listing, so these entries were reconstructed from the last " +
			itoa(int64(cacheEventLimit)) + " cache events: entries older than that window are missing"
		out.Entries = entriesFromEvents(events)
	}
	out.TotalCount = len(out.Entries)
	writeJSON(w, http.StatusOK, out)
}

func summariseCacheEvents(events []model.CacheEvent) (CacheStats, []CacheKeyStats) {
	var st CacheStats
	byKey := map[string]*CacheKeyStats{}
	get := func(k string) *CacheKeyStats {
		if s, ok := byKey[k]; ok {
			return s
		}
		s := &CacheKeyStats{Key: k}
		byKey[k] = s
		return s
	}
	for _, e := range events {
		k := get(e.Key)
		switch e.Kind {
		case "hit":
			st.Hits++
			k.Hits++
		case "miss":
			st.Misses++
			k.Misses++
		case "store":
			st.Stores++
			k.Stores++
		case "evict":
			st.Evictions++
			k.Evictions++
		}
	}
	st.EventsConsidered = len(events)
	if n := st.Hits + st.Misses; n > 0 {
		st.HitRate = float64(st.Hits) / float64(n)
	}
	out := make([]CacheKeyStats, 0, len(byKey))
	for _, v := range byKey {
		if n := v.Hits + v.Misses; n > 0 {
			v.HitRate = float64(v.Hits) / float64(n)
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits+out[i].Misses != out[j].Hits+out[j].Misses {
			return out[i].Hits+out[i].Misses > out[j].Hits+out[j].Misses
		}
		return out[i].Key < out[j].Key
	})
	return st, out
}

// entriesFromEvents reconstructs the live set from store/evict events: newest
// store per key wins, an eviction removes it.
func entriesFromEvents(events []model.CacheEvent) []CacheEntryDTO {
	live := map[string]CacheEntryDTO{}
	ordered := append([]model.CacheEvent(nil), events...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	for _, e := range ordered {
		switch e.Kind {
		case "store":
			live[e.Key] = CacheEntryDTO{Key: e.Key, SizeBytes: e.SizeBytes, CreatedAt: e.At, LastAccessed: e.At, Finalized: true}
		case "hit":
			if v, ok := live[e.Key]; ok {
				v.LastAccessed = e.At
				live[e.Key] = v
			}
		case "evict":
			delete(live, e.Key)
		}
	}
	out := make([]CacheEntryDTO, 0, len(live))
	for _, v := range live {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
