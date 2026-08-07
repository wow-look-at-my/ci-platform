package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func keyPEM(t *testing.T) string {
	t.Helper()
	der := x509.MarshalPKCS1PrivateKey(key(t))
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

var fixedNow = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func newApp(t *testing.T, baseURL string, now func() time.Time) *App {
	t.Helper()
	if now == nil {
		now = func() time.Time { return fixedNow }
	}
	a, err := LoadApp(Config{
		AppID:         12345,
		PrivateKeyPEM: keyPEM(t),
		BaseURL:       baseURL,
		Now:           now,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)
	return a
}

func TestLoadAppRejectsMissingConfig(t *testing.T) {
	_, err := LoadApp(Config{PrivateKeyPEM: keyPEM(t)})
	require.ErrorContains(t, err, "AppID")

	_, err = LoadApp(Config{AppID: 1})
	require.ErrorContains(t, err, "PrivateKeyPEM and PrivateKeyPath are both empty")

	_, err = LoadApp(Config{AppID: 1, PrivateKeyPEM: keyPEM(t), PrivateKeyPath: "/x"})
	require.ErrorContains(t, err, "both set")

	_, err = LoadApp(Config{AppID: 1, PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----\n"})
	require.ErrorContains(t, err, "PrivateKeyPEM does not hold a PEM RSA private key")

	_, err = LoadApp(Config{AppID: 1, PrivateKeyPath: filepath.Join(t.TempDir(), "absent.pem")})
	require.ErrorContains(t, err, "PrivateKeyPath")
}

func TestLoadAppFromPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(p, []byte(keyPEM(t)), 0o600))
	a, err := LoadApp(Config{AppID: 99, PrivateKeyPath: p})
	require.NoError(t, err)
	assert.Equal(t, int64(99), a.AppID)
	assert.Equal(t, gh.DefaultBaseURL, a.BaseURL)
	require.NotNil(t, a.PrivateKey)
}

func TestJWTClaims(t *testing.T) {
	a := newApp(t, "https://example.invalid", nil)
	tok, err := a.JWT()
	require.NoError(t, err)

	parsed, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return &key(t).PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return fixedNow }))
	require.NoError(t, err)
	assert.Equal(t, "RS256", parsed.Method.Alg())

	claims := parsed.Claims.(jwt.MapClaims)
	assert.Equal(t, "12345", claims["iss"])
	assert.EqualValues(t, fixedNow.Add(-60*time.Second).Unix(), int64(claims["iat"].(float64)))
	exp := int64(claims["exp"].(float64))
	assert.EqualValues(t, fixedNow.Add(9*time.Minute).Unix(), exp)
	assert.LessOrEqual(t, exp-fixedNow.Unix(), int64(600), "exp must stay under GitHub's 10 minute ceiling")
}

func TestJWTWithoutKeyFails(t *testing.T) {
	a := &App{AppID: 1, now: time.Now}
	_, err := a.JWT()
	require.ErrorContains(t, err, "no private key loaded")
}

// tokenServer serves /app/installations/{id}/access_tokens, counting mints.
func tokenServer(t *testing.T, expiry func(n int) time.Time) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var calls atomic.Int32
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey"), "App JWT expected")
		n := int(calls.Add(1))
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_token_%d", n),
			"expires_at": expiry(n).Format(time.RFC3339),
		})
	}))
	return srv, &calls, &bodies
}

func TestInstallationTokenIsCached(t *testing.T) {
	srv, calls, _ := tokenServer(t, func(int) time.Time { return fixedNow.Add(time.Hour) })
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	tok, exp, err := a.InstallationToken(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_1", tok)
	assert.Equal(t, fixedNow.Add(time.Hour).Unix(), exp.Unix())

	tok2, _, err := a.InstallationToken(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, tok, tok2)
	assert.EqualValues(t, 1, calls.Load(), "the second call must come from cache")

	// A different installation is a different cache entry.
	_, _, err = a.InstallationToken(context.Background(), 43)
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
}

func TestInstallationTokenRefreshesInsideFiveMinutes(t *testing.T) {
	srv, calls, _ := tokenServer(t, func(n int) time.Time {
		if n == 1 {
			return fixedNow.Add(4 * time.Minute) // inside the refresh window
		}
		return fixedNow.Add(time.Hour)
	})
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	tok, _, err := a.InstallationToken(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_1", tok)

	tok2, _, err := a.InstallationToken(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_2", tok2)
	assert.EqualValues(t, 2, calls.Load())
}

func TestScopedInstallationToken(t *testing.T) {
	srv, calls, bodies := tokenServer(t, func(int) time.Time { return fixedNow.Add(time.Hour) })
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	scope := TokenScope{
		RepositoryIDs: []int64{5, 3},
		Permissions:   map[string]string{"checks": "write", "contents": "read"},
	}
	_, _, err := a.ScopedInstallationToken(context.Background(), 42, scope)
	require.NoError(t, err)
	assert.JSONEq(t, `{"repository_ids":[5,3],"permissions":{"checks":"write","contents":"read"}}`, (*bodies)[0])

	// Same scope hits cache; a different scope does not.
	_, _, err = a.ScopedInstallationToken(context.Background(), 42, TokenScope{
		RepositoryIDs: []int64{3, 5},
		Permissions:   map[string]string{"contents": "read", "checks": "write"},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "scope key must be order-independent")

	_, _, err = a.ScopedInstallationToken(context.Background(), 42, TokenScope{Repositories: []string{"ci-platform"}})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
	assert.JSONEq(t, `{"repositories":["ci-platform"]}`, (*bodies)[1])

	// Unscoped sends no body at all.
	_, _, err = a.InstallationToken(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, "", (*bodies)[2])
}

func TestInstallationTokenIsConcurrencySafe(t *testing.T) {
	srv, calls, _ := tokenServer(t, func(int) time.Time { return fixedNow.Add(time.Hour) })
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	var wg sync.WaitGroup
	toks := make([]string, 32)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, _, err := a.InstallationToken(context.Background(), 1)
			assert.NoError(t, err)
			toks[i] = tok
		}(i)
	}
	wg.Wait()
	for _, tok := range toks {
		assert.NotEmpty(t, tok)
	}
	// Racing callers may each mint, but every one must be cached and valid; a
	// later call must not mint again.
	before := calls.Load()
	_, _, err := a.InstallationToken(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, before, calls.Load())
}

func TestInstallationTokenRejectsBadResponses(t *testing.T) {
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			_, _ = w.Write([]byte(`{"expires_at":"2026-08-06T13:00:00Z"}`))
		case 1:
			_, _ = w.Write([]byte(`{"token":"x"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad JWT"}`))
		}
	}))
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	_, _, err := a.InstallationToken(context.Background(), 1)
	require.ErrorContains(t, err, "empty token")

	mode.Store(1)
	_, _, err = a.InstallationToken(context.Background(), 2)
	require.ErrorContains(t, err, "no expiry")

	mode.Store(2)
	_, _, err = a.InstallationToken(context.Background(), 3)
	require.ErrorContains(t, err, "minting installation token for 3")

	_, _, err = a.InstallationToken(context.Background(), 0)
	require.ErrorContains(t, err, "installation id 0")
}

