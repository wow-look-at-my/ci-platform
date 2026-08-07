package oidc_test

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/oidc"
)

const issuer = "https://ci.example.ghe.com"

func fullSubject() *oidc.Subject {
	return &oidc.Subject{
		Repository:           "wow-look-at-my/ci-platform",
		RepositoryOwner:      "wow-look-at-my",
		RepositoryID:         41,
		RepositoryOwnerID:    7,
		RepositoryVisibility: "private",
		Ref:                  "refs/heads/main",
		RefType:              oidc.RefTypeBranch,
		SHA:                  "cafebabe",
		Actor:                "PazerOP",
		ActorID:              3,
		Workflow:             "CI",
		WorkflowRef:          "wow-look-at-my/ci-platform/.github/workflows/ci.yml@refs/heads/main",
		JobWorkflowRef:       "wow-look-at-my/ci-platform/.github/workflows/ci.yml@refs/heads/main",
		RunID:                42,
		RunNumber:            9,
		RunAttempt:           1,
		EventName:            "push",
	}
}

func newKeyring(t *testing.T, ttl time.Duration) (*oidc.Keyring, *oidc.FileKeyStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys", "oidc.json")
	ks, err := oidc.NewFileKeyStore(path)
	require.NoError(t, err)
	kr, err := oidc.NewKeyring(context.Background(), ks, oidc.KeyringOptions{TokenTTL: ttl})
	require.NoError(t, err)
	return kr, ks, path
}

func newSigner(t *testing.T, scopes []jobtoken.Scope) *jobtoken.Signer {
	t.Helper()
	s, err := jobtoken.New(jobtoken.Options{
		Key:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer: issuer,
		Lookup: func(int64, int64, int) (jobtoken.Job, error) {
			return jobtoken.Job{Repo: "wow-look-at-my/ci-platform", Scopes: scopes, ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
	})
	require.NoError(t, err)
	return s
}

func newService(t *testing.T, sub *oidc.Subject, scopes []jobtoken.Scope) (*oidc.Service, *jobtoken.Signer, *oidc.Keyring) {
	t.Helper()
	kr, _, _ := newKeyring(t, 15*time.Minute)
	signer := newSigner(t, scopes)
	svc, err := oidc.New(oidc.Options{
		Issuer:   issuer,
		Keyring:  kr,
		Verifier: signer.Verifier(),
		Lookup: func(context.Context, int64, int64, int) (*oidc.Subject, error) {
			return sub, nil
		},
	})
	require.NoError(t, err)
	return svc, signer, kr
}

func TestNewValidatesOptions(t *testing.T) {
	kr, _, _ := newKeyring(t, time.Minute)
	signer := newSigner(t, jobtoken.DefaultScopes)
	lookup := func(context.Context, int64, int64, int) (*oidc.Subject, error) { return nil, nil }

	for name, o := range map[string]oidc.Options{
		"no issuer":   {Keyring: kr, Verifier: signer.Verifier(), Lookup: lookup},
		"no keyring":  {Issuer: issuer, Verifier: signer.Verifier(), Lookup: lookup},
		"no verifier": {Issuer: issuer, Keyring: kr, Lookup: lookup},
		"no lookup":   {Issuer: issuer, Keyring: kr, Verifier: signer.Verifier()},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := oidc.New(o)
			assert.Error(t, err)
		})
	}
}

