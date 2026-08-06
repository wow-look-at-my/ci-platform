package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// testClient wires a client at a test server with instant, recorded sleeps.
func testClient(t *testing.T, srv *httptest.Server, mutate ...func(*Options)) (*Client, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	opts := Options{
		BaseURL: srv.URL,
		Tokens:  StaticToken("tok"),
		Sleep: func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := NewClient(opts)
	require.NoError(t, err)
	return c, &slept
}

func TestNewClientRejectsMissingCredential(t *testing.T) {
	_, err := NewClient(Options{BaseURL: "https://api.github.com"})
	require.ErrorIs(t, err, ErrNoToken)
	assert.Contains(t, err.Error(), "Options.Tokens")
}

func TestNewClientRejectsBadBaseURL(t *testing.T) {
	_, err := NewClient(Options{BaseURL: "not-a-url", Tokens: StaticToken("t")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scheme and host")

	_, err = NewClient(Options{BaseURL: "://bad", Tokens: StaticToken("t")})
	require.Error(t, err)
}

func TestStaticTokenEmptyIsAConfigError(t *testing.T) {
	_, err := StaticToken("").Token(context.Background())
	require.ErrorIs(t, err, ErrNoToken)
}

func TestClientSendsAuthAndAPIHeaders(t *testing.T) {
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = readAll(r)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4998")
		w.Header().Set("X-RateLimit-Reset", "1700000600")
		w.Header().Set("X-RateLimit-Resource", "core")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	var out struct {
		OK bool `json:"ok"`
	}
	resp, err := c.Post(context.Background(), "/thing", map[string]string{"a": "b"}, &out)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Equal(t, "Bearer tok", got.Get("Authorization"))
	assert.Equal(t, "application/vnd.github+json", got.Get("Accept"))
	assert.Equal(t, "2022-11-28", got.Get("X-GitHub-Api-Version"))
	assert.Equal(t, DefaultUserAgent, got.Get("User-Agent"))
	assert.JSONEq(t, `{"a":"b"}`, string(body))

	assert.Equal(t, 4998, resp.RateLimit.Remaining)
	assert.Equal(t, 5000, resp.RateLimit.Limit)
	assert.Equal(t, "core", resp.RateLimit.Resource)
	// The RoundTripper records it on the client too.
	assert.Equal(t, 4998, c.RateLimit().Remaining)
	assert.Equal(t, time.Unix(1700000600, 0).UTC(), c.RateLimit().Reset)
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func TestClientRetriesFiveHundredThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"upstream"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()

	c, slept := testClient(t, srv)
	var out struct {
		ID int `json:"id"`
	}
	_, err := c.Get(context.Background(), "/x", &out)
	require.NoError(t, err)
	assert.Equal(t, 7, out.ID)
	assert.EqualValues(t, 3, calls.Load())
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second}, *slept)
}

func TestClientHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, slept := testClient(t, srv)
	_, err := c.Get(context.Background(), "/x", nil)
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{7 * time.Second}, *slept)
}

func TestClientSurfacesRateLimitExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Unix(1_700_000_030, 0).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	c, slept := testClient(t, srv, func(o *Options) { o.MaxRetries = 1 })
	_, err := c.Get(context.Background(), "/x", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRateLimited)
	// The reset time, not the exponential default.
	assert.Equal(t, []time.Duration{30 * time.Second}, *slept)
}

func TestClientDoesNotRetryFourOhFour(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	_, err := c.Get(context.Background(), "/x", nil)
	require.ErrorIs(t, err, ErrNotFound)
	assert.EqualValues(t, 1, calls.Load())

	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, "Not Found", ae.Message)
	assert.Contains(t, ae.Error(), "HTTP 404")
	assert.False(t, ae.Retryable())
}

func TestAPIErrorClassification(t *testing.T) {
	infra := &APIError{StatusCode: 503, Method: "POST", URL: "https://api.github.com/x", Message: "unavailable"}
	assert.Equal(t, model.ClassInfra, infra.FailureClass())
	assert.True(t, infra.Retryable())

	limited := &APIError{StatusCode: 429, Message: "too many"}
	assert.Equal(t, model.ClassInfra, limited.FailureClass())
	assert.True(t, errors.Is(limited, ErrRateLimited))

	user := &APIError{StatusCode: 422, Message: "Validation Failed",
		Errors: []APIErrorDetail{{Resource: "CheckRun", Field: "conclusion", Code: "invalid"}}}
	assert.Equal(t, model.ClassUser, user.FailureClass())
	assert.Contains(t, user.Error(), "CheckRun.conclusion")
	assert.False(t, errors.Is(user, ErrNotFound))
}

