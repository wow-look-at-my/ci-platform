// Package cachesvc serves the actions/cache v1 REST API that @actions/cache
// speaks, verified against the toolkit rather than assumed.
//
// Version: getCacheServiceVersion() returns v1 unless ACTIONS_CACHE_SERVICE_V2
// is set in the job environment, and always v1 on GHES. The runner does not set
// that variable, so v1 is what runs. The v2 Twirp CacheService is deliberately
// not implemented; TwirpPrefix answers it with an error that says so rather
// than a 404 nobody can act on.
//
// Download URLs are fetched by an HTTP client with no credential handler
// (downloadCacheHttpClient constructs its own), so archiveLocation carries its
// own signature instead of expecting an Authorization header.
package cachesvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Env var names the runner injects for cache steps.
const (
	EnvCacheURL     = "ACTIONS_CACHE_URL"
	EnvRuntimeToken = "ACTIONS_RUNTIME_TOKEN"
	// EnvCacheMode is the client-side read/write gate: none, read, write, or
	// write-only. The server enforces the same thing from the token's scopes;
	// this variable stops the client wasting a round trip.
	EnvCacheMode = "ACTIONS_CACHE_MODE"
)

// EnvNames is every variable RunnerEnv sets.
var EnvNames = []string{EnvCacheURL, EnvRuntimeToken, EnvCacheMode}

// Cache modes, matching the client's KNOWN_CACHE_MODES.
const (
	ModeNone      = "none"
	ModeRead      = "read"
	ModeWrite     = "write"
	ModeWriteOnly = "write-only"
)

// ModeForScopes derives ACTIONS_CACHE_MODE from a token's scopes, so the
// client's gate and the server's agree by construction.
func ModeForScopes(c *jobtoken.Claims) string {
	switch {
	case c.CanReadCache() && c.CanWriteCache():
		return ModeWrite
	case c.CanReadCache():
		return ModeRead
	case c.CanWriteCache():
		return ModeWriteOnly
	default:
		return ModeNone
	}
}

// RunnerEnv is the environment the runner injects. The base URL keeps its
// trailing slash: the client builds `${baseUrl}_apis/artifactcache/...` by
// concatenation, so dropping it produces a 404 on every cache call.
func RunnerEnv(baseURL, jobToken, mode string) map[string]string {
	return map[string]string{
		EnvCacheURL:     strings.TrimSuffix(baseURL, "/") + "/",
		EnvRuntimeToken: jobToken,
		EnvCacheMode:    mode,
	}
}

// ReadDeniedPrefix is the prefix the client matches to turn a denial into a
// CacheReadDeniedError instead of a generic HTTP failure. It must appear in
// the "message" field of the error body.
const ReadDeniedPrefix = "cache read denied:"

// CacheFileSizeLimit is the client's own 10 GiB per-repository archive cap.
const CacheFileSizeLimit = 10 * 1024 * 1024 * 1024

// Route paths.
const (
	PathLookup   = "/_apis/artifactcache/cache"
	PathCaches   = "/_apis/artifactcache/caches"
	PathDownload = "/_apis/artifactcache/artifacts/"
	// TwirpPrefix is the v2 CacheService route, answered with a clear
	// "not implemented" rather than a 404.
	TwirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"
)

// Store is the persistence this service needs.
type Store interface {
	store.Caches
	store.Events
	store.Repos
}

// Options configures the Service.
type Options struct {
	Store  Store
	Blob   blob.Store
	Signer *jobtoken.Signer
	// BaseURL is this service's public URL; archiveLocation is built from it.
	BaseURL string
	// RepoQuotaBytes caps one repository's cache. Eviction runs on every
	// commit to hold the line.
	RepoQuotaBytes int64
	// MaxEntryBytes rejects an archive too large to be worth storing.
	MaxEntryBytes int64
	// SignedURLTTL bounds a download URL's life.
	SignedURLTTL time.Duration
	Now          func() time.Time
}

// DefaultSignedURLTTL is long enough for a large restore on a slow runner.
const DefaultSignedURLTTL = time.Hour

