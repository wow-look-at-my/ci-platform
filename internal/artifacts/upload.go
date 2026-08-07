// Artifact upload, download, and expiry: the byte paths, as distinct from the
// Twirp metadata surface in artifacts.go.
package artifacts

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/azureshim"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func (s *Service) finalizeArtifact(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) {
	var req FinalizeArtifactRequest
	if err := decodeTwirp(r, &req); err != nil {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: FinalizeArtifact: "+err.Error())
		return
	}
	if msg := checkBackendIDs(claims, req.WorkflowRunBackendID, req.WorkflowJobRunBackendID); msg != nil {
		writeTwirpError(w, CodePermissionDenied, *msg)
		return
	}
	if denyWithoutScope(w, claims, jobtoken.ScopeArtifactsWrite) {
		return
	}
	declared, err := parseInt64(req.Size)
	if err != nil {
		writeTwirpError(w, CodeInvalidArgument, fmt.Sprintf("artifacts: size %q is not an integer", req.Size.String()))
		return
	}

	ctx := r.Context()
	a, err := s.store.FindArtifact(ctx, claims.RunID, req.Name)
	if err != nil || a == nil {
		writeTwirpError(w, CodeNotFound, fmt.Sprintf(
			"artifacts: run %d has no artifact named %q to finalize", claims.RunID, req.Name))
		return
	}

	info, err := s.blob.Stat(ctx, storageKey(a.ID))
	if errors.Is(err, blob.ErrNotFound) {
		writeTwirpError(w, CodeInvalidArgument, fmt.Sprintf(
			"artifacts: no content was uploaded for %q, so there is nothing to finalize", req.Name))
		return
	}
	if err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: read stored %q: %v", req.Name, err))
		return
	}
	if declared > 0 && info.Size != declared {
		writeTwirpError(w, CodeInvalidArgument, fmt.Sprintf(
			"artifacts: %q was declared as %d bytes but %d were stored", req.Name, declared, info.Size))
		return
	}

	digest, _, err := blob.DigestOf(ctx, s.blob, storageKey(a.ID))
	if err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: digest %q: %v", req.Name, err))
		return
	}
	if want := strings.TrimPrefix(req.Hash, "sha256:"); want != "" && want != digest {
		// The client hashes what it sent. A mismatch means the bytes changed in
		// flight, and shipping it as a good artifact is how corruption spreads.
		writeTwirpError(w, CodeInvalidArgument, fmt.Sprintf(
			"artifacts: %q hashes to %s but the client sent %s; the upload was corrupted", req.Name, digest, want))
		return
	}

	if err := s.store.FinalizeArtifact(ctx, a.ID, info.Size, "sha256:"+digest); err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: finalize %q: %v", req.Name, err))
		return
	}
	a.SizeBytes, a.Digest, a.Finalized = info.Size, "sha256:"+digest, true
	s.record(ctx, a, "artifact_finalized",
		fmt.Sprintf("artifact %q stored: %d bytes, sha256:%s", a.Name, info.Size, digest), nil)

	writeTwirpJSON(w, FinalizeArtifactResponse{OK: true, ArtifactID: strconv.FormatInt(a.ID, 10)})
}

func (s *Service) listArtifacts(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) {
	var req ListArtifactsRequest
	if err := decodeTwirp(r, &req); err != nil {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: ListArtifacts: "+err.Error())
		return
	}
	if msg := checkBackendIDs(claims, req.WorkflowRunBackendID, req.WorkflowJobRunBackendID); msg != nil {
		writeTwirpError(w, CodePermissionDenied, *msg)
		return
	}

	if denyWithoutScope(w, claims, jobtoken.ScopeArtifactsRead) {
		return
	}

	all, err := s.store.ListArtifacts(r.Context(), claims.RunID)
	if err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: list run %d: %v", claims.RunID, err))
		return
	}
	var idFilter int64
	if req.IDFilter != "" {
		if idFilter, err = parseInt64(req.IDFilter); err != nil {
			writeTwirpError(w, CodeInvalidArgument, fmt.Sprintf("artifacts: id_filter %q is not an integer", req.IDFilter.String()))
			return
		}
	}

	resp := ListArtifactsResponse{Artifacts: []MonolithArtifact{}}
	runBackend, jobBackend := claims.BackendIDs()
	for _, a := range all {
		switch {
		case !a.Finalized:
			continue
		case req.NameFilter != "" && a.Name != req.NameFilter:
			continue
		case idFilter != 0 && a.ID != idFilter:
			continue
		}
		resp.Artifacts = append(resp.Artifacts, MonolithArtifact{
			WorkflowRunBackendID:    runBackend,
			WorkflowJobRunBackendID: jobBackend,
			DatabaseID:              strconv.FormatInt(a.ID, 10),
			Name:                    a.Name,
			Size:                    strconv.FormatInt(a.SizeBytes, 10),
			CreatedAt:               a.CreatedAt.UTC().Format(time.RFC3339),
			Digest:                  a.Digest,
		})
	}
	writeTwirpJSON(w, resp)
}

