package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// ArtifactDTO is one uploaded build output.
type ArtifactDTO struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	JobID       int64     `json:"job_id,omitempty"`
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"size_bytes"`
	Digest      string    `json:"digest"`
	Finalized   bool      `json:"finalized"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Expired     bool      `json:"expired"`
	DownloadURL string    `json:"download_url"`
}

// ArtifactListDTO is the artifacts browser's data.
type ArtifactListDTO struct {
	TotalCount int           `json:"total_count"`
	SizeBytes  int64         `json:"size_bytes"`
	Artifacts  []ArtifactDTO `json:"artifacts"`
}

func artifactDTO(a *model.Artifact, now time.Time) ArtifactDTO {
	return ArtifactDTO{
		ID:          a.ID,
		RunID:       a.RunID,
		JobID:       a.JobID,
		Name:        a.Name,
		SizeBytes:   a.SizeBytes,
		Digest:      a.Digest,
		Finalized:   a.Finalized,
		CreatedAt:   a.CreatedAt,
		ExpiresAt:   a.ExpiresAt,
		Expired:     !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now),
		DownloadURL: "/api/v1/artifacts/" + itoa(a.ID) + "/download",
	}
}

func (s *Server) listRunArtifacts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.cfg.Store.GetRun(r.Context(), id); err != nil {
		storeErr(w, "get run", err)
		return
	}
	arts, err := s.cfg.Store.ListArtifacts(r.Context(), id)
	if err != nil {
		storeErr(w, "list artifacts", err)
		return
	}
	now := s.now()
	out := ArtifactListDTO{TotalCount: len(arts), Artifacts: make([]ArtifactDTO, 0, len(arts))}
	for _, a := range arts {
		out.SizeBytes += a.SizeBytes
		out.Artifacts = append(out.Artifacts, artifactDTO(a, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	a, err := s.cfg.Store.GetArtifact(r.Context(), id)
	if err != nil {
		storeErr(w, "get artifact", err)
		return
	}
	if !a.Finalized {
		writeErr(w, http.StatusConflict, "not_finalized",
			"artifact %d (%q) is still uploading; there is nothing complete to download yet", a.ID, a.Name)
		return
	}
	if s.cfg.Blobs == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_blob_store",
			"no blob store is wired into this API instance, so artifact bytes cannot be served")
		return
	}
	rc, err := s.cfg.Blobs.Open(r.Context(), a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "blob_error", "open artifact %d (%q): %v", a.ID, a.Name, err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Name))
	if a.SizeBytes > 0 {
		w.Header().Set("Content-Length", itoa(a.SizeBytes))
	}
	if a.Digest != "" {
		w.Header().Set("X-Artifact-Digest", a.Digest)
	}
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already sent; the truncated body is the failure signal a
		// client sees, and this line is the one an operator can grep for.
		fmt.Fprintf(w, "\n[artifact %d truncated: %v]\n", a.ID, err)
	}
}
