package cachesvc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/cachesvc"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

const (
	repoID  = int64(9)
	jobRef  = "refs/heads/feature"
	version = "v1-linux-x64-abc123"
)

type harness struct {
	svc   *cachesvc.Service
	store *fakeStore
	blob  *disk.Store
	srv   *httptest.Server
	token string
}

func newHarness(t *testing.T, scopes []jobtoken.Scope, mutate func(*cachesvc.Options)) *harness {
	t.Helper()
	fs := newFakeStore()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	signer, err := jobtoken.New(jobtoken.Options{
		Key:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer: "https://ci.example.ghe.com",
		Lookup: func(int64, int64, int) (jobtoken.Job, error) {
			return jobtoken.Job{
				RepoID: repoID, Repo: "wow-look-at-my/ci-platform", Ref: jobRef,
				Scopes: scopes, ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	})
	require.NoError(t, err)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	opts := cachesvc.Options{
		Store: fs, Blob: bs, Signer: signer, BaseURL: srv.URL,
		RepoQuotaBytes: 1 << 30,
	}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := cachesvc.New(opts)
	require.NoError(t, err)
	mux.Handle("/", svc.Handler())

	token, err := signer.Mint(42, 7, 1)
	require.NoError(t, err)
	return &harness{svc: svc, store: fs, blob: bs, srv: srv, token: token}
}

// do issues a request the way @actions/cache does, against the URL it builds
// by concatenating onto ACTIONS_CACHE_URL.
func (h *harness) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json;api-version=6.0-preview.1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) postJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return h.do(t, http.MethodPost, path, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func lookupPath(keys []string, ver string) string {
	return cachesvc.PathLookup + "?keys=" + url.QueryEscape(strings.Join(keys, ",")) + "&version=" + ver
}

// saveCache replays @actions/cache's saveCache: reserve, PATCH chunks with
// Content-Range, then POST the size.
func (h *harness) saveCache(t *testing.T, key string, body []byte, chunk int) int64 {
	t.Helper()
	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: key, Version: version})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
	require.NotZero(t, id)

	for off := 0; off < len(body); off += chunk {
		end := min(off+chunk, len(body))
		patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
			bytes.NewReader(body[off:end]), map[string]string{
				"Content-Type": "application/octet-stream",
				// The client always sends "*" as the total.
				"Content-Range": fmt.Sprintf("bytes %d-%d/*", off, end-1),
			})
		require.True(t, patch.StatusCode >= 200 && patch.StatusCode < 300,
			"chunk upload returned %d", patch.StatusCode)
	}

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
		cachesvc.CommitCacheRequest{Size: int64(len(body))})
	require.True(t, commit.StatusCode >= 200 && commit.StatusCode < 300,
		"commit returned %d", commit.StatusCode)
	return id
}

func TestSaveAndRestoreCache(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	body := bytes.Repeat([]byte("cache archive "), 500)
	id := h.saveCache(t, "node-modules-abc123", body, 1024)

	resp := h.do(t, http.MethodGet, lookupPath([]string{"node-modules-abc123"}, version), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	entry := decode[cachesvc.ArtifactCacheEntry](t, resp)
	assert.Equal(t, "node-modules-abc123", entry.CacheKey)
	assert.Equal(t, version, entry.CacheVersion)
	assert.Equal(t, jobRef, entry.Scope)
	require.NotEmpty(t, entry.ArchiveLocation)

	// downloadCacheHttpClient sends no Authorization header, so the archive URL
	// must authenticate itself.
	dl, err := http.Get(entry.ArchiveLocation)
	require.NoError(t, err)
	defer dl.Body.Close()
	require.Equal(t, http.StatusOK, dl.StatusCode)
	got, err := io.ReadAll(dl.Body)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	stored, err := h.store.GetCache(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), stored.SizeBytes)
	assert.True(t, stored.Finalized)

	require.Len(t, h.store.eventsOfKind("hit"), 1)
	assert.Equal(t, "node-modules-abc123", h.store.eventsOfKind("hit")[0].MatchedOn)
	require.Len(t, h.store.eventsOfKind("store"), 1)
}

