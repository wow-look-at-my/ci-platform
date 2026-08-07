package mem

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func (s *Store) ReserveCache(_ context.Context, e *model.CacheEntry) error {
	if e == nil {
		return fmt.Errorf("mem: ReserveCache: nil entry")
	}
	if e.ID != 0 {
		return fmt.Errorf("mem: ReserveCache: id %d already set; the store allocates ids", e.ID)
	}
	if e.Key == "" {
		return fmt.Errorf("mem: ReserveCache: entry for repo %d has no key", e.RepoID)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.LastAccessed.IsZero() {
		e.LastAccessed = e.CreatedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[e.RepoID]; !ok {
		return fmt.Errorf("mem: ReserveCache: repo %d: %w", e.RepoID, store.ErrNotFound)
	}
	s.nextCache++
	e.ID = s.nextCache
	s.caches[e.ID] = cloneCacheEntry(e)
	return nil
}

func (s *Store) FinalizeCache(_ context.Context, id int64, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.caches[id]
	if !ok {
		return store.ErrNotFound
	}
	e.SizeBytes = size
	e.Finalized = true
	return nil
}

func (s *Store) GetCache(_ context.Context, id int64) (*model.CacheEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.caches[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneCacheEntry(e), nil
}

// ListCacheEntries returns a repository's finalized entries, newest first.
func (s *Store) ListCacheEntries(_ context.Context, repoID int64) ([]*model.CacheEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.CacheEntry
	for _, e := range s.caches {
		if e.RepoID == repoID && e.Finalized {
			out = append(out, cloneCacheEntry(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *Store) TouchCache(_ context.Context, id int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.caches[id]
	if !ok {
		return store.ErrNotFound
	}
	e.LastAccessed = at.UTC()
	return nil
}

// newest picks the winner among candidates: newest created_at, ties broken by
// the higher id so the choice is deterministic.
func newest(cands []*model.CacheEntry) *model.CacheEntry {
	var best *model.CacheEntry
	for _, e := range cands {
		switch {
		case best == nil:
			best = e
		case e.CreatedAt.After(best.CreatedAt):
			best = e
		case e.CreatedAt.Equal(best.CreatedAt) && e.ID > best.ID:
			best = e
		}
	}
	return best
}

// LookupCache implements restore-keys semantics: the exact key first, then each
// restore key as a prefix in declaration order, newest created_at winning
// within a key. matchedOn names the key that hit.
//
// ref is recorded on the entry and returned, not filtered on: which refs may
// restore from which is the caller's policy.
func (s *Store) LookupCache(_ context.Context, repoID int64, key string, restoreKeys []string, version, ref string) (*model.CacheEntry, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	eligible := func(e *model.CacheEntry) bool {
		return e.RepoID == repoID && e.Version == version && e.Finalized
	}

	var exact []*model.CacheEntry
	for _, e := range s.caches {
		if eligible(e) && e.Key == key {
			exact = append(exact, e)
		}
	}
	if best := newest(exact); best != nil {
		return cloneCacheEntry(best), key, nil
	}

	for _, rk := range restoreKeys {
		if rk == "" {
			continue
		}
		var cands []*model.CacheEntry
		for _, e := range s.caches {
			if eligible(e) && strings.HasPrefix(e.Key, rk) {
				cands = append(cands, e)
			}
		}
		if best := newest(cands); best != nil {
			return cloneCacheEntry(best), rk, nil
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *Store) RecordCacheEvent(_ context.Context, e model.CacheEvent) error {
	switch e.Kind {
	case "hit", "miss", "store", "evict":
	default:
		return fmt.Errorf("mem: RecordCacheEvent: unknown kind %q", e.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCacheEventLocked(e)
	return nil
}

func (s *Store) appendCacheEventLocked(e model.CacheEvent) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC()
	s.nextCacheEvt++
	e.ID = s.nextCacheEvt
	s.cacheEvts = append(s.cacheEvts, e)
}

func (s *Store) ListCacheEvents(_ context.Context, repoID int64, limit int) ([]model.CacheEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.CacheEvent
	for _, e := range s.cacheEvts {
		if e.RepoID == repoID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// CacheUsage sums the finalized entries. A reserved-but-unfinalized entry has
// no known size, so counting it would be a guess.
func (s *Store) CacheUsage(_ context.Context, repoID int64) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, e := range s.caches {
		if e.RepoID == repoID && e.Finalized {
			total += e.SizeBytes
		}
	}
	return total, nil
}

// EvictCaches drops least-recently-accessed entries until the repo is under
// quota and returns exactly what it removed. Every eviction is also recorded as
// a cache event and logged; silent eviction is forbidden.
func (s *Store) EvictCaches(_ context.Context, repoID int64, quotaBytes int64, now time.Time) ([]*model.CacheEntry, error) {
	if quotaBytes < 0 {
		return nil, fmt.Errorf("mem: EvictCaches: negative quota %d for repo %d", quotaBytes, repoID)
	}
	s.mu.Lock()
	var candidates []*model.CacheEntry
	var total int64
	for _, e := range s.caches {
		if e.RepoID == repoID && e.Finalized {
			candidates = append(candidates, e)
			total += e.SizeBytes
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].LastAccessed.Equal(candidates[j].LastAccessed) {
			return candidates[i].LastAccessed.Before(candidates[j].LastAccessed)
		}
		return candidates[i].ID < candidates[j].ID
	})

	var evicted []*model.CacheEntry
	for _, e := range candidates {
		if total <= quotaBytes {
			break
		}
		total -= e.SizeBytes
		evicted = append(evicted, cloneCacheEntry(e))
		delete(s.caches, e.ID)
		s.appendCacheEventLocked(model.CacheEvent{
			RepoID: repoID,
			Key:    e.Key,
			Kind:   "evict",
			Reason: fmt.Sprintf("evicted to stay under the %d byte cache quota; "+
				"it was the least recently used entry, last read %s",
				quotaBytes, e.LastAccessed.Format(time.RFC3339)),
			SizeBytes: e.SizeBytes,
			At:        now,
		})
	}
	s.mu.Unlock()

	for _, e := range evicted {
		slog.Warn("evicted cache entry",
			"repo_id", repoID, "key", e.Key, "version", e.Version,
			"size_bytes", e.SizeBytes, "last_accessed", e.LastAccessed, "quota_bytes", quotaBytes)
	}
	return evicted, nil
}
