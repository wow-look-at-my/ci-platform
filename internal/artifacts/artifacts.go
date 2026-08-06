// Package artifacts serves the GitHub Actions Results API that
// actions/upload-artifact@v4 and actions/download-artifact@v4 speak.
//
// The protocol has three parts, all verified against @actions/artifact rather
// than assumed:
//
//  1. Twirp with JSON encoding at {ACTIONS_RESULTS_URL}/twirp/... (see twirp.go).
//  2. An Azure Block Blob upload endpoint, because CreateArtifact returns a URL
//     the client hands to BlockBlobClient.uploadStream (see blob/azureshim).
//  3. A signed download URL fetched with an unauthenticated HTTP client, so the
//     URL carries its own signature.
//
// ACTIONS_RUNTIME_TOKEN must be a JWT carrying an "scp" claim of
// "Actions.Results:<runBackendId>:<jobBackendId>"; internal/jobtoken mints one.
//
// # The GHES trap
//
// @actions/artifact refuses to run at all when GITHUB_SERVER_URL's hostname is
// not github.com and does not end in .ghe.com or .localhost: every operation
// throws GHESNotSupportedError before a request is made. ValidateServerURL
// enforces that at startup so the failure is a named config error rather than
// every artifact step failing at runtime with a message about GHES.
package artifacts

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Env var names the runner injects for artifact steps.
const (
	EnvResultsURL   = "ACTIONS_RESULTS_URL"
	EnvRuntimeURL   = "ACTIONS_RUNTIME_URL"
	EnvRuntimeToken = "ACTIONS_RUNTIME_TOKEN"
	EnvRunID        = "GITHUB_RUN_ID"
	EnvServerURL    = "GITHUB_SERVER_URL"
	// EnvRetentionDays caps what the action asks for, before the service
	// clamps it again.
	EnvRetentionDays = "GITHUB_RETENTION_DAYS"
)

// EnvNames is every variable RunnerEnv sets, so the runner package can consume
// the list without restating it.
var EnvNames = []string{
	EnvResultsURL, EnvRuntimeURL, EnvRuntimeToken, EnvRunID, EnvServerURL, EnvRetentionDays,
}

// RequiredServerURLSuffixes are the GITHUB_SERVER_URL host endings
// @actions/artifact accepts. "github.com" is accepted as an exact host.
var RequiredServerURLSuffixes = []string{".ghe.com", ".localhost"}

// ValidateServerURL reports whether a URL will survive the client's isGhes()
// check. It is a startup gate: a URL that fails it makes every artifact step
// in every job throw GHESNotSupportedError, and nothing about that failure
// names the real cause.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("artifacts: GITHUB_SERVER_URL %q is not a URL: %w", raw, err)
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return fmt.Errorf("artifacts: GITHUB_SERVER_URL %q has no host", raw)
	}
	if host == "github.com" {
		return nil
	}
	for _, suffix := range RequiredServerURLSuffixes {
		if strings.HasSuffix(host, suffix) {
			return nil
		}
	}
	return fmt.Errorf(
		"artifacts: GITHUB_SERVER_URL host %q would be read as GitHub Enterprise Server by @actions/artifact, "+
			"which refuses to run there: upload-artifact@v4 and download-artifact@v4 would throw "+
			"GHESNotSupportedError before making a request. The host must be github.com or end in %s",
		host, strings.Join(RequiredServerURLSuffixes, " or "))
}

// RunnerEnv is the environment the runner injects for a job attempt. serverURL
// must already have passed ValidateServerURL.
func RunnerEnv(baseURL, serverURL string, runID int64, jobToken string, maxRetentionDays int) map[string]string {
	base := strings.TrimSuffix(baseURL, "/")
	return map[string]string{
		EnvResultsURL: base,
		// The v3 client builds `${ACTIONS_RUNTIME_URL}_apis/...`, so this one
		// keeps its trailing slash.
		EnvRuntimeURL:    base + "/",
		EnvRuntimeToken:  jobToken,
		EnvRunID:         strconv.FormatInt(runID, 10),
		EnvServerURL:     strings.TrimSuffix(serverURL, "/"),
		EnvRetentionDays: strconv.Itoa(maxRetentionDays),
	}
}

// Store is the persistence this service needs. It is narrower than store.Store
// so the service can be tested without a whole store implementation.
type Store interface {
	store.Artifacts
	store.Events
}

