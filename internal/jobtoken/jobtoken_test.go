package jobtoken_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
)

const testKey = "0123456789abcdef0123456789abcdef"

func newSigner(t *testing.T, mutate func(*jobtoken.Options)) *jobtoken.Signer {
	t.Helper()
	o := jobtoken.Options{
		Key:    []byte(testKey),
		Issuer: "https://ci.example.localhost",
		Lookup: func(runID, jobID int64, attempt int) (jobtoken.Job, error) {
			return jobtoken.Job{
				RepoID:    9,
				Repo:      "wow-look-at-my/ci-platform",
				Ref:       "refs/heads/main",
				ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	s, err := jobtoken.New(o)
	require.NoError(t, err)
	return s
}

func TestNewRejectsWeakConfig(t *testing.T) {
	_, err := jobtoken.New(jobtoken.Options{Key: []byte("short"), Issuer: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32")

	_, err = jobtoken.New(jobtoken.Options{Key: []byte(testKey)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Issuer")
}

// TestScpClaimMatchesToolkitParser replays getBackendIdsFromToken() from
// @actions/artifact: base64-decode the payload, read scp, split on spaces, find
// the Actions.Results entry, split on ":" into exactly three parts.
func TestScpClaimMatchesToolkitParser(t *testing.T) {
	s := newSigner(t, nil)
	tok, err := s.Mint(42, 7, 2)
	require.NoError(t, err)

	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3, "the runtime token must be a three-part JWT")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var decoded struct {
		Scp string `json:"scp"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.NotEmpty(t, decoded.Scp, "a missing scp claim makes upload-artifact throw before its first request")

	var found bool
	for _, entry := range strings.Split(decoded.Scp, " ") {
		scopeParts := strings.Split(entry, ":")
		if scopeParts[0] != "Actions.Results" {
			continue
		}
		require.Len(t, scopeParts, 3, "Actions.Results needs exactly three colon-separated parts")
		assert.Equal(t, jobtoken.BackendRunID(42), scopeParts[1])
		assert.Equal(t, jobtoken.BackendJobID(7, 2), scopeParts[2])
		found = true
	}
	assert.True(t, found, "no Actions.Results scope entry")
}

func TestBackendIDsAreStableAndAttemptScoped(t *testing.T) {
	assert.Equal(t, jobtoken.BackendRunID(42), jobtoken.BackendRunID(42))
	assert.NotEqual(t, jobtoken.BackendRunID(42), jobtoken.BackendRunID(43))
	assert.NotEqual(t, jobtoken.BackendJobID(7, 1), jobtoken.BackendJobID(7, 2),
		"a re-run must not be able to write the previous attempt's artifacts")
	_, err := uuidParse(jobtoken.BackendRunID(42))
	assert.NoError(t, err, "backend ids are UUIDs, which is what the toolkit logs")
}

func uuidParse(s string) (string, error) {
	if len(s) != 36 {
		return "", errors.New("not a uuid")
	}
	return s, nil
}

func TestMintAndVerifyRoundTrip(t *testing.T) {
	s := newSigner(t, nil)
	tok, err := s.Mint(42, 7, 1)
	require.NoError(t, err)

	claims, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, int64(42), claims.RunID)
	assert.Equal(t, int64(7), claims.JobID)
	assert.Equal(t, 1, claims.Attempt)
	assert.Equal(t, int64(9), claims.RepoID)
	assert.Equal(t, "wow-look-at-my/ci-platform", claims.Repo)
	assert.Equal(t, "refs/heads/main", claims.Ref)
	assert.True(t, claims.Has(jobtoken.ScopeArtifactsWrite))
	assert.True(t, claims.CanReadCache())
	assert.True(t, claims.CanWriteCache())
	assert.False(t, claims.Has(jobtoken.Scope("repo:write")), "a job token never carries repository write access")

	runID, jobID := claims.BackendIDs()
	assert.Equal(t, jobtoken.BackendRunID(42), runID)
	assert.Equal(t, jobtoken.BackendJobID(7, 1), jobID)
}

func TestMintRejectsIncompleteJobs(t *testing.T) {
	s := newSigner(t, nil)
	base := jobtoken.Job{RunID: 1, JobID: 1, Attempt: 1, Repo: "o/r", Scopes: jobtoken.DefaultScopes, ExpiresAt: time.Now().Add(time.Hour)}

	cases := map[string]func(*jobtoken.Job){
		"no run":     func(j *jobtoken.Job) { j.RunID = 0 },
		"no job":     func(j *jobtoken.Job) { j.JobID = 0 },
		"no attempt": func(j *jobtoken.Job) { j.Attempt = 0 },
		"no repo":    func(j *jobtoken.Job) { j.Repo = "" },
		"no scopes":  func(j *jobtoken.Job) { j.Scopes = nil },
		"no expiry":  func(j *jobtoken.Job) { j.ExpiresAt = time.Time{} },
		"bad scope":  func(j *jobtoken.Job) { j.Scopes = []jobtoken.Scope{"root:everything"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			j := base
			mutate(&j)
			_, err := s.MintJob(j)
			assert.Error(t, err)
		})
	}
}

func TestMintWithoutLookupFailsLoudly(t *testing.T) {
	s := newSigner(t, func(o *jobtoken.Options) { o.Lookup = nil })
	_, err := s.Mint(1, 1, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Lookup configured")
}

func TestMintSurfacesLookupFailure(t *testing.T) {
	s := newSigner(t, func(o *jobtoken.Options) {
		o.Lookup = func(int64, int64, int) (jobtoken.Job, error) {
			return jobtoken.Job{}, errors.New("database down")
		}
	})
	_, err := s.Mint(1, 1, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database down")
}

func TestVerifyRejections(t *testing.T) {
	s := newSigner(t, nil)
	good, err := s.Mint(42, 7, 1)
	require.NoError(t, err)

	t.Run("empty", func(t *testing.T) {
		_, err := s.Verify("")
		assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := s.Verify("not.a.jwt")
		assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
	})
	t.Run("wrong key", func(t *testing.T) {
		other := newSigner(t, func(o *jobtoken.Options) { o.Key = []byte("ffffffffffffffffffffffffffffffff") })
		_, err := other.Verify(good)
		assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
	})
	t.Run("wrong issuer", func(t *testing.T) {
		other := newSigner(t, func(o *jobtoken.Options) { o.Issuer = "https://evil.localhost" })
		_, err := other.Verify(good)
		assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
	})
	t.Run("expired", func(t *testing.T) {
		past := newSigner(t, func(o *jobtoken.Options) {
			o.Grace = time.Second
			o.Lookup = func(int64, int64, int) (jobtoken.Job, error) {
				return jobtoken.Job{Repo: "o/r", ExpiresAt: time.Now().Add(-time.Hour)}, nil
			}
		})
		tok, err := past.Mint(1, 1, 1)
		require.NoError(t, err)
		_, err = past.Verify(tok)
		require.ErrorIs(t, err, jobtoken.ErrUnauthorized)
		assert.Contains(t, err.Error(), "expired")
	})
	t.Run("alg none is refused", func(t *testing.T) {
		// A "none"-algorithm token with a well-formed payload must not pass.
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		body := base64.RawURLEncoding.EncodeToString([]byte(`{"run_id":42,"job_id":7,"attempt":1,"iss":"https://ci.example.localhost","exp":9999999999}`))
		_, err := s.Verify(hdr + "." + body + ".")
		assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
	})
}

func TestVerifierMiddleware(t *testing.T) {
	s := newSigner(t, nil)
	tok, err := s.Mint(42, 7, 1)
	require.NoError(t, err)

	var seen *jobtoken.Claims
	h := s.Verifier(jobtoken.ScopeArtifactsWrite).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := jobtoken.ClaimsFrom(r.Context())
		require.True(t, ok)
		seen = c
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("accepts a valid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		require.NotNil(t, seen)
		assert.Equal(t, int64(7), seen.JobID)
	})

	t.Run("accepts lowercase bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNoContent, rec.Code)
	})

	t.Run("401 with a reason when unauthenticated", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Contains(t, body["message"], "no token presented")
	})

	t.Run("403 when the scope is missing", func(t *testing.T) {
		limited := newSigner(t, func(o *jobtoken.Options) {
			o.Lookup = func(int64, int64, int) (jobtoken.Job, error) {
				return jobtoken.Job{Repo: "o/r", Scopes: []jobtoken.Scope{jobtoken.ScopeLogsWrite}, ExpiresAt: time.Now().Add(time.Hour)}, nil
			}
		})
		lt, err := limited.Mint(1, 1, 1)
		require.NoError(t, err)

		lh := limited.Verifier(jobtoken.ScopeArtifactsWrite).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler must not run")
		}))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+lt)
		rec := httptest.NewRecorder()
		lh.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "artifacts:write")
	})
}

func TestVerifierCustomDenialWriter(t *testing.T) {
	s := newSigner(t, nil)
	h := s.Verifier().WithDenialWriter(func(w http.ResponseWriter, _ *http.Request, status int, reason string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "unauthenticated", "msg": reason})
	}).Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"unauthenticated"`)
}

func TestVerifierCheck(t *testing.T) {
	s := newSigner(t, nil)
	tok, err := s.Mint(42, 7, 1)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/twirp/x/Y", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	claims, status, reason := s.Verifier().Check(req)
	require.Empty(t, reason)
	assert.Zero(t, status)
	assert.Equal(t, int64(42), claims.RunID)
}

func TestBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Empty(t, jobtoken.BearerToken(req))

	req.Header.Set("Authorization", "Basic abc")
	assert.Empty(t, jobtoken.BearerToken(req))

	req.Header.Set("Authorization", "Bearer  abc ")
	assert.Equal(t, "abc", jobtoken.BearerToken(req))
}

func TestSignAndVerifyURL(t *testing.T) {
	s := newSigner(t, nil)
	signed, err := s.SignURL("https://ci.example.localhost/_apis/artifactcache/artifacts/5?scope=main", time.Hour)
	require.NoError(t, err)

	u, err := url.Parse(signed)
	require.NoError(t, err)
	assert.NotEmpty(t, u.Query().Get("sig"))
	assert.NotEmpty(t, u.Query().Get("exp"))
	require.NoError(t, s.VerifyURL(u))

	t.Run("tampered path", func(t *testing.T) {
		bad := *u
		bad.Path = "/_apis/artifactcache/artifacts/6"
		assert.ErrorIs(t, s.VerifyURL(&bad), jobtoken.ErrUnauthorized)
	})
	t.Run("tampered query", func(t *testing.T) {
		bad := *u
		q := bad.Query()
		q.Set("scope", "other-branch")
		bad.RawQuery = q.Encode()
		assert.ErrorIs(t, s.VerifyURL(&bad), jobtoken.ErrUnauthorized)
	})
	t.Run("no signature", func(t *testing.T) {
		bad := *u
		q := bad.Query()
		q.Del("sig")
		bad.RawQuery = q.Encode()
		assert.ErrorIs(t, s.VerifyURL(&bad), jobtoken.ErrUnauthorized)
	})
	t.Run("bad expiry", func(t *testing.T) {
		bad := *u
		q := bad.Query()
		q.Set("exp", "soon")
		bad.RawQuery = q.Encode()
		assert.ErrorIs(t, s.VerifyURL(&bad), jobtoken.ErrUnauthorized)
	})
	t.Run("expired", func(t *testing.T) {
		expired := newSigner(t, func(o *jobtoken.Options) {
			o.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
		})
		old, err := expired.SignURL("https://ci.example.localhost/x", time.Minute)
		require.NoError(t, err)
		ou, err := url.Parse(old)
		require.NoError(t, err)
		err = s.VerifyURL(ou)
		require.ErrorIs(t, err, jobtoken.ErrUnauthorized)
		assert.Contains(t, err.Error(), "expired")
	})
	t.Run("ttl must be positive", func(t *testing.T) {
		_, err := s.SignURL("https://x/y", 0)
		assert.Error(t, err)
	})
}

func TestSignedURLIsNotABearerToken(t *testing.T) {
	s := newSigner(t, nil)
	signed, err := s.SignURL("https://ci.example.localhost/x", time.Hour)
	require.NoError(t, err)
	u, err := url.Parse(signed)
	require.NoError(t, err)

	_, err = s.Verify(u.Query().Get("sig"))
	assert.ErrorIs(t, err, jobtoken.ErrUnauthorized)
}

func TestScopeValid(t *testing.T) {
	for _, s := range jobtoken.DefaultScopes {
		assert.True(t, s.Valid())
	}
	assert.True(t, jobtoken.ScopeCacheRead.Valid())
	assert.True(t, jobtoken.ScopeCacheWrite.Valid())
	assert.False(t, jobtoken.Scope("").Valid())
	assert.False(t, jobtoken.Scope("admin").Valid())
}

// TestSignedURLToleratesAppendedParams is the Azure case: the Storage SDK adds
// comp= and blockid= to the upload URL after it is handed over, so a signature
// covering the whole query would reject every block upload.
func TestSignedURLToleratesAppendedParams(t *testing.T) {
	s := newSigner(t, nil)
	signed, err := s.SignURL("https://ci.example.localhost/_apis/results/artifacts/upload/1", time.Hour)
	require.NoError(t, err)

	for _, suffix := range []string{
		"&comp=block&blockid=YmxvY2stMQ==",
		"&comp=blocklist",
		"&path=out/notes.md",
	} {
		u, err := url.Parse(signed + suffix)
		require.NoError(t, err)
		assert.NoError(t, s.VerifyURL(u), "appending %s must not invalidate the signature", suffix)
	}

	// The path is still covered, so the URL cannot be pointed at another object.
	other, err := url.Parse(strings.Replace(signed, "/upload/1", "/upload/2", 1))
	require.NoError(t, err)
	assert.ErrorIs(t, s.VerifyURL(other), jobtoken.ErrUnauthorized)
}

// TestSignedParamsCannotBeStripped stops an attacker freeing a covered
// parameter by editing the list of covered names.
func TestSignedParamsCannotBeStripped(t *testing.T) {
	s := newSigner(t, nil)
	signed, err := s.SignURL("https://ci.example.localhost/x?scope=main", time.Hour)
	require.NoError(t, err)
	u, err := url.Parse(signed)
	require.NoError(t, err)
	require.Equal(t, "scope", u.Query().Get("sp"))

	stripped := *u
	q := stripped.Query()
	q.Del("sp")
	q.Set("scope", "other-branch")
	stripped.RawQuery = q.Encode()
	assert.ErrorIs(t, s.VerifyURL(&stripped), jobtoken.ErrUnauthorized)
}