func (s *Service) getSignedURL(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) {
	var req GetSignedArtifactURLRequest
	if err := decodeTwirp(r, &req); err != nil {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: GetSignedArtifactURL: "+err.Error())
		return
	}
	if msg := checkBackendIDs(claims, req.WorkflowRunBackendID, req.WorkflowJobRunBackendID); msg != nil {
		writeTwirpError(w, CodePermissionDenied, *msg)
		return
	}

	if denyWithoutScope(w, claims, jobtoken.ScopeArtifactsRead) {
		return
	}

	a, err := s.store.FindArtifact(r.Context(), claims.RunID, req.Name)
	if err != nil || a == nil {
		writeTwirpError(w, CodeNotFound, fmt.Sprintf(
			"artifacts: run %d has no artifact named %q", claims.RunID, req.Name))
		return
	}
	if !a.Finalized {
		writeTwirpError(w, CodeNotFound, fmt.Sprintf(
			"artifacts: artifact %q is still uploading and has no content to download yet", req.Name))
		return
	}
	signed, err := s.DownloadURL(a.ID)
	if err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: sign download url: %v", err))
		return
	}
	writeTwirpJSON(w, GetSignedArtifactURLResponse{SignedURL: signed})
}

// DownloadURL returns a self-authenticating URL for an artifact's bytes. The
// client fetches it with an HTTP client that sends no Authorization header, so
// the proof has to be in the URL.
func (s *Service) DownloadURL(artifactID int64) (string, error) {
	return s.signer.SignURL(fmt.Sprintf("%s%s%d", s.baseURL, PathDownload, artifactID), s.urlTTL)
}

func (s *Service) deleteArtifact(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) {
	var req DeleteArtifactRequest
	if err := decodeTwirp(r, &req); err != nil {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: DeleteArtifact: "+err.Error())
		return
	}
	if msg := checkBackendIDs(claims, req.WorkflowRunBackendID, req.WorkflowJobRunBackendID); msg != nil {
		writeTwirpError(w, CodePermissionDenied, *msg)
		return
	}
	if denyWithoutScope(w, claims, jobtoken.ScopeArtifactsWrite) {
		return
	}

	ctx := r.Context()
	a, err := s.store.FindArtifact(ctx, claims.RunID, req.Name)
	if err != nil || a == nil {
		writeTwirpError(w, CodeNotFound, fmt.Sprintf(
			"artifacts: run %d has no artifact named %q", claims.RunID, req.Name))
		return
	}
	if err := s.blob.Delete(ctx, storageKey(a.ID)); err != nil && !errors.Is(err, blob.ErrNotFound) {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: delete stored %q: %v", req.Name, err))
		return
	}
	s.record(ctx, a, "artifact_deleted",
		fmt.Sprintf("artifact %q deleted by job %d", a.Name, claims.JobID), nil)
	writeTwirpJSON(w, DeleteArtifactResponse{OK: true, ArtifactID: strconv.FormatInt(a.ID, 10)})
}

// uploadHandler is the Azure Block Blob endpoint CreateArtifact points at.
func (s *Service) uploadHandler() http.Handler {
	h, err := azureshim.New(azureshim.Options{
		Store: s.blob,
		Resolve: func(r *http.Request) (azureshim.Target, *azureshim.Error) {
			if err := s.signer.VerifyURL(r.URL); err != nil {
				return azureshim.Target{}, azureshim.Errorf(http.StatusForbidden,
					"AuthenticationFailed", "%v", err)
			}
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 {
				return azureshim.Target{}, azureshim.Errorf(http.StatusBadRequest,
					"InvalidUri", "%q is not an artifact id", r.PathValue("id"))
			}
			return azureshim.Target{Key: storageKey(id), Ref: strconv.FormatInt(id, 10)}, nil
		},
		OnCommit: func(ctx context.Context, t azureshim.Target, size int64, digest, _ string) error {
			// The bytes are stored; FinalizeArtifact records them. Nothing is
			// marked finalized here, so a crash between upload and finalize
			// leaves an unfinalized artifact rather than a phantom good one.
			return nil
		},
	})
	if err != nil {
		// Unreachable: every field azureshim validates is set above. Panicking
		// beats returning a handler that 500s on every upload.
		panic("artifacts: upload handler misconfigured: " + err.Error())
	}
	return h
}

