package operatorauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub stands in for github.com's OAuth endpoints and /user.
type fakeGitHub struct {
	srv *httptest.Server
	// login is who the code resolves to.
	login string
	// tokenStatus and tokenBody override the exchange response.
	tokenBody   string
	gotRedirect string
}

func newFakeGitHub(t *testing.T, login string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{login: login}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.gotRedirect = r.PostFormValue("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		if f.tokenBody != "" {
			_, _ = w.Write([]byte(f.tokenBody))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"user-token","token_type":"bearer"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"login": f.login}))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func authWithGitHub(t *testing.T, f *fakeGitHub, adminLogins ...string) *Auth {
	t.Helper()
	o := options(t)
	o.Admins = admins(t, adminLogins...)
	o.OAuth.AuthorizeURL = f.srv.URL + "/login/oauth/authorize"
	o.OAuth.TokenURL = f.srv.URL + "/login/oauth/access_token"
	o.OAuth.APIBaseURL = f.srv.URL
	a, err := New(o)
	require.NoError(t, err)
	return a
}

// start runs the login leg and returns the state cookie and the URL the browser
// was sent to.
func start(t *testing.T, a *Auth) (*http.Cookie, *url.URL) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathGitHubLogin, nil))
	require.Equal(t, http.StatusFound, rec.Code)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	to, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	return cookies[0], to
}

func TestGitHubLogin_SendsTheBrowserToGitHubWithState(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	a := authWithGitHub(t, f, "PazerOP")

	state, to := start(t, a)
	assert.Equal(t, stateCookie, state.Name)
	assert.True(t, state.HttpOnly)
	// Strict would not come back on the navigation from github.com, so every
	// sign-in would fail on a state mismatch.
	assert.Equal(t, http.SameSiteLaxMode, state.SameSite)
	assert.NotEmpty(t, state.Value)

	assert.Equal(t, "Iv1.client", to.Query().Get("client_id"))
	assert.Equal(t, state.Value, to.Query().Get("state"))
	assert.Equal(t, "https://ci.pazer.build/auth/github/callback", to.Query().Get("redirect_uri"))
	assert.Empty(t, to.Query().Get("scope"), "learning who signed in needs no scope")
}

func callback(t *testing.T, a *Auth, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, PathGitHubCallback+"?"+query, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, r)
	return rec
}

func TestGitHubCallback_SignsInAnAdmin(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	rec := callback(t, a, "code=abc&state="+url.QueryEscape(state.Value), state)
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/", rec.Header().Get("Location"))
	assert.Equal(t, "https://ci.pazer.build/auth/github/callback", f.gotRedirect)

	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			session = c
		}
	}
	require.NotNil(t, session, "no session cookie was set")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(session)
	got := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		id, ok := IdentityFrom(req.Context())
		require.True(t, ok)
		assert.Equal(t, "PazerOP", id.Login)
		assert.Equal(t, MethodGitHub, id.Method)
	})).ServeHTTP(got, r)
	assert.Equal(t, http.StatusOK, got.Code)
}

// This is the whole point of publishing the App: a stranger can authenticate
// with GitHub perfectly well and still must not reach the dashboard.
func TestGitHubCallback_RefusesAnAccountThatIsNotAnAdmin(t *testing.T) {
	f := newFakeGitHub(t, "a-total-stranger")
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	rec := callback(t, a, "code=abc&state="+url.QueryEscape(state.Value), state)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "a-total-stranger")
	assert.Contains(t, rec.Body.String(), "CIPLATFORM_ADMIN_LOGINS", "say how to fix it")
	for _, c := range rec.Result().Cookies() {
		require.NotEqual(t, CookieName, c.Name)

	}
}

// Without the state check, any page could start a sign-in and land the victim's
// browser on a session it did not ask for.
func TestGitHubCallback_RefusesAMissingOrMismatchedState(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	tests := []struct {
		name    string
		query   string
		cookies []*http.Cookie
	}{
		{"no cookie", "code=abc&state=" + url.QueryEscape(state.Value), nil},
		{"no state parameter", "code=abc", []*http.Cookie{state}},
		{"mismatched", "code=abc&state=somebody-elses", []*http.Cookie{state}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := callback(t, a, tc.query, tc.cookies...)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			for _, c := range rec.Result().Cookies() {
				assert.NotEqual(t, CookieName, c.Name, "a session was minted without a matching state")
			}
		})
	}
}

// The state cookie is cleared on the way out whatever happened, so a captured
// callback URL cannot be replayed against it.
func TestGitHubCallback_ClearsTheStateCookie(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	rec := callback(t, a, "code=abc&state="+url.QueryEscape(state.Value), state)
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared)
}

func TestGitHubCallback_TreatsAnErrorBodyAsAFailure(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	f.tokenBody = `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	rec := callback(t, a, "code=abc&state="+url.QueryEscape(state.Value), state)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "bad_verification_code")
	for _, c := range rec.Result().Cookies() {
		assert.NotEqual(t, CookieName, c.Name)
	}
}

func TestGitHubCallback_ReportsGitHubsOwnRefusal(t *testing.T) {
	f := newFakeGitHub(t, "PazerOP")
	a := authWithGitHub(t, f, "PazerOP")
	state, _ := start(t, a)

	rec := callback(t, a, "error=access_denied&error_description=the+user+said+no&state="+url.QueryEscape(state.Value), state)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "the user said no")
}
