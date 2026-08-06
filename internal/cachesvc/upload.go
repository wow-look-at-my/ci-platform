// Cache reserve, upload, commit, and download: the byte paths, as distinct
// from lookup and scoping policy in cachesvc.go.
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
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

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