func TestTokenSourceAndInstallationClient(t *testing.T) {
	srv, _, _ := tokenServer(t, func(int) time.Time { return fixedNow.Add(time.Hour) })
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	ts := a.TokenSource(11, TokenScope{})
	tok, err := ts.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_1", tok)

	cli, err := a.InstallationClient(11, TokenScope{})
	require.NoError(t, err)
	require.NotNil(t, cli)
}

func TestInstallationsPaginates(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/app/installations", r.URL.Path)
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/app/installations?per_page=100&page=2>; rel="next"`, srv.URL))
			_, _ = w.Write([]byte(`[{"id":1,"account":{"login":"wow-look-at-my","type":"Organization"},"repository_selection":"all"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":2,"account":{"login":"PazerOP","type":"User"},"suspended_at":"2026-01-01T00:00:00Z"}]`))
	}))
	defer srv.Close()

	a := newApp(t, srv.URL, nil)
	got, err := a.Installations(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "wow-look-at-my", got[0].Account.Login)
	assert.False(t, got[0].Suspended())
	assert.True(t, got[1].Suspended())
}

func TestInstallationsSurfacesDecodeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"not":"an array"}`))
	}))
	defer srv.Close()
	a := newApp(t, srv.URL, nil)
	_, err := a.Installations(context.Background())
	require.ErrorContains(t, err, "decoding installations page")
}

func TestInstallationForRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/wow-look-at-my/ci-platform/installation", r.URL.Path)
		_, _ = w.Write([]byte(`{"id":77,"target_type":"Organization"}`))
	}))
	defer srv.Close()
	a := newApp(t, srv.URL, nil)

	inst, err := a.InstallationForRepo(context.Background(), gh.Repo{Owner: "wow-look-at-my", Name: "ci-platform"})
	require.NoError(t, err)
	assert.Equal(t, int64(77), inst.ID)

	_, err = a.InstallationForRepo(context.Background(), gh.Repo{Owner: "x"})
	require.ErrorContains(t, err, "owner and name")
}

func TestInstallationRepositories(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_x", "expires_at": fixedNow.Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		assert.Equal(t, "/installation/repositories", r.URL.Path)
		assert.Equal(t, "Bearer ghs_x", r.Header.Get("Authorization"))
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/installation/repositories?page=2>; rel="next"`, srv.URL))
			_, _ = w.Write([]byte(`{"total_count":2,"repositories":[{"id":1,"full_name":"o/a"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"repositories":[{"id":2,"full_name":"o/b","private":true}]}`))
	}))
	defer srv.Close()

	a := newApp(t, srv.URL, nil)
	repos, err := a.InstallationRepositories(context.Background(), 5)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	assert.Equal(t, "o/a", repos[0].FullName)
	assert.True(t, repos[1].Private)
}

func TestInstallationRepositoriesReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_x", "expires_at": fixedNow.Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	a := newApp(t, srv.URL, nil)
	_, err := a.InstallationRepositories(context.Background(), 5)
	require.ErrorContains(t, err, "listing repositories for installation 5")
}

func TestTokenScopeKeyStability(t *testing.T) {
	assert.Equal(t, "*", TokenScope{}.key())
	a := TokenScope{RepositoryIDs: []int64{2, 1}, Permissions: map[string]string{"a": "b", "c": "d"}}
	b := TokenScope{RepositoryIDs: []int64{1, 2}, Permissions: map[string]string{"c": "d", "a": "b"}}
	assert.Equal(t, a.key(), b.key())
	assert.NotEqual(t, a.key(), TokenScope{Repositories: []string{"x"}}.key())
}

func TestURLEscape(t *testing.T) {
	assert.Equal(t, "a%3Fb%23c", urlEscape("a?b#c"))
}
