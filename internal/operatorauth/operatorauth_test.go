package operatorauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/ghaccounts"
)

const token = "operator-token-that-is-long-enough"

func admins(t *testing.T, logins ...string) *ghaccounts.Set {
	t.Helper()
	set, err := ghaccounts.New("CIPLATFORM_ADMIN_LOGINS", logins)
	require.NoError(t, err)
	return set
}

func options(t *testing.T) Options {
	t.Helper()
	return Options{
		Token:      token,
		Admins:     admins(t, "PazerOP"),
		SessionKey: []byte("session-signing-key"),
		OAuth: OAuthOptions{
			ClientID: "Iv1.client", ClientSecret: "client-secret",
			RedirectURL: "https://ci.pazer.build/auth/github/callback",
		},
	}
}

func newAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := New(options(t))
	require.NoError(t, err)
	return a
}

func protected(t *testing.T) http.Handler {
	t.Helper()
	return newAuth(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"secret":"logs"}`))
	}))
}

// sessionFor mints the cookie a signed-in browser would hold.
func sessionFor(t *testing.T, a *Auth, login string, method Method) *http.Cookie {
	t.Helper()
	value, _ := a.sessions.mint(login, method)
	return &http.Cookie{Name: CookieName, Value: value}
}

// A job container can always route to the control plane, so an unauthenticated
// request to the operator API is the exact request this package exists to stop.
func TestMiddleware_RejectsAnUncredentialedRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	protected(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/1/logs/raw", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), "logs\"")
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
	assert.Contains(t, rec.Body.String(), PathGitHubLogin, "the 401 says how to get in")
}

func TestMiddleware_AcceptsBearerAndSession(t *testing.T) {
	a := newAuth(t)
	tests := []struct {
		name  string
		apply func(*http.Request)
		want  int
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }, http.StatusOK},
		{"bearer lowercase scheme", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+token) }, http.StatusOK},
		{"github session", func(r *http.Request) { r.AddCookie(sessionFor(t, a, "PazerOP", MethodGitHub)) }, http.StatusOK},
		{"token session", func(r *http.Request) { r.AddCookie(sessionFor(t, a, "", MethodToken)) }, http.StatusOK},
		{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		{"forged cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: "nope.nope"}) }, http.StatusUnauthorized},
		// The credential itself is no longer a session: a cookie has to be signed.
		{"credential as a cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: token}) }, http.StatusUnauthorized},
		{"basic auth", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+token) }, http.StatusUnauthorized},
		{"token as a query parameter", func(r *http.Request) { r.URL.RawQuery = "token=" + token }, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
			tc.apply(r)
			rec := httptest.NewRecorder()
			protected(t).ServeHTTP(rec, r)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

// A session is a signed statement, so the signing key is what makes it one.
// Another instance's key must not open this one.
func TestMiddleware_RejectsASessionSignedWithAnotherKey(t *testing.T) {
	opts := options(t)
	opts.SessionKey = []byte("a-different-instances-key")
	other, err := New(opts)
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(sessionFor(t, other, "PazerOP", MethodGitHub))
	rec := httptest.NewRecorder()
	protected(t).ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Rotating the operator credential is what somebody does when they think it
// leaked, so the sessions it handed out have to stop working then, not up to a
// session lifetime later. A GitHub session is unaffected: it was never a claim
// about the credential.
func TestMiddleware_RotatingTheCredentialEndsTheSessionsItMinted(t *testing.T) {
	before := newAuth(t)
	fromToken := sessionFor(t, before, "", MethodToken)
	fromGitHub := sessionFor(t, before, "PazerOP", MethodGitHub)

	rotated := options(t)
	rotated.Token = "a-freshly-rotated-operator-token"
	after, err := New(rotated)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"session minted from the old credential", fromToken, http.StatusUnauthorized},
		{"session from signing in with GitHub", fromGitHub, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
			r.AddCookie(tc.cookie)
			rec := httptest.NewRecorder()
			after.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, r)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

// Dropping somebody from the admin list has to take effect now, not whenever
// their session happens to run out.
func TestMiddleware_RejectsASessionForAnAccountRemovedFromTheAdminList(t *testing.T) {
	a := newAuth(t)
	cookie := sessionFor(t, a, "a-former-admin", MethodGitHub)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})).ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMiddleware_AttachesTheIdentity(t *testing.T) {
	a := newAuth(t)
	var got Identity
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		require.True(t, ok)
		got = id
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(sessionFor(t, a, "PazerOP", MethodGitHub))
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, "PazerOP", got.Login)
	assert.Equal(t, "PazerOP", got.Actor())

	r = httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, MethodToken, got.Method)
	assert.Empty(t, got.Login)
	assert.Equal(t, "operator", got.Actor(), "a shared credential names nobody")
}

func TestLogin_SetsASessionCookieAndRejectsAWrongCredential(t *testing.T) {
	a := newAuth(t)

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathLogin,
		strings.NewReader(`{"token":"wrong"}`)))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Result().Cookies())

	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathLogin,
		strings.NewReader(`{"token":"`+token+`"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	assert.Equal(t, CookieName, c.Name)
	assert.True(t, c.HttpOnly, "script must not be able to read the session")
	// Lax, not Strict: the browser returns from github.com on a sign-in, and a
	// Strict cookie is withheld on that navigation. Lax still withholds it from
	// every cross-site POST, which is what this API's mutations are.
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Positive(t, c.MaxAge)
	assert.NotContains(t, c.Value, token, "the cookie must not be the credential itself")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(c)
	rec = httptest.NewRecorder()
	protected(t).ServeHTTP(rec, r)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestLogin_AcceptsAFormBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, PathLogin, strings.NewReader("token="+token))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	newAuth(t).Handler().ServeHTTP(rec, r)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestLogin_RejectsAMalformedBody(t *testing.T) {
	rec := httptest.NewRecorder()
	newAuth(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathLogin,
		strings.NewReader("{not json")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLogout_ClearsTheCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	newAuth(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathLogout, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Negative(t, cookies[0].MaxAge)
	assert.Empty(t, cookies[0].Value)
}

// The UI asks before rendering, so it can show a sign-in button instead of a
// wall of failed requests.
func TestStatus_ReportsWhoIsSignedIn(t *testing.T) {
	a := newAuth(t)

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathStatus, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Authenticated bool   `json:"authenticated"`
		Login         string `json:"login"`
		SignInURL     string `json:"sign_in_url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.False(t, out.Authenticated)
	assert.Equal(t, PathGitHubLogin, out.SignInURL)

	r := httptest.NewRequest(http.MethodGet, PathStatus, nil)
	r.AddCookie(sessionFor(t, a, "PazerOP", MethodGitHub))
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, r)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.True(t, out.Authenticated)
	assert.Equal(t, "PazerOP", out.Login)
}

// Each of these missing would leave the API open, or the dashboard unreachable,
// while the deployment looked configured.
func TestNew_RefusesAnIncompleteGate(t *testing.T) {
	base := func(t *testing.T) Options { return options(t) }

	_, err := New(Options{})
	require.ErrorContains(t, err, "token is required")

	_, err = New(Options{Token: "short"})
	require.ErrorContains(t, err, "at least 16")

	o := base(t)
	o.Admins = nil
	_, err = New(o)
	require.ErrorContains(t, err, "admin account list is required")

	o = base(t)
	o.SessionKey = nil
	_, err = New(o)
	require.ErrorContains(t, err, "session signing key is required")

	o = base(t)
	o.OAuth.ClientSecret = ""
	_, err = New(o)
	require.ErrorContains(t, err, "OAuth client id and secret are required")

	o = base(t)
	o.OAuth.RedirectURL = ""
	_, err = New(o)
	require.ErrorContains(t, err, "redirect URL is required")
}

func TestNew_Defaults(t *testing.T) {
	a, err := New(options(t))
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, a.sessions.ttl)
	assert.False(t, a.secure, "an http deployment must not get a cookie the browser refuses to send back")
	assert.Equal(t, "https://github.com/login/oauth/authorize", a.oauth.AuthorizeURL)
	assert.Equal(t, "https://api.github.com", a.oauth.APIBaseURL)

	o := options(t)
	o.Secure, o.SessionTTL = true, time.Minute
	a, err = New(o)
	require.NoError(t, err)
	assert.True(t, a.secure)
	assert.Equal(t, time.Minute, a.sessions.ttl)
}

func TestAuthenticated_IsExportedForMixedSurfaces(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, a.Authenticated(r))
	r.Header.Set("Authorization", "Bearer "+token)
	assert.True(t, a.Authenticated(r))
}