func TestChunksMayArriveOutOfOrder(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
	id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID

	// The client uploads chunks concurrently, so the server cannot assume order.
	for _, c := range []struct {
		off  int
		body string
	}{{6, "ghijkl"}, {0, "abcdef"}, {12, "mn"}} {
		patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
			strings.NewReader(c.body), map[string]string{
				"Content-Range": fmt.Sprintf("bytes %d-%d/*", c.off, c.off+len(c.body)-1),
			})
		require.Equal(t, http.StatusNoContent, patch.StatusCode)
	}

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), cachesvc.CommitCacheRequest{Size: 14})
	require.Equal(t, http.StatusNoContent, commit.StatusCode)

	rc, err := h.blob.Get(context.Background(), fmt.Sprintf("caches/%d/archive", id))
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "abcdefghijklmn", string(got))
}

func TestCommitRejectsAGappyUpload(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "gappy", Version: version})
	id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID

	h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
		strings.NewReader("abc"), map[string]string{"Content-Range": "bytes 0-2/*"})
	h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
		strings.NewReader("xyz"), map[string]string{"Content-Range": "bytes 100-102/*"})

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), cachesvc.CommitCacheRequest{Size: 6})
	require.Equal(t, http.StatusBadRequest, commit.StatusCode)
	assert.Contains(t, decode[map[string]string](t, commit)["message"], "gap")

	// The entry never becomes visible: a half-written archive fails restores.
	lookup := h.do(t, http.MethodGet, lookupPath([]string{"gappy"}, version), nil, nil)
	assert.Equal(t, http.StatusNoContent, lookup.StatusCode)
}

func TestCommitRejectsSizeMismatch(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "short", Version: version})
	id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
	h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
		strings.NewReader("abc"), map[string]string{"Content-Range": "bytes 0-2/*"})

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), cachesvc.CommitCacheRequest{Size: 99})
	require.Equal(t, http.StatusBadRequest, commit.StatusCode)
	assert.Contains(t, decode[map[string]string](t, commit)["message"], "declared as 99")
}

// TestMissIs204 is load-bearing: the client treats any non-2xx other than 204
// as an error, so a 404 on a miss fails the whole step.
func TestMissIs204(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.do(t, http.MethodGet, lookupPath([]string{"nothing-here"}, version), nil, nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	misses := h.store.eventsOfKind("miss")
	require.Len(t, misses, 1)
	assert.Equal(t, "nothing-here", misses[0].Key)
	assert.NotEmpty(t, misses[0].Reason)
}

// TestRestoreKeysSemantics: exact key first, then each prefix in order, newest
// match wins, and the response says which key matched.
func TestRestoreKeysSemantics(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	base := time.Now().Add(-time.Hour)
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "deps-linux-old", Version: version, Ref: jobRef, CreatedAt: base})
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "deps-linux-new", Version: version, Ref: jobRef, CreatedAt: base.Add(time.Minute)})
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "deps-exact", Version: version, Ref: jobRef, CreatedAt: base})

	t.Run("exact key wins over a prefix", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"deps-exact", "deps-linux"}, version), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "deps-exact", decode[cachesvc.ArtifactCacheEntry](t, resp).CacheKey)
	})

	t.Run("newest prefix match wins", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"deps-missing", "deps-linux"}, version), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "deps-linux-new", decode[cachesvc.ArtifactCacheEntry](t, resp).CacheKey)
	})

	t.Run("restore keys are tried in order", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"nope", "deps-exact", "deps-linux"}, version), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "deps-exact", decode[cachesvc.ArtifactCacheEntry](t, resp).CacheKey)
	})

	t.Run("a different version never matches", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"deps-exact"}, "other-version"), nil, nil)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	// The event trail records which key each hit matched on.
	matched := map[string]bool{}
	for _, e := range h.store.eventsOfKind("hit") {
		matched[e.MatchedOn] = true
	}
	assert.True(t, matched["deps-exact"])
	assert.True(t, matched["deps-linux"], "a prefix hit records the restore key that matched, not the entry's key")
}