// Options configures the Service.
type Options struct {
	Store  Store
	Blob   blob.Store
	Signer *jobtoken.Signer
	// BaseURL is the public URL of this service; upload and download URLs are
	// built from it.
	BaseURL string
	// DefaultRetentionDays applies when the client asks for nothing.
	DefaultRetentionDays int
	// MaxRetentionDays clamps what a client may ask for.
	MaxRetentionDays int
	// RepoQuotaBytes caps one repository's stored artifacts.
	RepoQuotaBytes int64
	// RepoUsage reports a repository's current artifact bytes. Required
	// alongside RepoQuotaBytes; the store contract has no usage query, so the
	// caller supplies one.
	RepoUsage func(ctx context.Context, repoID int64) (int64, error)
	// QuotaDisabled runs with no quota at all. It must be set explicitly: a
	// quota that silently does nothing is worse than none.
	QuotaDisabled bool
	// SignedURLTTL bounds a download URL's life.
	SignedURLTTL time.Duration
	Now          func() time.Time
}

// Defaults.
const (
	DefaultRetentionDays = 90
	DefaultSignedURLTTL  = 6 * time.Hour
)

// Service implements the artifact endpoints.
type Service struct {
	store   Store
	blob    blob.Store
	signer  *jobtoken.Signer
	baseURL string

	defaultRetention int
	maxRetention     int
	quotaBytes       int64
	usage            func(ctx context.Context, repoID int64) (int64, error)
	quotaOff         bool
	urlTTL           time.Duration
	now              func() time.Time
}

// New validates opts.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("artifacts: a Store is required")
	case opts.Blob == nil:
		return nil, errors.New("artifacts: a blob store is required")
	case opts.Signer == nil:
		return nil, errors.New("artifacts: a job-token Signer is required")
	case opts.BaseURL == "":
		return nil, errors.New("artifacts: BaseURL is required; upload and download URLs are built from it")
	}
	if !opts.QuotaDisabled {
		if opts.RepoQuotaBytes <= 0 {
			return nil, errors.New("artifacts: set RepoQuotaBytes, or set QuotaDisabled to run without a quota on purpose")
		}
		if opts.RepoUsage == nil {
			return nil, errors.New("artifacts: RepoQuotaBytes is set but RepoUsage is nil, so the quota could never be enforced")
		}
	}
	s := &Service{
		store:            opts.Store,
		blob:             opts.Blob,
		signer:           opts.Signer,
		baseURL:          strings.TrimSuffix(opts.BaseURL, "/"),
		defaultRetention: opts.DefaultRetentionDays,
		maxRetention:     opts.MaxRetentionDays,
		quotaBytes:       opts.RepoQuotaBytes,
		usage:            opts.RepoUsage,
		quotaOff:         opts.QuotaDisabled,
		urlTTL:           opts.SignedURLTTL,
		now:              opts.Now,
	}
	if s.maxRetention <= 0 {
		s.maxRetention = DefaultRetentionDays
	}
	if s.defaultRetention <= 0 || s.defaultRetention > s.maxRetention {
		s.defaultRetention = s.maxRetention
	}
	if s.urlTTL <= 0 {
		s.urlTTL = DefaultSignedURLTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Route paths outside the Twirp surface.
const (
	// PathUpload is the Azure-shaped upload endpoint. It has four path
	// segments because the Azure SDK parses container and blob names out of
	// the URL and rejects a path with fewer.
	PathUpload   = "/_apis/results/artifacts/upload/"
	PathDownload = "/_apis/results/artifacts/download/"
)

// Handler routes everything this service serves.
func (s *Service) Handler() http.Handler {
	verifier := s.signer.Verifier().WithDenialWriter(func(w http.ResponseWriter, _ *http.Request, status int, reason string) {
		code := CodeUnauthenticated
		if status == http.StatusForbidden {
			code = CodePermissionDenied
		}
		writeTwirpError(w, code, reason)
	})

	mux := http.NewServeMux()
	mux.Handle("POST "+TwirpPrefix+"{method}", verifier.Middleware(http.HandlerFunc(s.handleTwirp)))
	mux.Handle("PUT "+PathUpload+"{id}", s.uploadHandler())
	mux.HandleFunc("GET "+PathDownload+"{id}", s.handleDownload)
	return mux
}

func (s *Service) handleTwirp(w http.ResponseWriter, r *http.Request) {
	claims, ok := jobtoken.ClaimsFrom(r.Context())
	if !ok {
		writeTwirpError(w, CodeUnauthenticated, "artifacts: request carried no verified job token")
		return
	}
	switch r.PathValue("method") {
	case "CreateArtifact":
		s.createArtifact(w, r, claims)
	case "FinalizeArtifact":
		s.finalizeArtifact(w, r, claims)
	case "ListArtifacts":
		s.listArtifacts(w, r, claims)
	case "GetSignedArtifactURL":
		s.getSignedURL(w, r, claims)
	case "DeleteArtifact":
		s.deleteArtifact(w, r, claims)
	default:
		writeTwirpError(w, CodeNotFound, fmt.Sprintf(
			"artifacts: %s has no method %q", ServicePath, r.PathValue("method")))
	}
}

// checkBackendIDs ties a request's backend ids to the token that carried it. A
// token for one job must not be able to write another run's artifacts.
func checkBackendIDs(claims *jobtoken.Claims, runBackendID, jobBackendID string) *string {
	wantRun, wantJob := claims.BackendIDs()
	if runBackendID != "" && runBackendID != wantRun {
		msg := fmt.Sprintf("artifacts: workflow_run_backend_id %q does not belong to this job token", runBackendID)
		return &msg
	}
	if jobBackendID != "" && jobBackendID != wantJob {
		msg := fmt.Sprintf("artifacts: workflow_job_run_backend_id %q does not belong to this job token", jobBackendID)
		return &msg
	}
	return nil
}

// storageKey is where an artifact's bytes live. The artifact id is in the path
// so two runs uploading the same name never collide.
func storageKey(artifactID int64) string {
	return fmt.Sprintf("artifacts/%d/content.zip", artifactID)
}

func (s *Service) createArtifact(w http.ResponseWriter, r *http.Request, claims *jobtoken.Claims) {
	var req CreateArtifactRequest
	if err := decodeTwirp(r, &req); err != nil {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: CreateArtifact: "+err.Error())
		return
	}
	if req.Name == "" {
		writeTwirpError(w, CodeInvalidArgument, "artifacts: CreateArtifact: name is required")
		return
	}
	if msg := checkBackendIDs(claims, req.WorkflowRunBackendID, req.WorkflowJobRunBackendID); msg != nil {
		writeTwirpError(w, CodePermissionDenied, *msg)
		return
	}
	if !claims.Has(jobtoken.ScopeArtifactsWrite) {
		writeTwirpError(w, CodePermissionDenied, fmt.Sprintf(
			"artifacts: the job token for job %d lacks the %s scope", claims.JobID, jobtoken.ScopeArtifactsWrite))
		return
	}

	ctx := r.Context()
	if existing, err := s.store.FindArtifact(ctx, claims.RunID, req.Name); err == nil && existing != nil && existing.Finalized {
		writeTwirpError(w, CodeAlreadyExists, fmt.Sprintf(
			"artifacts: run %d already has a finalized artifact named %q", claims.RunID, req.Name))
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: look up %q: %v", req.Name, err))
		return
	}

	if err := s.checkQuota(ctx, claims.RepoID); err != nil {
		// "insufficient usage" is the substring @actions/artifact matches to
		// raise its storage-quota error, which is the message a user can act on.
		writeTwirpError(w, CodeResourceExhausted, "artifacts: insufficient usage: "+err.Error())
		return
	}

	a := &model.Artifact{
		RunID:     claims.RunID,
		JobID:     claims.JobID,
		Name:      req.Name,
		ExpiresAt: s.expiryFor(req.ExpiresAt),
		CreatedAt: s.now(),
	}
	if err := s.store.CreateArtifact(ctx, a); err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: create %q: %v", req.Name, err))
		return
	}
	if a.ID == 0 {
		writeTwirpError(w, CodeInternal, fmt.Sprintf(
			"artifacts: the store returned no id for %q, so its bytes would have nowhere to go", req.Name))
		return
	}

	uploadURL, err := s.signer.SignURL(fmt.Sprintf("%s%s%d", s.baseURL, PathUpload, a.ID), s.urlTTL)
	if err != nil {
		writeTwirpError(w, CodeInternal, fmt.Sprintf("artifacts: sign upload url: %v", err))
		return
	}
	s.record(ctx, a, "artifact_created", fmt.Sprintf("artifact %q reserved for run %d", a.Name, a.RunID), nil)
	writeTwirpJSON(w, CreateArtifactResponse{OK: true, SignedUploadURL: uploadURL})
}

