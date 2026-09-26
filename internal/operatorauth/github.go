package operatorauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Login and callback are unauthenticated by necessity: they are how a browser stops being anonymous.
const (
	PathGitHubLogin    = "/auth/github/login"
	PathGitHubCallback = "/auth/github/callback"
)

// stateCookie holds the CSRF state across the round trip to GitHub; SameSite=Lax survives the return from github.com.
const stateCookie = "ci_oauth_state"

const stateTTL = 10 * time.Minute

// OAuthOptions is the GitHub App's own OAuth client. A GitHub App is an OAuth
// provider in its own right, so signing operators in needs no second App: the
// client ID and secret are on the App's settings page.
type OAuthOptions struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must match the callback URL registered on the App.
	RedirectURL string
	// AuthorizeURL, TokenURL and APIBaseURL default to github.com and
	// api.github.com. They exist so the flow can be pointed at a test server.
	AuthorizeURL string
	TokenURL     string
	APIBaseURL   string
}

func (o *OAuthOptions) withDefaults() {
	if o.AuthorizeURL == "" {
		o.AuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if o.TokenURL == "" {
		o.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if o.APIBaseURL == "" {
		o.APIBaseURL = "https://api.github.com"
	}
}

// githubLogin sends the browser to GitHub with a state value this server can
// recognise on the way back.
func (a *Auth) githubLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomState()
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "could not start sign-in: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(stateTTL / time.Second),
	})

	q := url.Values{}
	q.Set("client_id", a.oauth.ClientID)
	q.Set("redirect_uri", a.oauth.RedirectURL)
	q.Set("state", state)
	// No scopes: the only thing this flow needs to learn is which account is
	// signing in, and an unscoped user-to-server token already answers that.
	// Asking for more would be asking for access nothing here uses.
	q.Set("scope", "")
	http.Redirect(w, r, a.oauth.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// githubCallback finishes the flow: verify the state, exchange the code for a
// user token, ask GitHub who it belongs to, and mint a session if that account
// is on the admin list.
func (a *Auth) githubCallback(w http.ResponseWriter, r *http.Request) {
	// The state cookie is single-use whatever happens next, so a failed attempt
	// cannot be retried with the same value.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})

	if msg := r.URL.Query().Get("error_description"); msg != "" {
		a.fail(w, http.StatusBadRequest, "GitHub refused the sign-in: "+msg)
		return
	}
	want, err := r.Cookie(stateCookie)
	if err != nil || want.Value == "" {
		a.fail(w, http.StatusBadRequest,
			"this sign-in did not start here, or it took longer than ten minutes; start again at "+PathGitHubLogin)
		return
	}
	got := r.URL.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(got), []byte(want.Value)) != 1 {
		a.fail(w, http.StatusBadRequest, "sign-in state did not match; start again at "+PathGitHubLogin)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.fail(w, http.StatusBadRequest, "GitHub sent no authorization code")
		return
	}

	token, err := a.exchangeCode(r.Context(), code)
	if err != nil {
		a.log.Error("oauth code exchange failed", "err", err)
		a.fail(w, http.StatusBadGateway, "could not exchange the code with GitHub: "+err.Error())
		return
	}
	login, err := a.githubLoginName(r.Context(), token)
	if err != nil {
		a.log.Error("oauth identity lookup failed", "err", err)
		a.fail(w, http.StatusBadGateway, "could not ask GitHub who signed in: "+err.Error())
		return
	}

	if !a.admins.Contains(login) {
		// Say who was refused. A sign-in that fails without naming the account
		// is indistinguishable from a broken deployment, and the operator is
		// usually one typo away from the answer.
		a.log.Warn("refused a dashboard sign-in", "login", login, "admins", a.admins.String())
		a.fail(w, http.StatusForbidden, fmt.Sprintf(
			"%s is not an administrator of this instance. Add the account to CIPLATFORM_ADMIN_LOGINS "+
				"and restart to let it in.", login))
		return
	}

	value, id := a.sessions.mint(login, MethodGitHub)
	a.setSession(w, value)
	a.log.Info("dashboard sign-in", "login", id.Login, "expires", id.Expires)
	http.Redirect(w, r, "/", http.StatusFound)
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (a *Auth) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", a.oauth.ClientID)
	form.Set("client_secret", a.oauth.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", a.oauth.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.oauth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub answers form-encoded, which is a different parse for
	// no benefit.
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint answered %s", resp.Status)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("token endpoint answered unparseable JSON: %w", err)
	}
	// GitHub reports a refused exchange as 200 with an error field, so the
	// status code alone is not the answer.
	if tr.Error != "" {
		return "", fmt.Errorf("%s: %s", tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access token")
	}
	return tr.AccessToken, nil
}

func (a *Auth) githubLoginName(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(a.oauth.APIBaseURL, "/")+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /user answered %s", resp.Status)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return "", fmt.Errorf("GET /user answered unparseable JSON: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("GET /user answered with no login")
	}
	return user.Login, nil
}

func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
