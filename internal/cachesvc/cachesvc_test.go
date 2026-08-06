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
