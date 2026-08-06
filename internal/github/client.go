// Package github is the platform's GitHub REST surface: a small client, the App
// authentication path, webhook ingest, and the two status reporters.
//
// Check runs can only be written with a GitHub App installation token, so the
// App path is load-bearing rather than optional. Nothing here has a
// "not configured yet" mode: a missing credential fails every call by name.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// DefaultBaseURL is api.github.com. GHES installs override it.
const DefaultBaseURL = "https://api.github.com"

// DefaultUserAgent identifies the control plane to GitHub.
const DefaultUserAgent = "ci-platform/1.0"

var (
	// ErrNotFound is returned for a 404.
	ErrNotFound = errors.New("github: not found")
	// ErrRateLimited is returned once retries are exhausted against a rate
	// limit. It is never swallowed: an update that could not be delivered is
	// reported, not dropped.
	ErrRateLimited = errors.New("github: rate limit exhausted")
	// ErrNoToken is returned when no credential is configured.
	ErrNoToken = errors.New("github: no token source configured")
)

// Repo is the owner/name pair every REST path is built from.
type Repo struct {
	Owner string
	Name  string
}

// RepoOf projects the persisted repo onto the pair the REST paths need.
func RepoOf(r model.Repo) Repo { return Repo{Owner: r.Owner, Name: r.Name} }

// String renders "owner/name".
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Valid reports whether both halves are present.
func (r Repo) Valid() bool { return r.Owner != "" && r.Name != "" }

func (r Repo) path() string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// TokenSource yields the credential for the next request. It is called per
// request so an installation token can be refreshed transparently.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Token implements TokenSource.
func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// StaticToken is a fixed credential, for tests and for GHES PAT deployments
// that do not use the Checks API.
type StaticToken string

// Token implements TokenSource. An empty StaticToken is a configuration error,
// not an anonymous request.
func (s StaticToken) Token(context.Context) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: StaticToken is empty", ErrNoToken)
	}
	return string(s), nil
}

// RateLimit is the last rate-limit state GitHub reported.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
	Resource  string    `json:"resource,omitempty"`
	// RetryAfter is the Retry-After header, set on secondary limits.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	Observed   time.Time     `json:"observed"`
}

// Exhausted reports whether the primary limit is spent.
func (r RateLimit) Exhausted() bool { return r.Limit > 0 && r.Remaining <= 0 }

// APIError is a non-2xx response, carrying enough to classify it.
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	Message    string
	DocURL     string
	Body       string
	RateLimit  RateLimit
	Errors     []APIErrorDetail
}