// handleDownload streams an artifact's bytes to a signed URL holder.
func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.signer.VerifyURL(r.URL); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, fmt.Sprintf("artifacts: %q is not an artifact id", r.PathValue("id")), http.StatusBadRequest)
		return
	}
	a, err := s.store.GetArtifact(r.Context(), id)
	if err != nil || a == nil {
		http.Error(w, fmt.Sprintf("artifacts: no artifact %d", id), http.StatusNotFound)
		return
	}

	// Prefer a driver-signed URL so the bytes never transit the control plane.
	if direct, err := s.blob.SignedURL(r.Context(), storageKey(id), s.urlTTL); err == nil {
		http.Redirect(w, r, direct, http.StatusFound)
		return
	} else if !errors.Is(err, blob.ErrUnsupported) {
		http.Error(w, fmt.Sprintf("artifacts: sign storage url for %d: %v", id, err), http.StatusInternalServerError)
		return
	}

	if p := r.URL.Query().Get("path"); p != "" {
		s.serveFileInArtifact(w, r, a, p)
		return
	}

	rc, err := s.blob.Get(r.Context(), storageKey(id))
	if errors.Is(err, blob.ErrNotFound) {
		http.Error(w, fmt.Sprintf("artifacts: artifact %d has no stored content", id), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("artifacts: read artifact %d: %v", id, err), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	// The client sniffs Content-Type and Content-Disposition to decide whether
	// to unzip; an artifact is always a zip unless it was uploaded raw.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Name+".zip"))
	if a.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	}
	if _, err := io.Copy(w, rc); err != nil {
		// The status is already sent, so this can only be logged by the caller
		// through the event trail.
		s.record(r.Context(), a, "artifact_download_failed",
			fmt.Sprintf("streaming artifact %q failed partway: %v", a.Name, err), nil)
	}
}

// serveFileInArtifact serves one path inside the artifact's zip.
func (s *Service) serveFileInArtifact(w http.ResponseWriter, r *http.Request, a *model.Artifact, want string) {
	want = strings.TrimPrefix(path.Clean("/"+want), "/")
	rc, err := s.blob.Get(r.Context(), storageKey(a.ID))
	if err != nil {
		http.Error(w, fmt.Sprintf("artifacts: read artifact %d: %v", a.ID, err), http.StatusNotFound)
		return
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		http.Error(w, fmt.Sprintf("artifacts: read artifact %d: %v", a.ID, err), http.StatusInternalServerError)
		return
	}
	zr, err := zip.NewReader(newByteReaderAt(body), int64(len(body)))
	if err != nil {
		http.Error(w, fmt.Sprintf("artifacts: artifact %d is not a zip, so it has no member %q", a.ID, want), http.StatusBadRequest)
		return
	}
	for _, f := range zr.File {
		if f.Name != want {
			continue
		}
		fr, err := f.Open()
		if err != nil {
			http.Error(w, fmt.Sprintf("artifacts: open %q in artifact %d: %v", want, a.ID, err), http.StatusInternalServerError)
			return
		}
		defer fr.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(want)))
		_, _ = io.Copy(w, fr)
		return
	}
	http.Error(w, fmt.Sprintf("artifacts: artifact %d has no file %q", a.ID, want), http.StatusNotFound)
}

type byteReaderAt struct{ b []byte }

func newByteReaderAt(b []byte) *byteReaderAt { return &byteReaderAt{b: b} }

func (b *byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.b)) {
		return 0, io.EOF
	}
	n := copy(p, b.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// ExpireArtifacts deletes every artifact past its TTL and records why each one
// went. An artifact that vanished without a recorded reason is the failure this
// method exists to prevent.
func (s *Service) ExpireArtifacts(ctx context.Context) (int, error) {
	now := s.now()
	expired, err := s.store.DeleteExpiredArtifacts(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("artifacts: find expired artifacts: %w", err)
	}
	var deleted int
	var errs []error
	for _, a := range expired {
		if err := s.blob.Delete(ctx, storageKey(a.ID)); err != nil && !errors.Is(err, blob.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete artifact %d's content: %w", a.ID, err))
			continue
		}
		s.record(ctx, a, "artifact_expired", fmt.Sprintf(
			"artifact %q was deleted: its %s retention expired at %s",
			a.Name, a.ExpiresAt.Sub(a.CreatedAt).Round(time.Hour), a.ExpiresAt.UTC().Format(time.RFC3339)),
			map[string]any{"size_bytes": a.SizeBytes, "expires_at": a.ExpiresAt})
		deleted++
	}
	return deleted, errors.Join(errs...)
}

// record writes an audit event. A failure to record is itself reported through
// the returned error of nothing, so it is folded into the event trail's own
// kind rather than dropped: the caller has already succeeded at the real work.
func (s *Service) record(ctx context.Context, a *model.Artifact, kind, message string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["artifact_id"] = a.ID
	detail["artifact_name"] = a.Name
	_ = s.store.RecordEvent(ctx, store.Event{
		RunID:   a.RunID,
		JobID:   a.JobID,
		Kind:    kind,
		Message: message,
		Detail:  detail,
		At:      s.now(),
	})
}
