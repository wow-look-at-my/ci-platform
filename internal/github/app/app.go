// Package app is the GitHub App authentication path: JWT minting, installation
// tokens, and the installation listing the registration flow needs.
//
// Check runs cannot be written with a PAT, so this is the only credential path
// that makes branch protection work. A missing or malformed key fails at
// startup by name; there is no unconfigured mode that idles green.
package app

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
)

// jwtTTL is the App JWT lifetime. GitHub rejects anything over 10 minutes.
const jwtTTL = 9 * time.Minute

// clockSkew backdates iat so a control-plane clock a minute fast still mints
// tokens GitHub accepts.
const clockSkew = 60 * time.Second

// refreshWindow is how much remaining life makes a cached installation token
// too stale to hand out.
const refreshWindow = 5 * time.Minute

// Config is the App's startup configuration. Exactly one of PrivateKeyPEM and
// PrivateKeyPath must be set.
type Config struct {
	AppID          int64
	PrivateKeyPEM  string
	PrivateKeyPath string
	BaseURL        string
	HTTPClient     *http.Client
	// Now is injectable for tests.
	Now func() time.Time
	// Sleep is injectable for tests, and is passed through to the REST client.
	Sleep func(context.Context, time.Duration) error
}

// App mints App JWTs and installation tokens.
type App struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
	BaseURL    string
	HTTPClient *http.Client

	now func() time.Time
	cli *gh.Client

	mu    sync.Mutex
	cache map[string]*cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// LoadApp parses the private key and prepares the App. Every failure names the
// config field that produced it.
func LoadApp(cfg Config) (*App, error) {
	if cfg.AppID <= 0 {
		return nil, fmt.Errorf("github app: config field AppID is %d; set it to the App's numeric id", cfg.AppID)
	}
	if cfg.PrivateKeyPEM == "" && cfg.PrivateKeyPath == "" {
		return nil, errors.New("github app: config fields PrivateKeyPEM and PrivateKeyPath are both empty; one must hold the App's RSA private key")
	}
	if cfg.PrivateKeyPEM != "" && cfg.PrivateKeyPath != "" {
		return nil, errors.New("github app: config fields PrivateKeyPEM and PrivateKeyPath are both set; use exactly one")
	}
	pem := []byte(cfg.PrivateKeyPEM)
	source := "PrivateKeyPEM"
	if cfg.PrivateKeyPath != "" {
		source = "PrivateKeyPath"
		b, err := os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("github app: config field PrivateKeyPath %q: %w", cfg.PrivateKeyPath, err)
		}
		pem = b
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("github app: config field %s does not hold a PEM RSA private key: %w", source, err)
	}

	a := &App{
		AppID:      cfg.AppID,
		PrivateKey: key,
		BaseURL:    cfg.BaseURL,
		HTTPClient: cfg.HTTPClient,
		now:        cfg.Now,
		cache:      map[string]*cachedToken{},
	}
	if a.BaseURL == "" {
		a.BaseURL = gh.DefaultBaseURL
	}
	if a.now == nil {
		a.now = time.Now
	}
	cli, err := gh.NewClient(gh.Options{
		BaseURL:    a.BaseURL,
		Tokens:     gh.TokenSourceFunc(func(context.Context) (string, error) { return a.JWT() }),
		HTTPClient: cfg.HTTPClient,
		Now:        a.now,
		Sleep:      cfg.Sleep,
	})
	if err != nil {
		return nil, fmt.Errorf("github app: building REST client: %w", err)
	}
	a.cli = cli
	return a, nil
}

// JWT mints an App JWT: RS256, iat backdated for clock skew, exp under the
// 10-minute ceiling, iss the App id.
func (a *App) JWT() (string, error) {
	return a.jwtAt(a.now())
}

func (a *App) jwtAt(t time.Time) (string, error) {
	if a.PrivateKey == nil {
		return "", errors.New("github app: no private key loaded; LoadApp was bypassed")
	}
	claims := jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(a.AppID, 10),
		IssuedAt:  jwt.NewNumericDate(t.Add(-clockSkew)),
		ExpiresAt: jwt.NewNumericDate(t.Add(jwtTTL)),
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("github app: signing App JWT: %w", err)
	}
	return s, nil
}

// TokenScope narrows an installation token. Jobs never receive one of these;
// the platform mints them for its own calls, as narrowly as it can.
type TokenScope struct {
	RepositoryIDs []int64
	Repositories  []string
	Permissions   map[string]string
}

func (s TokenScope) empty() bool {
	return len(s.RepositoryIDs) == 0 && len(s.Repositories) == 0 && len(s.Permissions) == 0
}

// key is a stable cache key for the scope.
func (s TokenScope) key() string {
	if s.empty() {
		return "*"
	}
	ids := append([]int64(nil), s.RepositoryIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	repos := append([]string(nil), s.Repositories...)
	sort.Strings(repos)
	perms := make([]string, 0, len(s.Permissions))
	for k, v := range s.Permissions {
		perms = append(perms, k+"="+v)
	}
	sort.Strings(perms)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "i%d;", id)
	}
	b.WriteString(strings.Join(repos, ",") + "|" + strings.Join(perms, ","))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