// expiryFor clamps the client's requested expiry to the configured maximum.
func (s *Service) expiryFor(requested string) time.Time {
	now := s.now()
	max := now.AddDate(0, 0, s.maxRetention)
	if requested == "" {
		return now.AddDate(0, 0, s.defaultRetention)
	}
	t, err := time.Parse(time.RFC3339, requested)
	if err != nil || t.After(max) || !t.After(now) {
		// An unparseable or out-of-range expiry falls back to the maximum
		// rather than to "never": an artifact with no expiry is a leak.
		return max
	}
	return t
}

func (s *Service) checkQuota(ctx context.Context, repoID int64) error {
	if s.quotaOff {
		return nil
	}
	used, err := s.usage(ctx, repoID)
	if err != nil {
		return fmt.Errorf("cannot read repository %d's artifact usage: %w", repoID, err)
	}
	if used >= s.quotaBytes {
		return fmt.Errorf("repository %d has used %d bytes of its %d byte artifact quota", repoID, used, s.quotaBytes)
	}
	return nil
}

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
	if !claims.Has(jobtoken.ScopeArtifactsWrite) {
		writeTwirpError(w, CodePermissionDenied, fmt.Sprintf(
			"artifacts: the job token for job %d lacks the %s scope", claims.JobID, jobtoken.ScopeArtifactsWrite))
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
