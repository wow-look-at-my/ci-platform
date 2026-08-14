// Package operatorauth gates the operator surface -- the REST API, the SSE
// tails, and the raw log and artifact downloads -- and decides who is allowed
// through it.
//
// It exists because every job container can route to the control plane: a job
// needs CIPLATFORM_PUBLIC_URL to upload artifacts, restore cache, and fetch an
// ID token. An unauthenticated /api/v1 on that same listener is therefore
// reachable from inside any workflow, including a fork PR's, which makes every
// other repository's logs and artifacts readable and every run cancellable by
// anything that can run a step.
//
// There are two ways to prove you are an operator, because there are two kinds
// of caller. A person signs in with GitHub and gets a session naming their
// account, checked against the admin list. A script sends the shared operator
// credential as a bearer header, and is recorded as "operator" because a shared
// credential names nobody.
//
// see docs/security.md
package operatorauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/ghaccounts"
)

// CookieName holds the session in a browser. It is HttpOnly, so the UI never
// reads it from script.
const CookieName = "ci_operator"

// MinTokenLen is the shortest operator credential accepted. It is a bearer
// secret with no lockout in front of it, so its entropy is the only thing
// standing between an attacker and the API.
const MinTokenLen = 16

// PathLogin, PathLogout and PathStatus are the unauthenticated endpoints the UI
// needs before it holds a session.
const (
	PathLogin  = "/auth/login"
	PathLogout = "/auth/logout"
	PathStatus = "/auth/status"
)

// Options configures the gate.
type Options struct {
	// Token is the shared operator credential, for scripts. Required.
	Token string
	// Admins are the GitHub accounts allowed to sign in. Required: a gate whose
	// admin list is empty is either open to everybody or open to nobody, and
	// guessing which is not this package's decision to make.
	Admins *ghaccounts.Set
	// OAuth is the GitHub App's own OAuth client. Required.
	OAuth OAuthOptions
	// SessionKey signs session cookies. Required.
	SessionKey []byte
	// SessionTTL bounds how long a session lasts. Default 12h.
	SessionTTL time.Duration
	// Secure marks cookies Secure. Set from the public URL's scheme: a Secure
	// cookie is never sent over plain http, so forcing it on an http deployment
	// silently logs every operator out on the next request.
	Secure     bool
	Logger     *slog.Logger
	HTTPClient *http.Client
	Now        func() time.Time
}

// Auth is the middleware and its sign-in endpoints.
type Auth struct {
	token    []byte
	admins   *ghaccounts.Set
	oauth    OAuthOptions
	sessions *signer
	secure   bool
	log      *slog.Logger
	http     *http.Client
	mux      *http.ServeMux
}

// New builds the gate. A missing credential, admin list, OAuth client or
// session key is an error rather than an open door: a gate that lets everybody
// through is worse than no gate, because the deployment looks protected.
func New(opts Options) (*Auth, error) {
	if opts.Token == "" {
		return nil, errors.New("operatorauth: token is required")
	}
	if len(opts.Token) < MinTokenLen {
		return nil, fmt.Errorf("operatorauth: token is %d characters; at least %d are required, "+
			"because it is the only thing protecting every repository's logs and artifacts",
			len(opts.Token), MinTokenLen)
	}
	if opts.Admins == nil {
		return nil, errors.New("operatorauth: the admin account list is required")
	}
	if len(opts.SessionKey) == 0 {
		return nil, errors.New("operatorauth: a session signing key is required")
	}
	if opts.OAuth.ClientID == "" || opts.OAuth.ClientSecret == "" {
		return nil, errors.New("operatorauth: the GitHub App's OAuth client id and secret are required, " +
			"because signing in with GitHub is how a person reaches the dashboard")
	}
	if opts.OAuth.RedirectURL == "" {
		return nil, errors.New("operatorauth: the OAuth redirect URL is required and must match the App's setting")
	}
	opts.OAuth.withDefaults()
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}

	a := &Auth{
		token:  []byte(opts.Token),
		admins: opts.Admins,
		oauth:  opts.OAuth,
		sessions: &signer{
			key:      opts.SessionKey,
			tokenKey: tokenSigningKey(opts.SessionKey, opts.Token),
			ttl:      opts.SessionTTL,
			now:      opts.Now,
		},
		secure: opts.Secure,
		log:    opts.Logger,
		http:   opts.HTTPClient,
	}
	a.mux = http.NewServeMux()
	a.mux.HandleFunc("POST "+PathLogin, a.login)
	a.mux.HandleFunc("POST "+PathLogout, a.logout)
	a.mux.HandleFunc("GET "+PathStatus, a.status)
	a.mux.HandleFunc("GET "+PathGitHubLogin, a.githubLogin)
	a.mux.HandleFunc("GET "+PathGitHubCallback, a.githubCallback)
	return a, nil
}