type accessTokenRequest struct {
	RepositoryIDs []int64           `json:"repository_ids,omitempty"`
	Repositories  []string          `json:"repositories,omitempty"`
	Permissions   map[string]string `json:"permissions,omitempty"`
}

type accessTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// InstallationToken returns a cached, unscoped installation token, refreshing
// it once under five minutes of life remain.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, time.Time, error) {
	return a.ScopedInstallationToken(ctx, installationID, TokenScope{})
}

// ScopedInstallationToken returns a token limited to the given repositories and
// permissions. Each distinct scope is cached separately.
func (a *App) ScopedInstallationToken(ctx context.Context, installationID int64, scope TokenScope) (string, time.Time, error) {
	if installationID <= 0 {
		return "", time.Time{}, fmt.Errorf("github app: installation id %d is not valid", installationID)
	}
	ck := strconv.FormatInt(installationID, 10) + "/" + scope.key()

	a.mu.Lock()
	if c, ok := a.cache[ck]; ok && a.now().Add(refreshWindow).Before(c.expiresAt) {
		tok, exp := c.token, c.expiresAt
		a.mu.Unlock()
		return tok, exp, nil
	}
	a.mu.Unlock()

	var body any
	if !scope.empty() {
		body = accessTokenRequest{
			RepositoryIDs: scope.RepositoryIDs,
			Repositories:  scope.Repositories,
			Permissions:   scope.Permissions,
		}
	}
	var out accessTokenResponse
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if _, err := a.cli.Post(ctx, path, body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github app: minting installation token for %d: %w", installationID, err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github app: installation %d returned an empty token", installationID)
	}
	if out.ExpiresAt.IsZero() {
		return "", time.Time{}, fmt.Errorf("github app: installation %d returned a token with no expiry", installationID)
	}

	a.mu.Lock()
	a.cache[ck] = &cachedToken{token: out.Token, expiresAt: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, out.ExpiresAt, nil
}

// TokenSource yields installation tokens for a REST client.
func (a *App) TokenSource(installationID int64, scope TokenScope) gh.TokenSource {
	return gh.TokenSourceFunc(func(ctx context.Context) (string, error) {
		tok, _, err := a.ScopedInstallationToken(ctx, installationID, scope)
		return tok, err
	})
}

// InstallationClient is a REST client authenticated as one installation.
func (a *App) InstallationClient(installationID int64, scope TokenScope) (*gh.Client, error) {
	return gh.NewClient(gh.Options{
		BaseURL:    a.BaseURL,
		Tokens:     a.TokenSource(installationID, scope),
		HTTPClient: a.HTTPClient,
		Now:        a.now,
	})
}

// Account is the user or org an App is installed on.
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Installation is one App installation.
type Installation struct {
	ID                  int64             `json:"id"`
	Account             Account           `json:"account"`
	AppID               int64             `json:"app_id"`
	TargetType          string            `json:"target_type"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	Events              []string          `json:"events"`
	SuspendedAt         *time.Time        `json:"suspended_at,omitempty"`
}

// Suspended reports whether GitHub has suspended this installation, which means
// every call made with its token will fail.
func (i Installation) Suspended() bool { return i.SuspendedAt != nil }

// Repository is one repo an installation covers.
type Repository struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Private       bool    `json:"private"`
	DefaultBranch string  `json:"default_branch"`
	Owner         Account `json:"owner"`
}

// Installations lists every installation of this App, following pagination.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	err := a.cli.Paginate(ctx, "/app/installations?per_page=100", func(page []byte) error {
		var batch []Installation
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("github app: decoding installations page: %w", err)
		}
		all = append(all, batch...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// InstallationForRepo resolves the installation covering a repository, which is
// how a webhook without an installation id is mapped onto a token.
func (a *App) InstallationForRepo(ctx context.Context, repo gh.Repo) (*Installation, error) {
	if !repo.Valid() {
		return nil, fmt.Errorf("github app: InstallationForRepo needs owner and name, got %q", repo)
	}
	var out Installation
	path := fmt.Sprintf("/repos/%s/%s/installation", urlEscape(repo.Owner), urlEscape(repo.Name))
	if _, err := a.cli.Get(ctx, path, &out); err != nil {
		return nil, fmt.Errorf("github app: resolving installation for %s: %w", repo, err)
	}
	return &out, nil
}

// InstallationRepositories lists the repositories an installation covers, using
// that installation's own token.
func (a *App) InstallationRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	cli, err := a.InstallationClient(installationID, TokenScope{})
	if err != nil {
		return nil, err
	}
	var all []Repository
	err = cli.Paginate(ctx, "/installation/repositories?per_page=100", func(page []byte) error {
		var body struct {
			Repositories []Repository `json:"repositories"`
		}
		if err := json.Unmarshal(page, &body); err != nil {
			return fmt.Errorf("github app: decoding installation repositories page: %w", err)
		}
		all = append(all, body.Repositories...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("github app: listing repositories for installation %d: %w", installationID, err)
	}
	return all, nil
}

func urlEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "?", "%3F"), "#", "%23")
}