// APIErrorDetail is one entry of GitHub's "errors" array.
type APIErrorDetail struct {
	Resource string `json:"resource,omitempty"`
	Field    string `json:"field,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("github: %s %s: HTTP %d", e.Method, e.URL, e.StatusCode)
	if e.Message != "" {
		s += ": " + e.Message
	}
	for _, d := range e.Errors {
		s += fmt.Sprintf(" [%s.%s %s %s]", d.Resource, d.Field, d.Code, d.Message)
	}
	return s
}

// Is lets callers match ErrNotFound and ErrRateLimited against an APIError.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrRateLimited:
		return e.rateLimited()
	}
	return false
}

func (e *APIError) rateLimited() bool {
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if e.StatusCode != http.StatusForbidden {
		return false
	}
	if e.RateLimit.Exhausted() || e.RateLimit.RetryAfter > 0 {
		return true
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "rate limit") || strings.Contains(m, "abuse detection") ||
		strings.Contains(m, "secondary rate")
}

// Retryable reports whether another attempt could plausibly succeed.
func (e *APIError) Retryable() bool {
	return e.StatusCode >= 500 || e.rateLimited()
}

// Decision classifies the error so a caller can decide whose fault it was.
func (e *APIError) Decision() classify.Decision {
	var c classify.Classifier
	return c.Classify(classify.Signal{
		HTTPStatus: e.StatusCode,
		Output:     e.Message,
		Host:       hostOf(e.URL),
		Err:        errors.New(e.Error()),
	})
}

// FailureClass is the classified owner of this failure.
func (e *APIError) FailureClass() model.FailureClass { return e.Decision().Class }

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// Options configures a Client. BaseURL and Tokens are the only required
// fields, and a missing Tokens is a startup error rather than an anonymous
// client that 403s later.
type Options struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client
	UserAgent  string
	// MaxRetries is the number of retries after the first attempt. Zero means
	// the default of 3; a negative value disables retrying entirely.
	MaxRetries  int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Logger      *slog.Logger
	Now         func() time.Time
	// Sleep is injectable so tests do not wait out a backoff.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRateLimit observes every rate-limit header set GitHub returns.
	OnRateLimit func(RateLimit)
}

// Client is a minimal GitHub REST client.
type Client struct {
	baseURL     *url.URL
	tokens      TokenSource
	hc          *http.Client
	ua          string
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	log         *slog.Logger
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	onRateLimit func(RateLimit)

	mu   sync.Mutex
	rate RateLimit
}

// NewClient builds a client. A nil or empty token source is rejected here so
// the failure names the missing configuration instead of surfacing as a 401 on
// the first check-run write.
func NewClient(opts Options) (*Client, error) {
	if opts.Tokens == nil {
		return nil, fmt.Errorf("%w: Options.Tokens is nil (set the GitHub App installation token source)", ErrNoToken)
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil {
		return nil, fmt.Errorf("github: Options.BaseURL %q is not a URL: %w", opts.BaseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("github: Options.BaseURL %q needs a scheme and host", base)
	}
	c := &Client{
		baseURL:     u,
		tokens:      opts.Tokens,
		ua:          orString(opts.UserAgent, DefaultUserAgent),
		maxRetries:  opts.MaxRetries,
		baseBackoff: opts.BaseBackoff,
		maxBackoff:  opts.MaxBackoff,
		log:         opts.Logger,
		now:         opts.Now,
		sleep:       opts.Sleep,
		onRateLimit: opts.OnRateLimit,
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = 3
	case c.maxRetries < 0:
		c.maxRetries = 0
	}
	if c.baseBackoff == 0 {
		c.baseBackoff = 500 * time.Millisecond
	}
	if c.maxBackoff == 0 {
		c.maxBackoff = 30 * time.Second
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Clone rather than mutate the caller's client: the transport wrapper is
	// ours, the client is not.
	cp := *hc
	cp.Transport = &rateTransport{base: cp.Transport, record: c.recordRate}
	c.hc = &cp
	return c, nil
}

func orString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rateTransport records rate-limit headers off every response.
type rateTransport struct {
	base   http.RoundTripper
	record func(RateLimit)
}

func (t *rateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if resp != nil && t.record != nil {
		t.record(parseRateLimit(resp.Header, time.Now()))
	}
	return resp, err
}

func parseRateLimit(h http.Header, at time.Time) RateLimit {
	rl := RateLimit{Observed: at, Resource: h.Get("X-RateLimit-Resource")}
	rl.Limit = atoiOr(h.Get("X-RateLimit-Limit"), 0)
	rl.Used = atoiOr(h.Get("X-RateLimit-Used"), 0)
	if v := h.Get("X-RateLimit-Remaining"); v != "" {
		rl.Remaining = atoiOr(v, 0)
	} else {
		rl.Remaining = -1
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Reset = time.Unix(secs, 0).UTC()
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			rl.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return rl
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (c *Client) recordRate(rl RateLimit) {
	c.mu.Lock()
	// A response with no rate headers must not erase the last known state.
	if rl.Limit > 0 || rl.Remaining >= 0 {
		c.rate = rl
	}
	c.mu.Unlock()
	if c.onRateLimit != nil && (rl.Limit > 0 || rl.Remaining >= 0) {
		c.onRateLimit(rl)
	}
}

// RateLimit returns the last observed rate-limit state.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate
}

// Response is one completed HTTP exchange.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	RateLimit  RateLimit
	// NextPage is the rel="next" link target, "" on the last page.
	NextPage string
}

// Do performs one request with retry on 5xx, 429, and secondary rate limits.
// out, when non-nil, receives the decoded JSON body.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) (*Response, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("github: encoding %s %s body: %w", method, path, err)
		}
	}
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = c.baseURL.String() + path
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, method, target, payload, out)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var ae *APIError
		retryable := errors.As(err, &ae) && ae.Retryable()
		if !retryable && !isTransport(err) {
			return nil, err
		}
		if attempt >= c.maxRetries {
			break
		}
		wait := c.backoff(attempt, ae)
		c.log.Warn("github request failed, retrying",
			"method", method, "url", target, "attempt", attempt+1,
			"max_attempts", c.maxRetries+1, "wait", wait, "err", err)
		if serr := c.sleep(ctx, wait); serr != nil {
			return nil, errors.Join(err, serr)
		}
	}
	var ae *APIError
	if errors.As(lastErr, &ae) && ae.rateLimited() {
		// Loud on purpose: an update lost to a rate limit is the failure this
		// platform must never hide.
		c.log.Error("github rate limit exhausted after retries",
			"method", method, "url", target, "remaining", ae.RateLimit.Remaining,
			"reset", ae.RateLimit.Reset, "retry_after", ae.RateLimit.RetryAfter)
		return nil, fmt.Errorf("%w: %s %s gave up after %d attempts: %w",
			ErrRateLimited, method, target, c.maxRetries+1, lastErr)
	}
	return nil, fmt.Errorf("github: %s %s failed after %d attempts: %w", method, target, c.maxRetries+1, lastErr)
}

// isTransport reports whether an error is a network fault worth another
// attempt. A bad credential, a cancelled context, and an undecodable body are
// all permanent: retrying them only delays the report.
func isTransport(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return false
	}
	if errors.Is(err, ErrNoToken) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return !errors.As(err, &syn) && !errors.As(err, &typ)
}

// backoff is exponential, capped, and overridden by Retry-After or the
// rate-limit reset when GitHub named a wait itself.
func (c *Client) backoff(attempt int, ae *APIError) time.Duration {
	d := c.baseBackoff << attempt
	if d > c.maxBackoff || d <= 0 {
		d = c.maxBackoff
	}
	if ae == nil {
		return d
	}
	if ae.RateLimit.RetryAfter > 0 {
		return minDur(ae.RateLimit.RetryAfter, c.maxBackoff)
	}
	if ae.rateLimited() && !ae.RateLimit.Reset.IsZero() {
		if until := ae.RateLimit.Reset.Sub(c.now()); until > 0 {
			return minDur(until, c.maxBackoff)
		}
	}
	return d
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (c *Client) once(ctx context.Context, method, target string, payload []byte, out any) (*Response, error) {
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, target, err)
	}
	if tok == "" {
		return nil, fmt.Errorf("%w: token source returned an empty token for %s %s", ErrNoToken, method, target)
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("github: building %s %s: %w", method, target, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Authorization", "Bearer "+tok)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hresp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, target, err)
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("github: reading %s %s body: %w", method, target, err)
	}
	rl := parseRateLimit(hresp.Header, c.now())
	resp := &Response{
		StatusCode: hresp.StatusCode,
		Header:     hresp.Header,
		Body:       raw,
		RateLimit:  rl,
		NextPage:   nextLink(hresp.Header.Get("Link")),
	}
	if hresp.StatusCode >= 400 {
		return nil, newAPIError(method, target, resp)
	}
	if out != nil && len(raw) > 0 && hresp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("github: decoding %s %s response: %w", method, target, err)
		}
	}
	return resp, nil
}

func newAPIError(method, target string, resp *Response) *APIError {
	ae := &APIError{
		StatusCode: resp.StatusCode,
		Method:     method,
		URL:        target,
		Body:       string(resp.Body),
		RateLimit:  resp.RateLimit,
	}
	var payload struct {
		Message string           `json:"message"`
		DocURL  string           `json:"documentation_url"`
		Errors  []APIErrorDetail `json:"errors"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err == nil {
		ae.Message, ae.DocURL, ae.Errors = payload.Message, payload.DocURL, payload.Errors
	}
	return ae
}