// TestRefScoping: a branch reads its own entries and the default branch's, and
// never another branch's.
func TestRefScoping(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	now := time.Now()
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "own", Version: version, Ref: jobRef, CreatedAt: now})
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "default", Version: version, Ref: "refs/heads/main", CreatedAt: now})
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "sibling", Version: version, Ref: "refs/heads/other", CreatedAt: now})

	t.Run("own ref", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"own"}, version), nil, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("default branch", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"default"}, version), nil, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("another branch is a miss, with the reason recorded", func(t *testing.T) {
		resp := h.do(t, http.MethodGet, lookupPath([]string{"sibling"}, version), nil, nil)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)

		var found bool
		for _, e := range h.store.eventsOfKind("miss") {
			if e.Key == "sibling" {
				found = true
				assert.Contains(t, e.Reason, "refs/heads/other")
				assert.Contains(t, e.Reason, "neither this job's ref")
			}
		}
		assert.True(t, found, "a cross-ref denial must be recorded, not silent")
	})
}

func TestRefScopingSurfacesRepoLookupFailure(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	h.store.add(model.CacheEntry{RepoID: repoID, Key: "sibling", Version: version, Ref: "refs/heads/other", CreatedAt: time.Now()})
	h.store.failRepo = errStoreDown

	resp := h.do(t, http.MethodGet, lookupPath([]string{"sibling"}, version), nil, nil)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, decode[map[string]string](t, resp)["message"], "cache store unavailable")
}

func TestUnfinalizedEntryIsAMiss(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "reserved", Version: version})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	lookup := h.do(t, http.MethodGet, lookupPath([]string{"reserved"}, version), nil, nil)
	require.Equal(t, http.StatusNoContent, lookup.StatusCode)

	var found bool
	for _, e := range h.store.eventsOfKind("miss") {
		if strings.Contains(e.Reason, "never committed") {
			found = true
		}
	}
	assert.True(t, found)
}

// TestEvictionIsNeverSilent covers the eviction contract: key, size, and why.
func TestEvictionIsNeverSilent(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, func(o *cachesvc.Options) { o.RepoQuotaBytes = 100 })

	old := h.store.add(model.CacheEntry{
		RepoID: repoID, Key: "stale", Version: version, Ref: jobRef,
		SizeBytes: 90, CreatedAt: time.Now().Add(-time.Hour), LastAccessed: time.Now().Add(-time.Hour),
	})
	_, _, err := h.blob.Put(context.Background(), fmt.Sprintf("caches/%d/archive", old), strings.NewReader("x"))
	require.NoError(t, err)

	h.saveCache(t, "fresh", bytes.Repeat([]byte("y"), 50), 1024)

	evictions := h.store.eventsOfKind("evict")
	require.Len(t, evictions, 1)
	assert.Equal(t, "stale", evictions[0].Key)
	assert.Equal(t, int64(90), evictions[0].SizeBytes)
	assert.Contains(t, evictions[0].Reason, "100 byte repository quota")
	assert.Contains(t, evictions[0].Reason, "last read")

	_, err = h.blob.Stat(context.Background(), fmt.Sprintf("caches/%d/archive", old))
	assert.ErrorIs(t, err, blob.ErrNotFound, "an evicted entry's bytes go too")
}

func TestEvictionFailureIsReported(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	h.store.failEvict = errStoreDown

	resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
	id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
	h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
		strings.NewReader("abc"), map[string]string{"Content-Range": "bytes 0-2/*"})

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), cachesvc.CommitCacheRequest{Size: 3})
	require.Equal(t, http.StatusInternalServerError, commit.StatusCode)
	assert.Contains(t, decode[map[string]string](t, commit)["message"], "enforcing the repository quota failed")
}

