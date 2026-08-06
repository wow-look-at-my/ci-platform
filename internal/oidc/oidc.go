// Package oidc issues GitHub-Actions-compatible ID tokens and publishes the
// JWKS that verifies them.
//
// # Issuer URL
//
// The issuer is the control plane's public base URL as configured in
// Options.Issuer, e.g. https://ci.example.ghe.com. Relying parties are
// configured to trust exactly that string: buildhost's BUILDHOST_OIDC_ISSUERS
// and secret-server's issuer allow-list both match the "iss" claim literally,
// and both discover the keys through:
//
//	{Issuer}/.well-known/openid-configuration
//	{Issuer}/.well-known/jwks.json
//
// The issuer must be reachable from those services and must not change once
// tokens have been issued against it.
//
// # Client contract
//
// @actions/core's getIDToken builds its request as
// `${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=<encoded>`, appending with "&" and
// never "?", so the URL the runner injects must already carry a query string.
// RequestURL builds one that does. The response body is {"value": "<jwt>"}.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
)

// Endpoint paths.
const (
	PathIDToken   = "/_apis/oidc/token"
	PathJWKS      = "/.well-known/jwks.json"
	PathDiscovery = "/.well-known/openid-configuration"
)

// APIVersionParam is the query parameter that makes ACTIONS_ID_TOKEN_REQUEST_URL
// carry a query string, which getIDToken requires before it appends
// "&audience=".
const APIVersionParam = "api-version=2.0"

// RunnerEnvSelfHosted is the runner_environment claim value for this platform.
// Every runner here is self-hosted; claiming "github-hosted" would be a lie a
// relying party might key a trust decision on.
const RunnerEnvSelfHosted = "self-hosted"