// TestIDTokenEndpointMatchesToolkitClient replays @actions/core's getIDToken.
func TestIDTokenEndpointMatchesToolkitClient(t *testing.T) {
	svc, signer, _ := newService(t, fullSubject(), jobtoken.DefaultScopes)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	runtimeToken, err := signer.Mint(42, 7, 1)
	require.NoError(t, err)

	// getIDToken appends "&audience=", so the injected URL must already have a
	// query string. Build the request exactly as the client does.
	requestURL := oidc.RequestURL(srv.URL)
	require.Contains(t, requestURL, "?", "ACTIONS_ID_TOKEN_REQUEST_URL must carry a query string")
	full := requestURL + "&audience=" + url.QueryEscape("https://buildhost.pazer.io")

	req, err := http.NewRequest(http.MethodGet, full, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+runtimeToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Value string `json:"value"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.Value)

	// Verify it against the published JWKS, which is what buildhost does.
	parsed, err := jwt.Parse(body.Value, jwksKeyFunc(t, srv.URL), jwt.WithIssuer(issuer), jwt.WithAudience("https://buildhost.pazer.io"))
	require.NoError(t, err)
	require.True(t, parsed.Valid)

	claims := parsed.Claims.(jwt.MapClaims)
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:ref:refs/heads/main", claims["sub"])
	assert.Equal(t, "wow-look-at-my/ci-platform", claims["repository"])
	assert.Equal(t, "wow-look-at-my", claims["repository_owner"])
	assert.Equal(t, "41", claims["repository_id"], "ids are strings, as GitHub issues them")
	assert.Equal(t, "7", claims["repository_owner_id"])
	assert.Equal(t, "private", claims["repository_visibility"])
	assert.Equal(t, "refs/heads/main", claims["ref"])
	assert.Equal(t, "branch", claims["ref_type"])
	assert.Equal(t, "cafebabe", claims["sha"])
	assert.Equal(t, "PazerOP", claims["actor"])
	assert.Equal(t, "3", claims["actor_id"])
	assert.Equal(t, "CI", claims["workflow"])
	assert.Equal(t, "42", claims["run_id"])
	assert.Equal(t, "9", claims["run_number"])
	assert.Equal(t, "1", claims["run_attempt"])
	assert.Equal(t, "push", claims["event_name"])
	assert.Equal(t, "self-hosted", claims["runner_environment"])
	assert.NotEmpty(t, claims["jti"])
	assert.NotEmpty(t, claims["nbf"])
	assert.NotContains(t, claims, "environment", "a job with no environment claims none")
	assert.Equal(t, "RS256", parsed.Method.Alg())
	assert.NotEmpty(t, parsed.Header["kid"], "every token names its key")
}

func jwksKeyFunc(t *testing.T, base string) jwt.Keyfunc {
	t.Helper()
	resp, err := http.Get(base + oidc.PathJWKS)
	require.NoError(t, err)
	defer resp.Body.Close()
	var doc oidc.JWKS
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		require.NoError(t, err)
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		require.NoError(t, err)
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		k, ok := keys[kid]
		if !ok {
			return nil, errors.New("unknown kid " + kid)
		}
		return k, nil
	}
}

func TestSubjectGrammar(t *testing.T) {
	branch := fullSubject()
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:ref:refs/heads/main", oidc.SubjectFor(branch))

	tag := fullSubject()
	tag.Ref, tag.RefType = "refs/tags/v1.2.3", oidc.RefTypeTag
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:ref:refs/tags/v1.2.3", oidc.SubjectFor(tag))

	pr := fullSubject()
	pr.EventName, pr.Ref = "pull_request", "refs/pull/12/merge"
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:pull_request", oidc.SubjectFor(pr))

	env := fullSubject()
	env.Environment = "production"
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:environment:production", oidc.SubjectFor(env))

	// Environment wins over the event, as it does on GitHub.
	envPR := fullSubject()
	envPR.EventName, envPR.Environment = "pull_request", "staging"
	assert.Equal(t, "repo:wow-look-at-my/ci-platform:environment:staging", oidc.SubjectFor(envPR))
}

func TestPullRequestClaims(t *testing.T) {
	sub := fullSubject()
	sub.EventName = "pull_request"
	sub.Ref = "refs/pull/12/merge"
	sub.HeadRef = "feature-branch"
	sub.BaseRef = "main"
	sub.Environment = "staging"

	svc, _, _ := newService(t, sub, jobtoken.DefaultScopes)
	tok, err := svc.Issue(sub, "aud")
	require.NoError(t, err)

	claims := unverifiedClaims(t, tok)
	assert.Equal(t, "feature-branch", claims["head_ref"])
	assert.Equal(t, "main", claims["base_ref"])
	assert.Equal(t, "staging", claims["environment"])
}

func unverifiedClaims(t *testing.T, token string) jwt.MapClaims {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims jwt.MapClaims
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims
}

// TestForkPRGetsNoToken is the security case: a fork PR's workflow is
// attacker-controlled, so it must never receive a credential.
func TestForkPRGetsNoToken(t *testing.T) {
	sub := fullSubject()
	sub.IsForkPR = true
	sub.EventName = "pull_request"

	svc, signer, _ := newService(t, sub, jobtoken.DefaultScopes)

	_, err := svc.Issue(sub, "https://buildhost.pazer.io")
	assert.ErrorIs(t, err, oidc.ErrForkPR)

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	tok, err := signer.Mint(42, 7, 1)
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, oidc.RequestURL(srv.URL)+"&audience=x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Contains(t, body["message"], "fork pull requests are not issued ID tokens")
	assert.Contains(t, body["message"], "attacker-controlled")
}

func TestTokenEndpointRejections(t *testing.T) {
	svc, signer, _ := newService(t, fullSubject(), jobtoken.DefaultScopes)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	tok, err := signer.Mint(42, 7, 1)
	require.NoError(t, err)

	do := func(t *testing.T, target, auth string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, target, nil)
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("no token", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, do(t, oidc.RequestURL(srv.URL)+"&audience=x", "").StatusCode)
	})
	t.Run("bad token", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, do(t, oidc.RequestURL(srv.URL)+"&audience=x", "Bearer nonsense").StatusCode)
	})
	t.Run("no audience", func(t *testing.T) {
		resp := do(t, oidc.RequestURL(srv.URL), "Bearer "+tok)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("missing oidc scope", func(t *testing.T) {
		limited := newSigner(t, []jobtoken.Scope{jobtoken.ScopeLogsWrite})
		lsvc, err := oidc.New(oidc.Options{
			Issuer: issuer, Keyring: mustKeyring(t), Verifier: limited.Verifier(),
			Lookup: func(context.Context, int64, int64, int) (*oidc.Subject, error) { return fullSubject(), nil },
		})
		require.NoError(t, err)
		lsrv := httptest.NewServer(lsvc.Handler())
		defer lsrv.Close()

		lt, err := limited.Mint(42, 7, 1)
		require.NoError(t, err)
		req, _ := http.NewRequest(http.MethodGet, oidc.RequestURL(lsrv.URL)+"&audience=x", nil)
		req.Header.Set("Authorization", "Bearer "+lt)
		resp, err := lsrv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	t.Run("lookup failure is reported, not papered over", func(t *testing.T) {
		fsvc, err := oidc.New(oidc.Options{
			Issuer: issuer, Keyring: mustKeyring(t), Verifier: signer.Verifier(),
			Lookup: func(context.Context, int64, int64, int) (*oidc.Subject, error) {
				return nil, errors.New("run store unavailable")
			},
		})
		require.NoError(t, err)
		fsrv := httptest.NewServer(fsvc.Handler())
		defer fsrv.Close()

		req, _ := http.NewRequest(http.MethodGet, oidc.RequestURL(fsrv.URL)+"&audience=x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := fsrv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "run store unavailable")
	})
	t.Run("unknown job", func(t *testing.T) {
		nsvc, err := oidc.New(oidc.Options{
			Issuer: issuer, Keyring: mustKeyring(t), Verifier: signer.Verifier(),
			Lookup: func(context.Context, int64, int64, int) (*oidc.Subject, error) { return nil, nil },
		})
		require.NoError(t, err)
		nsrv := httptest.NewServer(nsvc.Handler())
		defer nsrv.Close()

		req, _ := http.NewRequest(http.MethodGet, oidc.RequestURL(nsrv.URL)+"&audience=x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := nsrv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