// TestCacheModeIsEnforcedServerSide: the client gate is advisory, the token is not.
func TestCacheModeIsEnforcedServerSide(t *testing.T) {
	t.Run("read-only token cannot write", func(t *testing.T) {
		h := newHarness(t, []jobtoken.Scope{jobtoken.ScopeCacheRead}, nil)
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["message"], "no cache write scope")
	})

	t.Run("write-only token cannot read, with the prefix the client matches", func(t *testing.T) {
		h := newHarness(t, []jobtoken.Scope{jobtoken.ScopeCacheWrite}, nil)
		resp := h.do(t, http.MethodGet, lookupPath([]string{"k"}, version), nil, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		msg := decode[map[string]string](t, resp)["message"]
		assert.Contains(t, msg, cachesvc.ReadDeniedPrefix,
			"the client turns this prefix into a CacheReadDeniedError instead of a generic failure")
		assert.Contains(t, msg, "write-only")
	})

	t.Run("no cache scope at all", func(t *testing.T) {
		h := newHarness(t, []jobtoken.Scope{jobtoken.ScopeLogsWrite}, nil)
		assert.Equal(t, http.StatusForbidden,
			h.do(t, http.MethodGet, lookupPath([]string{"k"}, version), nil, nil).StatusCode)
		assert.Equal(t, http.StatusForbidden,
			h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version}).StatusCode)
	})
}

func TestModeForScopes(t *testing.T) {
	mode := func(scopes ...jobtoken.Scope) string {
		return cachesvc.ModeForScopes(&jobtoken.Claims{Scopes: scopes})
	}
	assert.Equal(t, cachesvc.ModeWrite, mode(jobtoken.ScopeCacheRW))
	assert.Equal(t, cachesvc.ModeWrite, mode(jobtoken.ScopeCacheRead, jobtoken.ScopeCacheWrite))
	assert.Equal(t, cachesvc.ModeRead, mode(jobtoken.ScopeCacheRead))
	assert.Equal(t, cachesvc.ModeWriteOnly, mode(jobtoken.ScopeCacheWrite))
	assert.Equal(t, cachesvc.ModeNone, mode(jobtoken.ScopeLogsWrite))
}