// Env var names the runner injects for the ID token endpoint.
const (
	EnvIDTokenRequestURL   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	EnvIDTokenRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// RequestURL builds the value of ACTIONS_ID_TOKEN_REQUEST_URL for a base URL.
func RequestURL(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/") + PathIDToken + "?" + APIVersionParam
}

// RunnerEnv is the environment the runner injects so @actions/core can request
// an ID token. The token is the job token, which must carry oidc:issue.
func RunnerEnv(baseURL, jobToken string) map[string]string {
	return map[string]string{
		EnvIDTokenRequestURL:   RequestURL(baseURL),
		EnvIDTokenRequestToken: jobToken,
	}
}

// RefType distinguishes a branch from a tag in the sub claim.
const (
	RefTypeBranch = "branch"
	RefTypeTag    = "tag"
)

// Subject is everything about a job attempt that the ID token asserts. The
// service does not invent any of it: a field the caller cannot fill is a field
// the token must not claim.
type Subject struct {
	Repository           string
	RepositoryOwner      string
	RepositoryID         int64
	RepositoryOwnerID    int64
	RepositoryVisibility string

	// Ref is the full git ref, e.g. refs/heads/main or refs/tags/v1.
	Ref     string
	RefType string
	SHA     string

	Actor   string
	ActorID int64

	Workflow string
	// WorkflowRef is owner/repo/.github/workflows/ci.yml@refs/heads/main.
	WorkflowRef string
	// JobWorkflowRef is the ref of the workflow file the job is defined in,
	// which differs from WorkflowRef for a reusable workflow call.
	JobWorkflowRef string

	RunID      int64
	RunNumber  int64
	RunAttempt int

	EventName string
	// Environment is set only when the job declares one.
	Environment string
	// HeadRef and BaseRef are set for pull_request events.
	HeadRef string
	BaseRef string

	// IsForkPR denies the token outright. A fork PR's workflow is attacker-
	// controlled, and an ID token is a credential.
	IsForkPR bool
}

// Claims is the issued token's payload, matching GitHub's claim names so a
// relying party's existing subject and claim conditions keep working.
type Claims struct {
	Repository           string `json:"repository"`
	RepositoryOwner      string `json:"repository_owner"`
	RepositoryID         string `json:"repository_id"`
	RepositoryOwnerID    string `json:"repository_owner_id,omitempty"`
	RepositoryVisibility string `json:"repository_visibility"`

	Ref     string `json:"ref"`
	RefType string `json:"ref_type"`
	SHA     string `json:"sha"`

	Actor   string `json:"actor"`
	ActorID string `json:"actor_id,omitempty"`

	Workflow       string `json:"workflow"`
	WorkflowRef    string `json:"workflow_ref"`
	JobWorkflowRef string `json:"job_workflow_ref"`

	RunID      string `json:"run_id"`
	RunNumber  string `json:"run_number"`
	RunAttempt string `json:"run_attempt"`

	EventName   string `json:"event_name"`
	Environment string `json:"environment,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	BaseRef     string `json:"base_ref,omitempty"`

	RunnerEnvironment string `json:"runner_environment"`

	jwt.RegisteredClaims
}

// Lookup resolves the subject of a token request. The OIDC service holds no
// run state of its own.
type Lookup func(ctx context.Context, runID, jobID int64, attempt int) (*Subject, error)

// Options configures the Service.
type Options struct {
	// Issuer is the public base URL; it becomes the iss claim verbatim.
	Issuer   string
	Keyring  *Keyring
	Verifier *jobtoken.Verifier
	Lookup   Lookup
	// TokenTTL bounds an ID token's life. Keep it short: it is a credential.
	TokenTTL time.Duration
	Now      func() time.Time
}

// DefaultTokenTTL matches GitHub's own ID token lifetime closely enough that
// relying parties tuned for it do not need reconfiguring.
const DefaultTokenTTL = 15 * time.Minute

// Service issues ID tokens and serves discovery documents.
type Service struct {
	issuer   string
	keyring  *Keyring
	verifier *jobtoken.Verifier
	lookup   Lookup
	ttl      time.Duration
	now      func() time.Time
}

// New validates opts. Every field it rejects would otherwise produce tokens
// nothing can verify or claims nobody can trust.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Issuer == "":
		return nil, errors.New("oidc: Issuer is required; it is the iss claim relying parties match on")
	case opts.Keyring == nil:
		return nil, errors.New("oidc: a Keyring is required")
	case opts.Verifier == nil:
		return nil, errors.New("oidc: a job-token Verifier is required; the token endpoint is a credential endpoint")
	case opts.Lookup == nil:
		return nil, errors.New("oidc: a Lookup is required; the service will not invent claims")
	}
	if _, err := url.Parse(opts.Issuer); err != nil {
		return nil, fmt.Errorf("oidc: Issuer %q: %w", opts.Issuer, err)
	}
	s := &Service{
		issuer:   strings.TrimSuffix(opts.Issuer, "/"),
		keyring:  opts.Keyring,
		verifier: opts.Verifier,
		lookup:   opts.Lookup,
		ttl:      opts.TokenTTL,
		now:      opts.Now,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTokenTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Handler routes the three endpoints.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+PathIDToken, s.verifier.Middleware(http.HandlerFunc(s.handleToken)))
	mux.HandleFunc("GET "+PathJWKS, s.handleJWKS)
	mux.HandleFunc("GET "+PathDiscovery, s.handleDiscovery)
	return mux
}

// optionalID renders an id, or nothing when it is unknown. Emitting "0" would
// be a claim that matches every job on the platform, which is worse than an
// absent claim a relying party can reject.
func optionalID(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// SubjectFor builds the sub claim using GitHub's grammar. Environment wins
// over event, and a pull_request has no ref form.
func SubjectFor(sub *Subject) string {
	repo := "repo:" + sub.Repository
	switch {
	case sub.Environment != "":
		return repo + ":environment:" + sub.Environment
	case sub.EventName == "pull_request":
		return repo + ":pull_request"
	default:
		return repo + ":ref:" + sub.Ref
	}
}

// Issue mints an ID token for a subject and audience.
func (s *Service) Issue(sub *Subject, audience string) (string, error) {
	if sub == nil {
		return "", errors.New("oidc: no subject to issue a token for")
	}
	if sub.IsForkPR {
		return "", ErrForkPR
	}
	if audience == "" {
		return "", errors.New("oidc: audience is required")
	}
	if err := validateSubject(sub); err != nil {
		return "", err
	}

	now := s.now()
	claims := Claims{
		Repository:           sub.Repository,
		RepositoryOwner:      sub.RepositoryOwner,
		RepositoryID:         strconv.FormatInt(sub.RepositoryID, 10),
		RepositoryOwnerID:    optionalID(sub.RepositoryOwnerID),
		RepositoryVisibility: sub.RepositoryVisibility,
		Ref:                  sub.Ref,
		RefType:              sub.RefType,
		SHA:                  sub.SHA,
		Actor:                sub.Actor,
		ActorID:              optionalID(sub.ActorID),
		Workflow:             sub.Workflow,
		WorkflowRef:          sub.WorkflowRef,
		JobWorkflowRef:       sub.JobWorkflowRef,
		RunID:                strconv.FormatInt(sub.RunID, 10),
		RunNumber:            strconv.FormatInt(sub.RunNumber, 10),
		RunAttempt:           strconv.Itoa(sub.RunAttempt),
		EventName:            sub.EventName,
		Environment:          sub.Environment,
		HeadRef:              sub.HeadRef,
		BaseRef:              sub.BaseRef,
		RunnerEnvironment:    RunnerEnvSelfHosted,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   SubjectFor(sub),
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
			ID:        uuid.NewString(),
		},
	}

	priv, kid := s.keyring.Active()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		return "", fmt.Errorf("oidc: sign id token for %s: %w", claims.Subject, err)
	}
	return signed, nil
}

// validateSubject refuses to mint a token asserting things the caller left
// blank. A relying party keys authorisation on these fields.
func validateSubject(sub *Subject) error {
	missing := []string{}
	for _, f := range []struct{ name, value string }{
		{"Repository", sub.Repository},
		{"RepositoryOwner", sub.RepositoryOwner},
		{"Ref", sub.Ref},
		{"RefType", sub.RefType},
		{"SHA", sub.SHA},
		{"EventName", sub.EventName},
		{"Workflow", sub.Workflow},
		{"WorkflowRef", sub.WorkflowRef},
		{"JobWorkflowRef", sub.JobWorkflowRef},
		{"RepositoryVisibility", sub.RepositoryVisibility},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("oidc: refusing to issue a token with empty %s", strings.Join(missing, ", "))
	}
	if sub.RunID <= 0 || sub.RunAttempt <= 0 {
		return fmt.Errorf("oidc: refusing to issue a token for run %d attempt %d", sub.RunID, sub.RunAttempt)
	}
	return nil
}

// ErrForkPR is returned for a fork pull request, which gets no token at all.
var ErrForkPR = errors.New("oidc: fork pull requests are not issued ID tokens: the workflow is attacker-controlled and an ID token is a credential")

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	claims, ok := jobtoken.ClaimsFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "oidc: request carried no verified job token")
		return
	}
	if !claims.Has(jobtoken.ScopeOIDCIssue) {
		writeError(w, http.StatusForbidden, fmt.Sprintf(
			"oidc: the job token for job %d lacks the %s scope", claims.JobID, jobtoken.ScopeOIDCIssue))
		return
	}
	audience := r.URL.Query().Get("audience")
	if audience == "" {
		writeError(w, http.StatusBadRequest, "oidc: the audience query parameter is required")
		return
	}

	sub, err := s.lookup(r.Context(), claims.RunID, claims.JobID, claims.Attempt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf(
			"oidc: resolve claims for run %d job %d: %v", claims.RunID, claims.JobID, err))
		return
	}
	if sub == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"oidc: no run %d job %d attempt %d", claims.RunID, claims.JobID, claims.Attempt))
		return
	}

	token, err := s.Issue(sub, audience)
	if errors.Is(err, ErrForkPR) {
		writeError(w, http.StatusForbidden, ErrForkPR.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": token, "count": 1})
}

func (s *Service) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "max-age=300")
	writeJSON(w, http.StatusOK, s.keyring.JWKS())
}

func (s *Service) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"jwks_uri":                              s.issuer + PathJWKS,
		"subject_types_supported":               []string{"public", "pairwise"},
		"response_types_supported":              []string{"id_token"},
		"claims_supported":                      claimNames,
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid"},
	})
}

// claimNames is what the discovery document advertises. It matches the Claims
// struct; a claim listed here that is never issued would be a lie a relying
// party could write a condition against.
var claimNames = []string{
	"sub", "aud", "iss", "iat", "nbf", "exp", "jti",
	"repository", "repository_owner", "repository_id", "repository_owner_id",
	"repository_visibility", "ref", "ref_type", "sha", "actor", "actor_id",
	"workflow", "workflow_ref", "job_workflow_ref", "run_id", "run_number",
	"run_attempt", "event_name", "environment", "head_ref", "base_ref",
	"runner_environment",
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}
