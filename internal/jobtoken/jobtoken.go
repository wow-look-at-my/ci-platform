// Package jobtoken mints and verifies the per-job bearer token the runner
// injects as ACTIONS_RUNTIME_TOKEN.
//
// The token is a real JWT because the toolkit reads it as one: @actions/artifact
// base64-decodes ACTIONS_RUNTIME_TOKEN and requires an "scp" claim containing
// "Actions.Results:<workflowRunBackendId>:<workflowJobRunBackendId>", splitting
// on ":" into exactly three parts, before it makes a single request. A token
// without that claim fails the action client-side with a message that names
// nothing useful.
//
// The two backend IDs are UUIDv5 values derived from the run and job IDs, so
// the service can map them back without a lookup table and can prove that a
// request's IDs belong to the token that carried them.
//
// The token grants exactly the scopes it names, never repository write access.
package jobtoken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Scope is one capability a job token may carry.
type Scope string

const (
	ScopeArtifactsWrite Scope = "artifacts:write"
	ScopeArtifactsRead  Scope = "artifacts:read"
	ScopeCacheRW        Scope = "cache:rw"
	ScopeCacheRead      Scope = "cache:read"
	ScopeCacheWrite     Scope = "cache:write"
	ScopeLogsWrite      Scope = "logs:write"
	ScopeOIDCIssue      Scope = "oidc:issue"
)

// Valid reports whether s is a known scope. An unknown scope in a mint request
// is a programming error, not something to drop quietly.
func (s Scope) Valid() bool {
	switch s {
	case ScopeArtifactsWrite, ScopeArtifactsRead, ScopeCacheRW, ScopeCacheRead,
		ScopeCacheWrite, ScopeLogsWrite, ScopeOIDCIssue:
		return true
	}
	return false
}

// DefaultScopes is what an ordinary job attempt gets.
var DefaultScopes = []Scope{
	ScopeArtifactsWrite, ScopeArtifactsRead, ScopeCacheRW, ScopeLogsWrite, ScopeOIDCIssue,
}

// ActionsResultsScopePrefix is the scope entry @actions/artifact looks for.
const ActionsResultsScopePrefix = "Actions.Results"

// backendIDNamespace derives stable UUIDv5 backend IDs. It is a constant so
// the same run always produces the same ID across control-plane restarts.
var backendIDNamespace = uuid.MustParse("6b2f4a1e-6c1f-5a0c-9d3b-2f0a7c4e51d8")

// BackendRunID is the workflowRunBackendId for a run.
func BackendRunID(runID int64) string {
	return uuid.NewSHA1(backendIDNamespace, []byte("run:"+strconv.FormatInt(runID, 10))).String()
}

// BackendJobID is the workflowJobRunBackendId for one job attempt. The attempt
// is part of it: a re-run must not be able to write the previous attempt's
// artifacts.
func BackendJobID(jobID int64, attempt int) string {
	return uuid.NewSHA1(backendIDNamespace,
		[]byte("job:"+strconv.FormatInt(jobID, 10)+":"+strconv.Itoa(attempt))).String()
}

// Claims is the token payload.
type Claims struct {
	RunID   int64  `json:"run_id"`
	JobID   int64  `json:"job_id"`
	Attempt int    `json:"attempt"`
	RepoID  int64  `json:"repo_id"`
	Repo    string `json:"repository"`
	// Ref is the job's git ref, carried so cache ref-scoping needs no lookup.
	Ref    string  `json:"ref,omitempty"`
	Scopes []Scope `json:"scopes"`
	// Scp is the Actions-compatible space-separated scope string. The toolkit
	// reads this and nothing else in this struct.
	Scp string `json:"scp"`

	jwt.RegisteredClaims
}

// Has reports whether the token carries a scope.
func (c *Claims) Has(s Scope) bool {
	for _, got := range c.Scopes {
		if got == s {
			return true
		}
	}
	return false
}

// CanReadCache reports whether the token may look a cache entry up.
func (c *Claims) CanReadCache() bool { return c.Has(ScopeCacheRW) || c.Has(ScopeCacheRead) }