func TestCrossRepoAccessIsDenied(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	other := h.store.add(model.CacheEntry{RepoID: 999, Key: "theirs", Version: version, Ref: jobRef, CreatedAt: time.Now()})

	patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, other),
		strings.NewReader("x"), map[string]string{"Content-Range": "bytes 0-0/*"})
	assert.Equal(t, http.StatusForbidden, patch.StatusCode)

	commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, other), cachesvc.CommitCacheRequest{Size: 1})
	assert.Equal(t, http.StatusForbidden, commit.StatusCode)

	// A lookup is scoped to the token's repository, so another repo's entry is
	// simply not there.
	lookup := h.do(t, http.MethodGet, lookupPath([]string{"theirs"}, version), nil, nil)
	assert.Equal(t, http.StatusNoContent, lookup.StatusCode)
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+lookupPath([]string{"k"}, version), nil)
	require.NoError(t, err)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.NotEmpty(t, decode[map[string]string](t, resp)["message"])
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)

	t.Run("lookup without version", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest,
			h.do(t, http.MethodGet, cachesvc.PathLookup+"?keys=k", nil, nil).StatusCode)
	})
	t.Run("lookup without keys", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest,
			h.do(t, http.MethodGet, cachesvc.PathLookup+"?version=v", nil, nil).StatusCode)
	})
	t.Run("reserve without a key", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest,
			h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Version: version}).StatusCode)
	})
	t.Run("reserve with a malformed body", func(t *testing.T) {
		resp := h.do(t, http.MethodPost, cachesvc.PathCaches, strings.NewReader("not json"), nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("archive over the size limit", func(t *testing.T) {
		size := int64(cachesvc.CacheFileSizeLimit) + 1
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "huge", Version: version, CacheSize: &size})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["message"], "exceeds the")
	})
	t.Run("chunk with no Content-Range", func(t *testing.T) {
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k1", Version: version})
		id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
		patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), strings.NewReader("x"), nil)
		require.Equal(t, http.StatusBadRequest, patch.StatusCode)
		assert.Contains(t, decode[map[string]string](t, patch)["message"], "Content-Range")
	})
	t.Run("malformed Content-Range", func(t *testing.T) {
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k2", Version: version})
		id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
		for _, bad := range []string{"0-10/*", "bytes abc-10/*", "bytes 0-abc/*", "bytes 10-0/*", "bytes 0/*"} {
			patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
				strings.NewReader("x"), map[string]string{"Content-Range": bad})
			assert.Equal(t, http.StatusBadRequest, patch.StatusCode, "Content-Range %q must be rejected", bad)
		}
	})
	t.Run("unknown cache id", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound,
			h.postJSON(t, cachesvc.PathCaches+"/4040", cachesvc.CommitCacheRequest{Size: 1}).StatusCode)
		assert.Equal(t, http.StatusBadRequest,
			h.postJSON(t, cachesvc.PathCaches+"/not-a-number", cachesvc.CommitCacheRequest{Size: 1}).StatusCode)
	})
	t.Run("chunk for an entry with no open upload", func(t *testing.T) {
		id := h.store.add(model.CacheEntry{RepoID: repoID, Key: "orphan", Version: version, Ref: jobRef, CreatedAt: time.Now()})
		patch := h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
			strings.NewReader("x"), map[string]string{"Content-Range": "bytes 0-0/*"})
		require.Equal(t, http.StatusNotFound, patch.StatusCode)
		assert.Contains(t, decode[map[string]string](t, patch)["message"], "restarted mid-upload")
	})
}

func TestStoreFailuresAreReported(t *testing.T) {
	t.Run("reserve fails", func(t *testing.T) {
		h := newHarness(t, jobtoken.DefaultScopes, nil)
		h.store.failReserve = errStoreDown
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["message"], "cache store unavailable")
	})
	t.Run("reserve conflict", func(t *testing.T) {
		h := newHarness(t, jobtoken.DefaultScopes, nil)
		h.store.add(model.CacheEntry{RepoID: repoID, Key: "taken", Version: version, Ref: jobRef, CreatedAt: time.Now()})
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "taken", Version: version})
		assert.Equal(t, http.StatusConflict, resp.StatusCode)
	})
	t.Run("store returns no id", func(t *testing.T) {
		h := newHarness(t, jobtoken.DefaultScopes, nil)
		h.store.noIDOnReserve = true
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["message"], "no id")
	})
	t.Run("finalize fails", func(t *testing.T) {
		h := newHarness(t, jobtoken.DefaultScopes, nil)
		resp := h.postJSON(t, cachesvc.PathCaches, cachesvc.ReserveCacheRequest{Key: "k", Version: version})
		id := decode[cachesvc.ReserveCacheResponse](t, resp).CacheID
		h.do(t, http.MethodPatch, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id),
			strings.NewReader("abc"), map[string]string{"Content-Range": "bytes 0-2/*"})
		h.store.failFinalize = errStoreDown

		commit := h.postJSON(t, fmt.Sprintf("%s/%d", cachesvc.PathCaches, id), cachesvc.CommitCacheRequest{Size: 3})
		assert.Equal(t, http.StatusInternalServerError, commit.StatusCode)
	})
}

