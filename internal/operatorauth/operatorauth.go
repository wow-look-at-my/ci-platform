// Package operatorauth gates the operator surface -- the REST API, the SSE
// tails, and the raw log and artifact downloads -- behind a shared operator
// credential.
//
// It exists because every job container can route to the control plane: a job
// needs CIPLATFORM_PUBLIC_URL to upload artifacts, restore cache, and fetch an
// ID token. An unauthenticated /api/v1 on that same listener is therefore
// reachable from inside any workflow, including a fork PR's, which makes every
// other repository's logs and artifacts readable and every run cancellable by
// anything that can run a step.
//
// see docs/security.md
package operatorauth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CookieName holds the operator credential in a browser. It is HttpOnly and
// SameSite=Strict: the UI never reads it from script, and a page on another
// origin cannot make the browser send it along with a cancel.
const CookieName = "ci_operator"

// MinTokenLen is the shortest credential accepted. The token is a bearer
// secret with no lockout in front of it, so its entropy is the only thing
// standing between an attacker and the API.
const MinTokenLen = 16

// PathLogin, PathLogout and PathStatus are the three unauthenticated endpoints
// the UI needs before it holds a session.
const (
	PathLogin  = "/auth/login"
	PathLogout = "/auth/logout"
	PathStatus = "/auth/status"
)

// Options configures the gate.
type Options struct {
	// Token is the operator credential. Required.
	Token string
	// SessionTTL bounds how long a browser session lasts. Default 12h.
	SessionTTL time.Duration
	// Secure marks the cookie Secure. Set from the public URL's scheme: a
	// Secure cookie is never sent over plain http, so forcing it on an http
	// deployment silently logs every operator out on the next request.
	Secure bool
}

// Auth is the middleware and its login endpoints.
type Auth struct {
	token  []byte
	ttl    time.Duration
	secure bool
	mux    *http.ServeMux
}

// New builds the gate. An empty or too-short token is an error rather than an
// open door: a gate that lets everybody through is worse than no gate, because
// the deployment looks protected.
func New(opts Options) (*Auth, error) {
	if opts.Token == "" {
		return nil, errors.New("operatorauth: token is required")
	}
	if len(opts.Token) < MinTokenLen {
		return nil, fmt.Errorf("operatorauth: token is %d characters; at least %d are required, "+
			"because it is the only thing protecting every repository's logs and artifacts",
			len(opts.Token), MinTokenLen)
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 12 * time.Hour
	}
	a := &Auth{token: []byte(opts.Token), ttl: opts.SessionTTL, secure: opts.Secure}
	a.mux = http.NewServeMux()
	a.mux.HandleFunc("POST "+PathLogin, a.login)
	a.mux.HandleFunc("POST "+PathLogout, a.logout)
	a.mux.HandleFunc("GET "+PathStatus, a.status)
	return a, nil
}

// Handler serves the login, logout, and status endpoints. These are the only
// operator-surface routes that answer without a credential.
func (a *Auth) Handler() http.Handler { return a.mux }

// Middleware rejects any request that does not carry the operator credential,
// as either an Authorization: Bearer header or the session cookie.
//
// Both forms are accepted because both are needed: scripts and curl send the
// header, while the UI's EventSource log tail and its download links cannot set
// headers at all and can only authenticate with a cookie.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ci-platform"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "unauthorized",
				"message": "this endpoint requires the operator credential: send it as an Authorization: Bearer header, or sign in at " + PathLogin,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Authenticated reports whether a request carries the credential. Exported so
// a handler that serves both a public and a privileged view can ask.
func (a *Auth) Authenticated(r *http.Request) bool { return a.authenticated(r) }

func (a *Auth) authenticated(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, value, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			if a.matches(strings.TrimSpace(value)) {
				return true
			}
		}
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return a.matches(c.Value)
	}
	return false
}

// matches compares in constant time so a wrong credential leaks nothing about
// how much of it was right.
func (a *Auth) matches(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), a.token) == 1
}

// login exchanges the operator credential for a session cookie. The cookie
// value is the credential itself: with a single shared operator identity there
// is no per-session state worth keeping, and inventing a session table would
// buy nothing a rotated token does not already do.
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
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    string(a.token),
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.ttl / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (a *Auth) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// status lets the UI show a sign-in form without first provoking a 401 on a
// data endpoint. It answers 200 either way: whether somebody is signed in is
// not a secret, and a 401 here would be indistinguishable from a broken server.
func (a *Auth) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": a.authenticated(r)})
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