// CanWriteCache reports whether the token may reserve and commit cache entries.
func (c *Claims) CanWriteCache() bool { return c.Has(ScopeCacheRW) || c.Has(ScopeCacheWrite) }

// BackendIDs returns the pair the Results API requests carry.
func (c *Claims) BackendIDs() (run, job string) {
	return BackendRunID(c.RunID), BackendJobID(c.JobID, c.Attempt)
}

// Job is everything needed to mint a token for one attempt.
type Job struct {
	RunID     int64
	JobID     int64
	Attempt   int
	RepoID    int64
	Repo      string
	Ref       string
	Scopes    []Scope
	ExpiresAt time.Time
}

// Lookup resolves the job a Mint call names. The scheduler owns this data; the
// token package refuses to guess it.
type Lookup func(runID, jobID int64, attempt int) (Job, error)

// Options configures a Signer.
type Options struct {
	// Key is the HMAC key; at least 32 bytes.
	Key []byte
	// Issuer is this control plane's URL, recorded as the "iss" claim.
	Issuer string
	// Grace extends a token past the job's deadline so an upload in flight when
	// the job ends still lands.
	Grace  time.Duration
	Lookup Lookup
	Now    func() time.Time
}

// Signer mints and verifies job tokens.
type Signer struct {
	key    []byte
	issuer string
	grace  time.Duration
	lookup Lookup
	now    func() time.Time
}

// MinKeyLen is the shortest HMAC key accepted. A shorter key is a
// configuration error, not something to pad.
const MinKeyLen = 32

// New validates o and returns a Signer.
func New(o Options) (*Signer, error) {
	if len(o.Key) < MinKeyLen {
		return nil, fmt.Errorf("jobtoken: signing key is %d bytes, need at least %d", len(o.Key), MinKeyLen)
	}
	if o.Issuer == "" {
		return nil, errors.New("jobtoken: Issuer is required; it is the iss claim every service checks")
	}
	s := &Signer{key: o.Key, issuer: o.Issuer, grace: o.Grace, lookup: o.Lookup, now: o.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.grace == 0 {
		s.grace = 5 * time.Minute
	}
	return s, nil
}

// MintJob issues a token for an explicitly described job.
func (s *Signer) MintJob(j Job) (string, error) {
	switch {
	case j.RunID <= 0:
		return "", fmt.Errorf("jobtoken: run id %d is not valid", j.RunID)
	case j.JobID <= 0:
		return "", fmt.Errorf("jobtoken: job id %d is not valid", j.JobID)
	case j.Attempt <= 0:
		return "", fmt.Errorf("jobtoken: attempt %d is not valid", j.Attempt)
	case j.Repo == "":
		return "", errors.New("jobtoken: repository is required")
	case len(j.Scopes) == 0:
		return "", errors.New("jobtoken: a token with no scopes can do nothing; name the scopes")
	case j.ExpiresAt.IsZero():
		return "", errors.New("jobtoken: ExpiresAt is required; a token that never expires is not a job token")
	}
	for _, sc := range j.Scopes {
		if !sc.Valid() {
			return "", fmt.Errorf("jobtoken: unknown scope %q", sc)
		}
	}

	now := s.now()
	runBackend := BackendRunID(j.RunID)
	jobBackend := BackendJobID(j.JobID, j.Attempt)
	claims := Claims{
		RunID:   j.RunID,
		JobID:   j.JobID,
		Attempt: j.Attempt,
		RepoID:  j.RepoID,
		Repo:    j.Repo,
		Ref:     j.Ref,
		Scopes:  j.Scopes,
		Scp:     ActionsResultsScopePrefix + ":" + runBackend + ":" + jobBackend,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   fmt.Sprintf("job:%d:%d", j.JobID, j.Attempt),
			Audience:  jwt.ClaimStrings{s.issuer},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(j.ExpiresAt.Add(s.grace)),
			ID:        uuid.NewString(),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("jobtoken: sign token for job %d attempt %d: %w", j.JobID, j.Attempt, err)
	}
	return tok, nil
}

// Mint matches scheduler.Options.MintJobToken. It resolves the job through the
// configured Lookup; without one there is nothing to put in the token, and
// that is a wiring error rather than a token with invented contents.
func (s *Signer) Mint(runID, jobID int64, attempt int) (string, error) {
	if s.lookup == nil {
		return "", errors.New("jobtoken: no Lookup configured, so Mint cannot resolve the job's repository and scopes")
	}
	j, err := s.lookup(runID, jobID, attempt)
	if err != nil {
		return "", fmt.Errorf("jobtoken: resolve job %d attempt %d: %w", jobID, attempt, err)
	}
	j.RunID, j.JobID, j.Attempt = runID, jobID, attempt
	if len(j.Scopes) == 0 {
		j.Scopes = DefaultScopes
	}
	return s.MintJob(j)
}

// ErrUnauthorized is the class of every verification failure. The wrapped
// error always names the specific reason.
var ErrUnauthorized = errors.New("jobtoken: unauthorized")

// Verify parses and checks a token, returning the reason on failure.
func (s *Signer) Verify(token string) (*Claims, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: no token presented", ErrUnauthorized)
	}
	var claims Claims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return s.key, nil
	},
		jwt.WithIssuer(s.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("%w: token failed validation", ErrUnauthorized)
	}
	if claims.RunID <= 0 || claims.JobID <= 0 || claims.Attempt <= 0 {
		return nil, fmt.Errorf("%w: token names run %d job %d attempt %d", ErrUnauthorized, claims.RunID, claims.JobID, claims.Attempt)
	}
	wantScp := ActionsResultsScopePrefix + ":" + BackendRunID(claims.RunID) + ":" + BackendJobID(claims.JobID, claims.Attempt)
	if !hasScopeEntry(claims.Scp, wantScp) {
		return nil, fmt.Errorf("%w: scp claim does not match the run and job it names", ErrUnauthorized)
	}
	return &claims, nil
}

