// Reserve, upload, commit, and download tests.
package cachesvc_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
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
