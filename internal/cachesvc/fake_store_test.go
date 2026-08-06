package cachesvc_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// fakeStore implements cachesvc.Store: the Caches, Events, and Repos slices of
// store.Store. LookupCache implements the restore-keys rule the real stores
// must also implement, so the service's tests exercise the same semantics.
type fakeStore struct {
	mu      sync.Mutex
	nextID  int64
	entries map[int64]*model.CacheEntry
	events  []model.CacheEvent
	repo    model.Repo

	failReserve   error
	failFinalize  error
	failEvict     error
	failUsage     error
	failRepo      error
	noIDOnReserve bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		entries: map[int64]*model.CacheEntry{},
		repo:    model.Repo{ID: 9, Owner: "wow-look-at-my", Name: "ci-platform", DefaultBranch: "main"},
	}
}

func (f *fakeStore) ReserveCache(_ context.Context, e *model.CacheEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReserve != nil {
		return f.failReserve
	}
	for _, existing := range f.entries {
		if existing.RepoID == e.RepoID && existing.Key == e.Key && existing.Version == e.Version && existing.Finalized {
			return store.ErrConflict
		}
	}
	if f.noIDOnReserve {
		return nil
	}
	f.nextID++
	e.ID = f.nextID
	copied := *e
	f.entries[e.ID] = &copied
	return nil
}

func (f *fakeStore) FinalizeCache(_ context.Context, id, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFinalize != nil {
		return f.failFinalize
	}
	e, ok := f.entries[id]
	if !ok {
		return store.ErrNotFound
	}
	e.SizeBytes, e.Finalized = size, true
	return nil
}

// LookupCache tries the exact key, then each restore key as a prefix, newest
// match first, and reports which key matched.
func (f *fakeStore) LookupCache(_ context.Context, repoID int64, key string, restoreKeys []string, version, _ string) (*model.CacheEntry, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	candidates := func(match func(*model.CacheEntry) bool) *model.CacheEntry {
		var found []*model.CacheEntry
		for _, e := range f.entries {
			if e.RepoID == repoID && e.Version == version && match(e) {
				found = append(found, e)
			}
		}
		sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.After(found[j].CreatedAt) })
		if len(found) == 0 {
			return nil
		}
		copied := *found[0]
		return &copied
	}

	if e := candidates(func(e *model.CacheEntry) bool { return e.Key == key }); e != nil {
		return e, key, nil
	}
	for _, rk := range restoreKeys {
		if e := candidates(func(e *model.CacheEntry) bool { return strings.HasPrefix(e.Key, rk) }); e != nil {
			return e, rk, nil
		}
	}
	return nil, "", store.ErrNotFound
}

func (f *fakeStore) GetCache(_ context.Context, id int64) (*model.CacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	copied := *e
	return &copied, nil
}

func (f *fakeStore) TouchCache(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return store.ErrNotFound
	}
	e.LastAccessed = at
	return nil
}

func (f *fakeStore) RecordCacheEvent(_ context.Context, e model.CacheEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) ListCacheEvents(_ context.Context, repoID int64, limit int) ([]model.CacheEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.CacheEvent
	for _, e := range f.events {
		if e.RepoID == repoID {
			out = append(out, e)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// EvictCaches drops least-recently-used finalized entries until the repository
// fits under the quota.
func (f *fakeStore) EvictCaches(_ context.Context, repoID, quotaBytes int64, _ time.Time) ([]*model.CacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEvict != nil {
		return nil, f.failEvict
	}
	var live []*model.CacheEntry
	var used int64
	for _, e := range f.entries {
		if e.RepoID == repoID && e.Finalized {
			live = append(live, e)
			used += e.SizeBytes
		}
	}
	// LRU, with creation standing in for an entry never read since it was
	// stored. Without the fallback a just-committed entry sorts oldest and
	// evicts itself on the commit that created it.
	lastUse := func(e *model.CacheEntry) time.Time {
		if e.LastAccessed.IsZero() {
			return e.CreatedAt
		}
		return e.LastAccessed
	}
	sort.Slice(live, func(i, j int) bool { return lastUse(live[i]).Before(lastUse(live[j])) })

	var evicted []*model.CacheEntry
	for _, e := range live {
		if used <= quotaBytes {
			break
		}
		used -= e.SizeBytes
		copied := *e
		evicted = append(evicted, &copied)
		delete(f.entries, e.ID)
	}
	return evicted, nil
}

func (f *fakeStore) CacheUsage(_ context.Context, repoID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUsage != nil {
		return 0, f.failUsage
	}
	var total int64
	for _, e := range f.entries {
		if e.RepoID == repoID && e.Finalized {
			total += e.SizeBytes
		}
	}
	return total, nil
}

func (f *fakeStore) RecordEvent(context.Context, store.Event) error { return nil }

func (f *fakeStore) ListEvents(context.Context, int64, int64) ([]store.Event, error) {
	return nil, nil
}

func (f *fakeStore) UpsertRepo(context.Context, *model.Repo) error { return nil }

func (f *fakeStore) GetRepo(_ context.Context, id int64) (*model.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRepo != nil {
		return nil, f.failRepo
	}
	if id != f.repo.ID {
		return nil, store.ErrNotFound
	}
	r := f.repo
	return &r, nil
}

func (f *fakeStore) GetRepoByName(context.Context, string, string) (*model.Repo, error) {
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListRepos(context.Context) ([]*model.Repo, error) { return nil, nil }

// add inserts a finalized entry directly, for lookup tests.
func (f *fakeStore) add(e model.CacheEntry) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	e.ID = f.nextID
	e.Finalized = true
	f.entries[e.ID] = &e
	return e.ID
}

func (f *fakeStore) eventsOfKind(kind string) []model.CacheEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.CacheEvent
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

var errStoreDown = errors.New("cache store unavailable")