func TestDownloadRejectsUnsignedURLs(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	id := h.saveCache(t, "k", []byte("archive"), 1024)

	resp, err := http.Get(fmt.Sprintf("%s%s%d", h.srv.URL, cachesvc.PathDownload, id))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestDownloadUnknownEntry(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	lookup := h.do(t, http.MethodGet, lookupPath([]string{"k"}, version), nil, nil)
	require.Equal(t, http.StatusNoContent, lookup.StatusCode)

	// Sign a URL for an id that does not exist, the way archiveLocation would.
	id := h.store.add(model.CacheEntry{RepoID: repoID, Key: "no-bytes", Version: version, Ref: jobRef, CreatedAt: time.Now()})
	entry := h.do(t, http.MethodGet, lookupPath([]string{"no-bytes"}, version), nil, nil)
	require.Equal(t, http.StatusOK, entry.StatusCode)
	location := decode[cachesvc.ArtifactCacheEntry](t, entry).ArchiveLocation

	resp, err := http.Get(location)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "entry %d has no archive", id)
}

func TestDiagnosticListing(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	h.saveCache(t, "deps-linux-abc", []byte("x"), 1024)

	resp := h.do(t, http.MethodGet, cachesvc.PathCaches+"?key=deps", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := decode[cachesvc.ArtifactCacheList](t, resp)
	assert.Equal(t, 1, list.TotalCount)
	require.Len(t, list.ArtifactCaches, 1)
	assert.Equal(t, "deps-linux-abc", list.ArtifactCaches[0].CacheKey)

	none := h.do(t, http.MethodGet, cachesvc.PathCaches+"?key=nothing", nil, nil)
	assert.Equal(t, 0, decode[cachesvc.ArtifactCacheList](t, none).TotalCount)
}

// TestCacheV2IsExplicitlyNotImplemented: the v2 Twirp service is not built, and
// says so rather than 404ing.
func TestCacheV2IsExplicitlyNotImplemented(t *testing.T) {
	h := newHarness(t, jobtoken.DefaultScopes, nil)
	resp := h.do(t, http.MethodPost, cachesvc.TwirpPrefix+"CreateCacheEntry", strings.NewReader("{}"), nil)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	body := decode[map[string]string](t, resp)
	assert.Equal(t, "unimplemented", body["code"])
	assert.Contains(t, body["msg"], "ACTIONS_CACHE_SERVICE_V2")
}

func TestNewValidatesOptions(t *testing.T) {
	fs := newFakeStore()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	signer, err := jobtoken.New(jobtoken.Options{Key: []byte("0123456789abcdef0123456789abcdef"), Issuer: "https://x.localhost"})
	require.NoError(t, err)

	for name, o := range map[string]cachesvc.Options{
		"no store":   {Blob: bs, Signer: signer, BaseURL: "x", RepoQuotaBytes: 1},
		"no blob":    {Store: fs, Signer: signer, BaseURL: "x", RepoQuotaBytes: 1},
		"no signer":  {Store: fs, Blob: bs, BaseURL: "x", RepoQuotaBytes: 1},
		"no baseurl": {Store: fs, Blob: bs, Signer: signer, RepoQuotaBytes: 1},
		"no quota":   {Store: fs, Blob: bs, Signer: signer, BaseURL: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cachesvc.New(o)
			assert.Error(t, err)
		})
	}
}

func TestRunnerEnv(t *testing.T) {
	env := cachesvc.RunnerEnv("https://ci.example.ghe.com", "tok", cachesvc.ModeWrite)
	assert.Equal(t, "https://ci.example.ghe.com/", env[cachesvc.EnvCacheURL],
		"the client concatenates _apis/artifactcache/ onto this, so the trailing slash is required")
	assert.Equal(t, "tok", env[cachesvc.EnvRuntimeToken])
	assert.Equal(t, "write", env[cachesvc.EnvCacheMode])

	for _, name := range cachesvc.EnvNames {
		assert.Contains(t, env, name)
	}
	assert.Len(t, env, len(cachesvc.EnvNames))

	// The URL the client builds must be the one the service routes.
	assert.Equal(t, "https://ci.example.ghe.com"+cachesvc.PathLookup,
		env[cachesvc.EnvCacheURL]+"_apis/artifactcache/cache")
}
