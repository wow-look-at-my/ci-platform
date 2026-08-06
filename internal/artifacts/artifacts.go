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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
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

// denyWithoutScope answers PermissionDenied and reports true when the job
// token does not carry the scope a call needs.
//
// Every artifact call goes through this: a scope the minter refuses to issue
// but no handler checks is a permission that exists only in the documentation.
func denyWithoutScope(w http.ResponseWriter, claims *jobtoken.Claims, scope jobtoken.Scope) bool {
	if claims.Has(scope) {
		return false
	}
	writeTwirpError(w, CodePermissionDenied, fmt.Sprintf(
		"artifacts: the job token for job %d lacks the %s scope", claims.JobID, scope))
	return true
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
	if denyWithoutScope(w, claims, jobtoken.ScopeArtifactsWrite) {
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