func TestClientDoesNotRetryABadCredential(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()

	c, err := NewClient(Options{
		BaseURL: srv.URL,
		Tokens:  TokenSourceFunc(func(context.Context) (string, error) { return "", fmt.Errorf("%w: vault down", ErrNoToken) }),
	})
	require.NoError(t, err)
	_, err = c.Get(context.Background(), "/x", nil)
	require.ErrorIs(t, err, ErrNoToken)
	assert.EqualValues(t, 0, calls.Load())
}

func TestClientRejectsEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c, err := NewClient(Options{
		BaseURL: srv.URL,
		Tokens:  TokenSourceFunc(func(context.Context) (string, error) { return "", nil }),
	})
	require.NoError(t, err)
	_, err = c.Get(context.Background(), "/x", nil)
	require.ErrorIs(t, err, ErrNoToken)
}

func TestPaginateFollowsNextLinks(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next", <%s/items?page=2>; rel="last"`, srv.URL, srv.URL))
			_, _ = w.Write([]byte(`[1,2]`))
		default:
			_, _ = w.Write([]byte(`[3]`))
		}
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	var seen []int
	err := c.Paginate(context.Background(), "/items", func(page []byte) error {
		var batch []int
		require.NoError(t, json.Unmarshal(page, &batch))
		seen = append(seen, batch...)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, seen)
}

func TestPaginateSurfacesCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)
	boom := errors.New("boom")
	err := c.Paginate(context.Background(), "/items", func([]byte) error { return boom })
	require.ErrorIs(t, err, boom)
}

func TestNextLink(t *testing.T) {
	assert.Equal(t, "https://a/2", nextLink(`<https://a/2>; rel="next", <https://a/9>; rel="last"`))
	assert.Equal(t, "", nextLink(`<https://a/9>; rel="last"`))
	assert.Equal(t, "", nextLink(""))
	assert.Equal(t, "", nextLink(`garbage; rel="next"`))
}

func TestCreateDeploymentStatus(t *testing.T) {
	var path string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ = readAll(r)
		_, _ = w.Write([]byte(`{"id":42,"state":"success"}`))
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	out, err := c.CreateDeploymentStatus(context.Background(), Repo{"wow-look-at-my", "ci-platform"}, 9,
		DeploymentStatusRequest{State: "success", Description: "deployed", LogURL: "https://ci/1", Environment: "prod"})
	require.NoError(t, err)
	assert.Equal(t, int64(42), out.ID)
	assert.Equal(t, "/repos/wow-look-at-my/ci-platform/deployments/9/statuses", path)
	assert.JSONEq(t, `{"state":"success","description":"deployed","log_url":"https://ci/1","environment":"prod"}`, string(body))
}

func TestCreateDeploymentStatusValidates(t *testing.T) {
	c, _ := testClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	_, err := c.CreateDeploymentStatus(context.Background(), Repo{}, 1, DeploymentStatusRequest{State: "x"})
	require.Error(t, err)
	_, err = c.CreateDeploymentStatus(context.Background(), Repo{"a", "b"}, 0, DeploymentStatusRequest{State: "x"})
	require.Error(t, err)
	_, err = c.CreateDeploymentStatus(context.Background(), Repo{"a", "b"}, 1, DeploymentStatusRequest{})
	require.Error(t, err)
}

func TestGetFileContents(t *testing.T) {
	want := "name: CI\non: push\n"
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		assert.Equal(t, "/repos/o/r/contents/.github/workflows/ci.yml", r.URL.Path)
		_ = json.NewEncoder(w).Encode(contentEntry{
			Type: "file", Path: ".github/workflows/ci.yml", SHA: "abc", Size: int64(len(want)),
			Content: base64.StdEncoding.EncodeToString([]byte(want)) + "\n", Encoding: "base64",
		})
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	f, err := c.GetFileContents(context.Background(), Repo{"o", "r"}, ".github/workflows/ci.yml", "feature/x")
	require.NoError(t, err)
	assert.Equal(t, want, string(f.Content))
	assert.Equal(t, "abc", f.SHA)
	assert.Equal(t, "ref=feature%2Fx", query)
}

func TestGetFileContentsRejectsDirAndOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/contents/dir" {
			_ = json.NewEncoder(w).Encode(contentEntry{Type: "dir"})
			return
		}
		_ = json.NewEncoder(w).Encode(contentEntry{Type: "file", Encoding: "none"})
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	_, err := c.GetFileContents(context.Background(), Repo{"o", "r"}, "dir", "")
	require.ErrorContains(t, err, "is a directory")

	_, err = c.GetFileContents(context.Background(), Repo{"o", "r"}, "big", "")
	require.ErrorContains(t, err, "encoding")

	_, err = c.GetFileContents(context.Background(), Repo{"o", ""}, "x", "")
	require.Error(t, err)
	_, err = c.GetFileContents(context.Background(), Repo{"o", "r"}, "", "")
	require.Error(t, err)
}

func TestGetFileContentsRejectsBadBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(contentEntry{Type: "file", Encoding: "base64", Content: "!!!!"})
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)
	_, err := c.GetFileContents(context.Background(), Repo{"o", "r"}, "x", "")
	require.ErrorContains(t, err, "decoding")
}

func TestListWorkflowFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/o/r/contents/.github/workflows", r.URL.Path)
		_ = json.NewEncoder(w).Encode([]contentEntry{
			{Type: "file", Name: "ci.yml", Path: ".github/workflows/ci.yml", SHA: "a", Size: 10},
			{Type: "file", Name: "release.yaml", Path: ".github/workflows/release.yaml", SHA: "b"},
			{Type: "file", Name: "README.md", Path: ".github/workflows/README.md"},
			{Type: "dir", Name: "nested", Path: ".github/workflows/nested"},
		})
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	got, err := c.ListWorkflowFiles(context.Background(), Repo{"o", "r"}, "main")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, ".github/workflows/ci.yml", got[0].Path)
	assert.Equal(t, ".github/workflows/release.yaml", got[1].Path)
}

func TestListWorkflowFilesMissingDirIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c, _ := testClient(t, srv)
	_, err := c.ListWorkflowFiles(context.Background(), Repo{"o", "r"}, "main")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), ".github/workflows")

	_, err = c.ListWorkflowFiles(context.Background(), Repo{"", "r"}, "main")
	require.Error(t, err)
}

func TestRepoHelpers(t *testing.T) {
	r := RepoOf(model.Repo{Owner: "o", Name: "n"})
	assert.Equal(t, "o/n", r.String())
	assert.True(t, r.Valid())
	assert.False(t, Repo{Owner: "o"}.Valid())
	assert.Equal(t, "/repos/o/n", r.path())
}

func TestEscapePathAndRateParsing(t *testing.T) {
	assert.Equal(t, "a/b%20c/d", escapePath("a/b c/d"))
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "not-a-number")
	rl := parseRateLimit(h, time.Unix(1, 0))
	assert.Equal(t, 0, rl.Limit)
	assert.Equal(t, -1, rl.Remaining)
}

func TestOnRateLimitCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "12")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var seen []RateLimit
	c, _ := testClient(t, srv, func(o *Options) { o.OnRateLimit = func(rl RateLimit) { seen = append(seen, rl) } })
	_, err := c.Get(context.Background(), "/x", nil)
	require.NoError(t, err)
	require.Len(t, seen, 1)
	assert.Equal(t, 12, seen[0].Remaining)
}

func TestDoRejectsUnencodableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c, _ := testClient(t, srv)
	_, err := c.Post(context.Background(), "/x", make(chan int), nil)
	require.ErrorContains(t, err, "encoding")
}

func TestDoReportsUndecodableResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)
	var out struct{ ID int }
	_, err := c.Get(context.Background(), "/x", &out)
	require.ErrorContains(t, err, "decoding")
}

func TestDoRetriesTransportFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c, slept := testClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})),
		func(o *Options) { o.MaxRetries = 2 })
	_, err := c.Get(context.Background(), url+"/x", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 3 attempts")
	assert.Len(t, *slept, 2)
}

func TestBackoffIsCapped(t *testing.T) {
	c := &Client{baseBackoff: time.Second, maxBackoff: 4 * time.Second, now: time.Now}
	assert.Equal(t, time.Second, c.backoff(0, nil))
	assert.Equal(t, 4*time.Second, c.backoff(5, nil))
	assert.Equal(t, 4*time.Second, c.backoff(99, nil))
}

func TestHostOf(t *testing.T) {
	assert.Equal(t, "api.github.com", hostOf("https://api.github.com/x"))
	assert.Equal(t, "", hostOf("://nope"))
}
