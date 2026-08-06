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
	"io"
	"net/http"
	"strconv"
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

func splitKeys(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// handleList is the debug listing the client prints after a miss.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	claims, _ := jobtoken.ClaimsFrom(r.Context())
	key := r.URL.Query().Get("key")

	events, err := s.store.ListCacheEvents(r.Context(), claims.RepoID, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: list cache events: %v", err))
		return
	}
	out := ArtifactCacheList{ArtifactCaches: []ArtifactCacheEntry{}}
	seen := map[string]bool{}
	for _, e := range events {
		if e.Kind != "store" || seen[e.Key] || (key != "" && !strings.HasPrefix(e.Key, key)) {
			continue
		}
		seen[e.Key] = true
		out.ArtifactCaches = append(out.ArtifactCaches, ArtifactCacheEntry{
			CacheKey:     e.Key,
			CreationTime: e.At.UTC().Format(time.RFC3339),
		})
	}
	out.TotalCount = len(out.ArtifactCaches)
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleReserve(w http.ResponseWriter, r *http.Request) {
	claims, _ := jobtoken.ClaimsFrom(r.Context())
	if !claims.CanWriteCache() {
		writeErr(w, http.StatusForbidden, fmt.Sprintf(
			"cachesvc: the job token for job %d carries no cache write scope (cache mode %s)",
			claims.JobID, ModeForScopes(claims)))
		return
	}

	var req ReserveCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cachesvc: reserve body is not JSON: %v", err))
		return
	}
	if req.Key == "" || req.Version == "" {
		writeErr(w, http.StatusBadRequest, "cachesvc: reserve requires both key and version")
		return
	}
	if req.CacheSize != nil && *req.CacheSize > s.maxSize {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"cachesvc: archive of %d bytes exceeds the %d byte limit", *req.CacheSize, s.maxSize))
		return
	}

	entry := &model.CacheEntry{
		RepoID:    claims.RepoID,
		Key:       req.Key,
		Version:   req.Version,
		Ref:       claims.Ref,
		CreatedAt: s.now(),
	}
	if err := s.store.ReserveCache(r.Context(), entry); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, fmt.Sprintf(
				"cachesvc: %q version %q already exists for this repository", req.Key, req.Version))
			return
		}
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: reserve %q: %v", req.Key, err))
		return
	}
	if entry.ID == 0 {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf(
			"cachesvc: the store returned no id for %q, so its archive would have nowhere to go", req.Key))
		return
	}

	upload, err := blob.NewChunkedUpload(s.blob, storageKey(entry.ID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: open upload for %q: %v", req.Key, err))
		return
	}
	s.mu.Lock()
	s.uploads[entry.ID] = upload
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, ReserveCacheResponse{CacheID: entry.ID})
}

// handleUpload takes one Content-Range chunk. The client sends
// "bytes start-end/*", uploading chunks concurrently and out of order.
func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	claims, _ := jobtoken.ClaimsFrom(r.Context())
	if !claims.CanWriteCache() {
		writeErr(w, http.StatusForbidden, fmt.Sprintf(
			"cachesvc: the job token for job %d carries no cache write scope", claims.JobID))
		return
	}
	id, ok := s.entryID(w, r, claims)
	if !ok {
		return
	}
	start, _, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cachesvc: "+err.Error())
		return
	}

	s.mu.Lock()
	upload := s.uploads[id]
	s.mu.Unlock()
	if upload == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf(
			"cachesvc: entry %d has no open upload; it was already committed, or the control plane restarted mid-upload", id))
		return
	}
	defer r.Body.Close()
	if err := upload.WriteRange(r.Context(), start, r.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, "cachesvc: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseContentRange reads "bytes start-end/total"; the client always sends "*"
// for the total.
func parseContentRange(h string) (start, end int64, err error) {
	if h == "" {
		return 0, 0, errors.New("a chunk upload must carry a Content-Range header")
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes ")
	if !ok {
		return 0, 0, fmt.Errorf("Content-Range %q does not start with \"bytes \"", h)
	}
	rng, _, _ := strings.Cut(spec, "/")
	startRaw, endRaw, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, 0, fmt.Errorf("Content-Range %q is not start-end", h)
	}
	if start, err = strconv.ParseInt(strings.TrimSpace(startRaw), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("Content-Range %q has a non-numeric start", h)
	}
	if end, err = strconv.ParseInt(strings.TrimSpace(endRaw), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("Content-Range %q has a non-numeric end", h)
	}
	if end < start {
		return 0, 0, fmt.Errorf("Content-Range %q ends before it starts", h)
	}
	return start, end, nil
}

// entryID resolves and authorises the {id} path value.
func (s *Service) entryID(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cachesvc: %q is not a cache id", r.PathValue("id")))
		return 0, false
	}
	entry, err := s.store.GetCache(r.Context(), id)
	if err != nil || entry == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("cachesvc: no cache entry %d", id))
		return 0, false
	}
	if entry.RepoID != claims.RepoID {
		writeErr(w, http.StatusForbidden, fmt.Sprintf(
			"cachesvc: cache entry %d belongs to another repository", id))
		return 0, false
	}
	return id, true
}

