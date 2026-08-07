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
)

const token = "operator-token-that-is-long-enough"

func newAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := New(Options{Token: token})
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

// A job container can always route to the control plane, so an unauthenticated
// request to the operator API is the exact request this package exists to stop.
func TestMiddleware_RejectsAnUncredentialedRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	protected(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/1/logs/raw", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), "logs\"")
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
}

func TestMiddleware_AcceptsBearerAndCookie(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*http.Request)
		want  int
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }, http.StatusOK},
		{"bearer lowercase scheme", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+token) }, http.StatusOK},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: token}) }, http.StatusOK},
		{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		{"wrong cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: "nope"}) }, http.StatusUnauthorized},
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
	assert.True(t, c.HttpOnly, "script must not be able to read the credential")
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite,
		"a page on another origin must not be able to make the browser cancel a run")
	assert.Positive(t, c.MaxAge)

	// The cookie the login handed out actually opens the gate.
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

// The UI asks before rendering, so it can show a sign-in form instead of a
// wall of failed requests.
func TestStatus_ReportsBothStates(t *testing.T) {
	a := newAuth(t)
	for _, tc := range []struct {
		name string
		auth bool
	}{{"signed out", false}, {"signed in", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, PathStatus, nil)
			if tc.auth {
				r.AddCookie(&http.Cookie{Name: CookieName, Value: token})
			}
			rec := httptest.NewRecorder()
			a.Handler().ServeHTTP(rec, r)
			require.Equal(t, http.StatusOK, rec.Code)

			var body struct {
				Authenticated bool `json:"authenticated"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.auth, body.Authenticated)
		})
	}
}

// An empty or short token would leave the API open while the deployment looked
// protected, which is the failure this whole package exists to prevent.
func TestNew_RefusesAWeakToken(t *testing.T) {
	_, err := New(Options{})
	require.ErrorContains(t, err, "token is required")

	_, err = New(Options{Token: "short"})
	require.ErrorContains(t, err, "at least 16")
}

func TestNew_Defaults(t *testing.T) {
	a, err := New(Options{Token: token})
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, a.ttl)
	assert.False(t, a.secure, "an http deployment must not get a cookie the browser refuses to send back")

	a, err = New(Options{Token: token, Secure: true, SessionTTL: time.Minute})
	require.NoError(t, err)
	assert.True(t, a.secure)
	assert.Equal(t, time.Minute, a.ttl)
}

func TestAuthenticated_IsExportedForMixedSurfaces(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, a.Authenticated(r))
	r.Header.Set("Authorization", "Bearer "+token)
	assert.True(t, a.Authenticated(r))
}