// Handler serves the sign-in, sign-out, and status endpoints. These are the
// only operator-surface routes that answer without a credential.
func (a *Auth) Handler() http.Handler { return a.mux }

type contextKey struct{}

// IdentityFrom returns who a request is acting as. The second result is false
// on an unauthenticated request, which the middleware never forwards.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// Middleware rejects any request that carries neither the operator credential
// nor a valid session, and attaches the caller's identity to the ones it
// forwards.
//
// Both credential forms are accepted because both are needed: scripts and curl
// send the header, while the UI's EventSource log tail and its download links
// cannot set headers at all and can only authenticate with a cookie.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.identify(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ci-platform"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "unauthorized",
				"message": "this endpoint requires an operator: sign in with GitHub at " + PathGitHubLogin +
					", or send the operator credential as an Authorization: Bearer header",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, id)))
	})
}

// Authenticated reports whether a request carries a credential. Exported so a
// handler that serves both a public and a privileged view can ask.
func (a *Auth) Authenticated(r *http.Request) bool {
	_, ok := a.identify(r)
	return ok
}

// identify resolves a request to an operator, preferring the explicit header
// over an ambient cookie.
func (a *Auth) identify(r *http.Request) (Identity, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, value, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			if a.matches(strings.TrimSpace(value)) {
				return Identity{Method: MethodToken}, true
			}
		}
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return Identity{}, false
	}
	id, err := a.sessions.parse(c.Value)
	if err != nil {
		return Identity{}, false
	}
	// An account dropped from the admin list loses the dashboard on its next
	// request, rather than keeping it until the session happens to expire.
	if id.Method == MethodGitHub && !a.admins.Contains(id.Login) {
		a.log.Warn("rejected a session for an account no longer on the admin list", "login", id.Login)
		return Identity{}, false
	}
	return id, true
}

// matches compares in constant time so a wrong credential leaks nothing about
// how much of it was right.
func (a *Auth) matches(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), a.token) == 1
}

func (a *Auth) setSession(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		// Lax, not Strict: the browser arrives back from github.com after a
		// sign-in, and a Strict cookie is not sent on a cross-site navigation,
		// so the dashboard would render signed-out immediately after signing
		// in. Lax still withholds the cookie from every cross-site POST, which
		// is what the API's mutations are.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(a.sessions.ttl / time.Second),
	})
}

// login exchanges the shared operator credential for a session. It stays
// because a script, a curl one-liner, and a browser on a machine that cannot
// reach github.com all need a way in that does not involve an OAuth round trip.
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	// A JSON body is what the UI sends; a form body is what a curl one-liner
	// reaches for. Rejecting either would be a papercut with no upside.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "malformed form body: " + err.Error()})
			return
		}
		body.Token = r.PostFormValue("token")
	} else if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "malformed JSON body: " + err.Error()})
		return
	}

	if !a.matches(strings.TrimSpace(body.Token)) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "unauthorized", "message": "that is not the operator credential",
		})
		return
	}
	value, _ := a.sessions.mint("", MethodToken)
	a.setSession(w, value)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "method": MethodToken})
}

func (a *Auth) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// status lets the UI show a sign-in button without first provoking a 401 on a
// data endpoint. It answers 200 either way: whether somebody is signed in is
// not a secret, and a 401 here would be indistinguishable from a broken server.
func (a *Auth) status(w http.ResponseWriter, r *http.Request) {
	id, ok := a.identify(r)
	body := map[string]any{
		"authenticated": ok,
		"login":         id.Login,
		"method":        string(id.Method),
		"sign_in_url":   PathGitHubLogin,
	}
	if ok && !id.Expires.IsZero() {
		body["expires"] = id.Expires.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, body)
}

// fail answers a browser-facing route. These are reached by navigation, not by
// fetch, so the message has to be readable in a window on its own.
func (a *Auth) fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, message)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"message":"failed to encode response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