func (s *Service) handleCommit(w http.ResponseWriter, r *http.Request) {
	claims, _ := jobtoken.ClaimsFrom(r.Context())
	if !claims.CanWriteCache() {
		writeErr(w, http.StatusForbidden, fmt.Sprintf(
			"cachesvc: the job token for job %d carries no cache write scope", claims.JobID))
		return
	}
	id, ok := s.entryID(w, r, claims)
	if !ok {
		return
	}
	var req CommitCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cachesvc: commit body is not JSON: %v", err))
		return
	}

	s.mu.Lock()
	upload := s.uploads[id]
	delete(s.uploads, id)
	s.mu.Unlock()
	if upload == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf(
			"cachesvc: entry %d has no open upload to commit", id))
		return
	}

	ctx := r.Context()
	size, _, err := upload.Commit(ctx, req.Size)
	if err != nil {
		// The archive is incomplete, so the entry must not become visible. A
		// half-written cache is worse than no cache: it fails jobs downstream.
		_ = upload.Abort(ctx)
		writeErr(w, http.StatusBadRequest, "cachesvc: "+err.Error())
		return
	}
	if err := s.store.FinalizeCache(ctx, id, size); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: finalize entry %d: %v", id, err))
		return
	}

	entry, err := s.store.GetCache(ctx, id)
	if err != nil || entry == nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("cachesvc: reread entry %d after commit: %v", id, err))
		return
	}
	s.recordCacheEvent(ctx, claims.RepoID, entry.Key, "store", "",
		fmt.Sprintf("stored %d bytes for version %s on ref %s", size, entry.Version, entry.Ref), size)

	if err := s.Evict(ctx, claims.RepoID); err != nil {
		// The entry is stored; the quota is now over. That is worth reporting
		// rather than hiding behind a 204.
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf(
			"cachesvc: entry %d was stored but enforcing the repository quota failed: %v", id, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Evict enforces the per-repository quota, deleting each evicted entry's bytes
// and recording why it went. Silent eviction is what makes a cache look merely
// slow instead of broken.
func (s *Service) Evict(ctx context.Context, repoID int64) error {
	evicted, err := s.store.EvictCaches(ctx, repoID, s.quota, s.now())
	if err != nil {
		return fmt.Errorf("cachesvc: evict for repository %d: %w", repoID, err)
	}
	used, err := s.store.CacheUsage(ctx, repoID)
	if err != nil {
		return fmt.Errorf("cachesvc: read cache usage for repository %d: %w", repoID, err)
	}
	var errs []error
	for _, e := range evicted {
		if err := s.blob.Delete(ctx, storageKey(e.ID)); err != nil && !errors.Is(err, blob.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete entry %d's archive: %w", e.ID, err))
			continue
		}
		s.recordCacheEvent(ctx, repoID, e.Key, "evict", "", fmt.Sprintf(
			"evicted to stay under the %d byte repository quota (now %d bytes used); entry was %d bytes, last read %s",
			s.quota, used, e.SizeBytes, e.LastAccessed.UTC().Format(time.RFC3339)), e.SizeBytes)
	}
	return errors.Join(errs...)
}

// handleDownload streams an archive to a holder of a signed URL.
func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.signer.VerifyURL(r.URL); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, fmt.Sprintf("cachesvc: %q is not a cache id", r.PathValue("id")), http.StatusBadRequest)
		return
	}
	entry, err := s.store.GetCache(r.Context(), id)
	if err != nil || entry == nil {
		http.Error(w, fmt.Sprintf("cachesvc: no cache entry %d", id), http.StatusNotFound)
		return
	}

	// The client ranges over the archive when it downloads concurrently, so
	// http.ServeContent is used for its Range handling.
	rc, err := s.blob.Get(r.Context(), storageKey(id))
	if errors.Is(err, blob.ErrNotFound) {
		http.Error(w, fmt.Sprintf("cachesvc: entry %d has no stored archive", id), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("cachesvc: read entry %d: %v", id, err), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	if entry.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(entry.SizeBytes, 10))
	}
	if rs, ok := rc.(io.ReadSeeker); ok {
		// ServeContent brings Range handling, which the client uses when it
		// downloads an archive with concurrent range requests.
		http.ServeContent(w, r, "cache", entry.CreatedAt, rs)
		return
	}
	_, _ = io.Copy(w, rc)
}

// handleV2NotImplemented answers the v2 Twirp CacheService. The client only
// picks v2 when ACTIONS_CACHE_SERVICE_V2 is set in the job environment, which
// the runner does not do; saying so beats a 404.
func (s *Service) handleV2NotImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": "unimplemented",
		"msg": fmt.Sprintf(
			"cachesvc: the v2 cache service (%s) is not implemented; this platform serves the v1 %s API. "+
				"Unset ACTIONS_CACHE_SERVICE_V2 in the job environment.",
			r.PathValue("method"), PathCaches),
	})
}

func (s *Service) recordCacheEvent(ctx context.Context, repoID int64, key, kind, matchedOn, reason string, size int64) {
	_ = s.store.RecordCacheEvent(ctx, model.CacheEvent{
		RepoID:    repoID,
		Key:       key,
		Kind:      kind,
		MatchedOn: matchedOn,
		Reason:    reason,
		SizeBytes: size,
		At:        s.now(),
	})
}