func hasScopeEntry(scp, want string) bool {
	for _, e := range strings.Fields(scp) {
		if e == want {
			return true
		}
	}
	return false
}

// BearerToken pulls the token out of an Authorization header, accepting any
// capitalisation of the scheme because the toolkit's clients differ.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	scheme, rest, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

type ctxKey struct{}

// ClaimsFrom returns the verified claims a Verifier attached to the request.
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

// WithClaims attaches claims to a context. Handlers use it in tests; the
// Verifier uses it in production.
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// DenialWriter renders a rejection in the shape the calling client parses.
type DenialWriter func(w http.ResponseWriter, r *http.Request, status int, reason string)

// DefaultDenialWriter writes {"message": ...}, which is what
// @actions/http-client surfaces as the error message.
func DefaultDenialWriter(w http.ResponseWriter, _ *http.Request, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": reason})
}

// Verifier is the middleware the artifact, cache, log, and OIDC services put
// in front of themselves.
type Verifier struct {
	signer   *Signer
	required []Scope
	deny     DenialWriter
}

// Verifier builds middleware demanding the given scopes.
func (s *Signer) Verifier(required ...Scope) *Verifier {
	return &Verifier{signer: s, required: required, deny: DefaultDenialWriter}
}

// WithDenialWriter changes how rejections are rendered, e.g. to Twirp's
// {code,msg} shape.
func (v *Verifier) WithDenialWriter(d DenialWriter) *Verifier {
	c := *v
	c.deny = d
	return &c
}

// Middleware verifies the bearer token and attaches its claims.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, status, reason := v.check(r)
		if reason != "" {
			v.deny(w, r, status, reason)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
	})
}

// Check verifies a request without wrapping a handler, for services that need
// the claims before routing. It returns an empty reason on success.
func (v *Verifier) Check(r *http.Request) (claims *Claims, status int, reason string) {
	return v.check(r)
}