// nextLink extracts rel="next" from a Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(strings.TrimSpace(part), ";")
		if len(seg) < 2 {
			continue
		}
		raw := strings.TrimSpace(seg[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}
		for _, p := range seg[1:] {
			if strings.EqualFold(strings.TrimSpace(p), `rel="next"`) {
				return raw[1 : len(raw)-1]
			}
		}
	}
	return ""
}

// Get performs a GET and decodes into out.
func (c *Client) Get(ctx context.Context, path string, out any) (*Response, error) {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body, out any) (*Response, error) {
	return c.Do(ctx, http.MethodPost, path, body, out)
}

// Patch performs a PATCH with a JSON body.
func (c *Client) Patch(ctx context.Context, path string, body, out any) (*Response, error) {
	return c.Do(ctx, http.MethodPatch, path, body, out)
}

// Paginate walks rel="next" links, handing each page's raw body to fn.
func (c *Client) Paginate(ctx context.Context, path string, fn func(page []byte) error) error {
	const maxPages = 100
	next := path
	for i := 0; next != "" && i < maxPages; i++ {
		resp, err := c.Do(ctx, http.MethodGet, next, nil, nil)
		if err != nil {
			return err
		}
		if err := fn(resp.Body); err != nil {
			return err
		}
		next = resp.NextPage
	}
	return nil
}