// Service implements the cache endpoints.
type Service struct {
	store   Store
	blob    blob.Store
	signer  *jobtoken.Signer
	baseURL string
	quota   int64
	maxSize int64
	urlTTL  time.Duration
	now     func() time.Time

	mu      sync.Mutex
	uploads map[int64]*blob.ChunkedUpload
}

// New validates opts.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("cachesvc: a Store is required")
	case opts.Blob == nil:
		return nil, errors.New("cachesvc: a blob store is required")
	case opts.Signer == nil:
		return nil, errors.New("cachesvc: a job-token Signer is required")
	case opts.BaseURL == "":
		return nil, errors.New("cachesvc: BaseURL is required; archiveLocation is built from it")
	case opts.RepoQuotaBytes <= 0:
		return nil, errors.New("cachesvc: RepoQuotaBytes must be positive; a cache with no quota grows until the disk fills")
	}
	s := &Service{
		store: opts.Store, blob: opts.Blob, signer: opts.Signer,
		baseURL: strings.TrimSuffix(opts.BaseURL, "/"),
		quota:   opts.RepoQuotaBytes,
		maxSize: opts.MaxEntryBytes,
		urlTTL:  opts.SignedURLTTL,
		now:     opts.Now,
		uploads: map[int64]*blob.ChunkedUpload{},
	}
	if s.maxSize <= 0 {
		s.maxSize = CacheFileSizeLimit
	}
	if s.urlTTL <= 0 {
		s.urlTTL = DefaultSignedURLTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Handler routes the cache endpoints.
func (s *Service) Handler() http.Handler {
	verifier := s.signer.Verifier()
	mux := http.NewServeMux()
	mux.Handle("GET "+PathLookup, verifier.Middleware(http.HandlerFunc(s.handleLookup)))
	mux.Handle("GET "+PathCaches, verifier.Middleware(http.HandlerFunc(s.handleList)))
	mux.Handle("POST "+PathCaches, verifier.Middleware(http.HandlerFunc(s.handleReserve)))
	mux.Handle("PATCH "+PathCaches+"/{id}", verifier.Middleware(http.HandlerFunc(s.handleUpload)))
	mux.Handle("POST "+PathCaches+"/{id}", verifier.Middleware(http.HandlerFunc(s.handleCommit)))
	mux.HandleFunc("GET "+PathDownload+"{id}", s.handleDownload)
	mux.HandleFunc(TwirpPrefix+"{method}", s.handleV2NotImplemented)
	return mux
}

// storageKey is where a cache entry's archive lives.
func storageKey(id int64) string { return fmt.Sprintf("caches/%d/archive", id) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr renders a failure in the shape @actions/http-client reads: it takes
// the error message from the body's "message" field.
func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// ArtifactCacheEntry is the lookup response the client parses.
type ArtifactCacheEntry struct {
	CacheKey        string `json:"cacheKey"`
	Scope           string `json:"scope"`
	CacheVersion    string `json:"cacheVersion"`
	CreationTime    string `json:"creationTime"`
	ArchiveLocation string `json:"archiveLocation"`
}

// ReserveCacheRequest opens an upload.
type ReserveCacheRequest struct {
	Key       string `json:"key"`
	Version   string `json:"version"`
	CacheSize *int64 `json:"cacheSize"`
}

// ReserveCacheResponse hands back the id later calls use.
type ReserveCacheResponse struct {
	CacheID int64 `json:"cacheId"`
}

// CommitCacheRequest closes an upload with its size.
type CommitCacheRequest struct {
	Size int64 `json:"size"`
}

// ArtifactCacheList is the diagnostic listing the client prints on a miss when
// debug logging is on.
type ArtifactCacheList struct {
	TotalCount     int                  `json:"totalCount"`
	ArtifactCaches []ArtifactCacheEntry `json:"artifactCaches"`
}

// handleLookup implements restore-keys semantics. A miss is 204: the client
// treats any other non-2xx as an error, so a 404 would fail the step rather
// than proceed without a cache.
func (s *Service) handleLookup(w http.ResponseWriter, r *http.Request) {
	claims, _ := jobtoken.ClaimsFrom(r.Context())
	if !claims.CanReadCache() {
		writeErr(w, http.StatusForbidden, fmt.Sprintf(
			"%s the job token for job %d carries no cache read scope (cache mode %s)",
			ReadDeniedPrefix, claims.JobID, ModeForScopes(claims)))
		return
	}

	rawKeys := r.URL.Query().Get("keys")
	version := r.URL.Query().Get("version")
	if rawKeys == "" || version == "" {
		writeErr(w, http.StatusBadRequest, "cachesvc: both keys and version are required")
		return
	}
	keys := splitKeys(rawKeys)
	primary, restore := keys[0], keys[1:]

	ctx := r.Context()
	entry, matchedOn, err := s.store.LookupCache(ctx, claims.RepoID, primary, restore, version, claims.Ref)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: look up %q: %v", primary, err))
		return
	}
	if entry == nil || errors.Is(err, store.ErrNotFound) {
		s.recordCacheEvent(ctx, claims.RepoID, primary, "miss", "", "no entry matched the key or its restore keys", 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Ref scoping is enforced here as well as in the store: a branch may read
	// its own entries and the default branch's, never a sibling branch's.
	allowed, reason, err := s.refAllowed(ctx, claims, entry)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		s.recordCacheEvent(ctx, claims.RepoID, primary, "miss", matchedOn, reason, 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !entry.Finalized {
		s.recordCacheEvent(ctx, claims.RepoID, primary, "miss", matchedOn,
			fmt.Sprintf("entry %q was reserved but never committed", entry.Key), 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	location, err := s.archiveLocation(ctx, entry.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.TouchCache(ctx, entry.ID, s.now()); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: record access of entry %d: %v", entry.ID, err))
		return
	}
	s.recordCacheEvent(ctx, claims.RepoID, primary, "hit", matchedOn,
		fmt.Sprintf("matched %q on ref %s", entry.Key, entry.Ref), entry.SizeBytes)

	writeJSON(w, http.StatusOK, ArtifactCacheEntry{
		CacheKey:        entry.Key,
		Scope:           entry.Ref,
		CacheVersion:    entry.Version,
		CreationTime:    entry.CreatedAt.UTC().Format(time.RFC3339),
		ArchiveLocation: location,
	})
}

// refAllowed implements the ref-scoping rule.
func (s *Service) refAllowed(ctx context.Context, claims *jobtoken.Claims, entry *model.CacheEntry) (bool, string, error) {
	if entry.Ref == "" || entry.Ref == claims.Ref {
		return true, "", nil
	}
	repo, err := s.store.GetRepo(ctx, claims.RepoID)
	if err != nil {
		return false, "", fmt.Errorf("cachesvc: read repository %d to check cache ref scoping: %w", claims.RepoID, err)
	}
	for _, allowed := range []string{repo.DefaultBranch, "refs/heads/" + repo.DefaultBranch} {
		if allowed != "" && entry.Ref == allowed {
			return true, "", nil
		}
	}
	return false, fmt.Sprintf(
		"entry belongs to ref %s, which is neither this job's ref %s nor the default branch %s",
		entry.Ref, claims.Ref, repo.DefaultBranch), nil
}

// archiveLocation builds a URL the client can fetch without credentials.
func (s *Service) archiveLocation(ctx context.Context, id int64) (string, error) {
	if direct, err := s.blob.SignedURL(ctx, storageKey(id), s.urlTTL); err == nil {
		return direct, nil
	} else if !errors.Is(err, blob.ErrUnsupported) {
		return "", fmt.Errorf("cachesvc: sign storage url for entry %d: %w", id, err)
	}
	signed, err := s.signer.SignURL(fmt.Sprintf("%s%s%d", s.baseURL, PathDownload, id), s.urlTTL)
	if err != nil {
		return "", fmt.Errorf("cachesvc: sign download url for entry %d: %w", id, err)
	}
	return signed, nil
}