func (v *Verifier) check(r *http.Request) (*Claims, int, string) {
	claims, err := v.signer.Verify(BearerToken(r))
	if err != nil {
		return nil, http.StatusUnauthorized, err.Error()
	}
	for _, need := range v.required {
		if !claims.Has(need) {
			return nil, http.StatusForbidden, fmt.Sprintf(
				"jobtoken: token for job %d lacks the %q scope", claims.JobID, need)
		}
	}
	return claims, 0, ""
}

// urlSigningKey is domain-separated from the token key so a signed URL can
// never be replayed as a bearer token.
func (s *Signer) urlSigningKey() []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("ci-platform/url-signing/v1"))
	return m.Sum(nil)
}

// Query parameters a signed URL carries.
const (
	ParamExpires   = "exp"
	ParamSignature = "sig"
	// ParamSignedParams names the query parameters the signature covers.
	//
	// The signature deliberately does not cover the whole query: the Azure
	// Storage SDK appends comp=block, blockid=, and comp=blocklist to the
	// upload URL after it is handed over, and covering those would make every
	// block upload fail its own signature. Only the path, the expiry, and the
	// parameters present at signing time are covered, which is how a real SAS
	// behaves. Anything a caller must not be able to change belongs in the
	// path or in a parameter set before signing.
	ParamSignedParams = "sp"
)

// SignURL appends an expiry and signature to a URL, for handing to a client
// that cannot send an Authorization header. actions/cache downloads its
// archive with an unauthenticated HTTP client, and @actions/artifact fetches
// its signed download URL the same way, so those URLs must carry their own
// proof.
func (s *Signer) SignURL(raw string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("jobtoken: signed URL ttl must be positive, got %s", ttl)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("jobtoken: sign url %q: %w", raw, err)
	}
	q := u.Query()
	q.Del(ParamSignature)
	q.Del(ParamSignedParams)
	q.Set(ParamExpires, strconv.FormatInt(s.now().Add(ttl).Unix(), 10))

	names := make([]string, 0, len(q))
	for k := range q {
		if k != ParamExpires {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		q.Set(ParamSignedParams, strings.Join(names, ","))
	}
	q.Set(ParamSignature, s.urlSignature(u.Path, q))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// VerifyURL checks a signature produced by SignURL.
func (s *Signer) VerifyURL(u *url.URL) error {
	q := u.Query()
	sig := q.Get(ParamSignature)
	if sig == "" {
		return fmt.Errorf("%w: url carries no signature", ErrUnauthorized)
	}
	expRaw := q.Get(ParamExpires)
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: url expiry %q is not a timestamp", ErrUnauthorized, expRaw)
	}
	if s.now().After(time.Unix(exp, 0)) {
		return fmt.Errorf("%w: url signature expired at %s", ErrUnauthorized, time.Unix(exp, 0).UTC().Format(time.RFC3339))
	}
	// Only the covered parameters are fed back in; anything the client
	// appended afterwards is outside the signature by design.
	covered := url.Values{ParamExpires: {expRaw}}
	if names := q.Get(ParamSignedParams); names != "" {
		covered.Set(ParamSignedParams, names)
		for _, name := range strings.Split(names, ",") {
			if vs, ok := q[name]; ok {
				covered[name] = vs
			}
		}
	}
	if !hmac.Equal([]byte(sig), []byte(s.urlSignature(u.Path, covered))) {
		return fmt.Errorf("%w: url signature does not match", ErrUnauthorized)
	}
	return nil
}

// urlSignature covers the path and the given parameters. The sp parameter is
// among them, so an attacker cannot drop a name from the covered set to free
// up the parameter it names.
func (s *Signer) urlSignature(path string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == ParamSignature {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := hmac.New(sha256.New, s.urlSigningKey())
	m.Write([]byte(path))
	for _, k := range keys {
		for _, v := range q[k] {
			m.Write([]byte{0})
			m.Write([]byte(k))
			m.Write([]byte{0})
			m.Write([]byte(v))
		}
	}
	return hex.EncodeToString(m.Sum(nil))
}
