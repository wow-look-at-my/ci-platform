package statuses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

var repo = gh.Repo{Owner: "wow-look-at-my", Name: "ci-platform"}

const sha = "0123456789abcdef0123456789abcdef01234567"

type recorded struct {
	Method string
	Path   string
	Body   map[string]any
}

func newFake(t *testing.T, opts Options) (*[]recorded, *Reporter) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, recorded{Method: r.Method, Path: r.URL.Path, Body: body})
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "state": body["state"], "context": body["context"]})
	}))
	t.Cleanup(srv.Close)

	cli, err := gh.NewClient(gh.Options{
		BaseURL: srv.URL, Tokens: gh.StaticToken("ghs_x"), MaxRetries: -1,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)
	return &got, NewReporter(cli, opts)
}

func TestPostSendsExactBody(t *testing.T) {
	got, r := newFake(t, Options{})
	out, err := r.Post(context.Background(), Status{
		Repo: repo, SHA: sha, Context: "build (linux)", State: StateSuccess,
		Description: "all 42 tests passed", TargetURL: "https://ci.example/jobs/1",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5), out.ID)
	require.Len(t, *got, 1)
	assert.Equal(t, http.MethodPost, (*got)[0].Method)
	assert.Equal(t, "/repos/wow-look-at-my/ci-platform/statuses/"+sha, (*got)[0].Path)
	assert.Equal(t, map[string]any{
		"state":       "success",
		"context":     "build (linux)",
		"description": "all 42 tests passed",
		"target_url":  "https://ci.example/jobs/1",
	}, (*got)[0].Body)
}

// The org's aggregate context is owned by another app; posting it would shadow
// the real gate in the UI.
func TestAllBuildsContextIsRefused(t *testing.T) {
	got, r := newFake(t, Options{})
	for _, name := range []string{"all-builds", "ALL-BUILDS", "  all-builds  "} {
		_, err := r.Post(context.Background(), Status{Repo: repo, SHA: sha, Context: name, State: StateSuccess})
		require.ErrorIs(t, err, ErrForbiddenContext, name)
		assert.Contains(t, err.Error(), "owned by another app")
		assert.True(t, r.Forbidden(name))
	}
	assert.Empty(t, *got, "a forbidden context must never reach the API")
}

func TestExtraForbiddenContexts(t *testing.T) {
	got, r := newFake(t, Options{ForbiddenContexts: []string{"required", "  "}})
	_, err := r.Post(context.Background(), Status{Repo: repo, SHA: sha, Context: "Required", State: StatePending})
	require.ErrorIs(t, err, ErrForbiddenContext)
	// all-builds stays forbidden even when the caller supplies its own set.
	assert.True(t, r.Forbidden(AllBuildsContext))
	assert.False(t, r.Forbidden("build"))
	assert.Empty(t, *got)
}

func TestDescriptionTruncatedTo140WithEllipsis(t *testing.T) {
	got, r := newFake(t, Options{})
	long := strings.Repeat("a", 300)
	_, err := r.Post(context.Background(), Status{
		Repo: repo, SHA: sha, Context: "build", State: StateFailure, Description: long,
	})
	require.NoError(t, err)
	desc := (*got)[0].Body["description"].(string)
	assert.Len(t, desc, MaxDescription)
	assert.True(t, strings.HasSuffix(desc, "..."))
	assert.Equal(t, strings.Repeat("a", 137)+"...", desc)
}

func TestTruncateDescription(t *testing.T) {
	assert.Equal(t, "short", TruncateDescription("short"))
	assert.Len(t, TruncateDescription(strings.Repeat("b", MaxDescription)), MaxDescription)
	assert.Len(t, TruncateDescription(strings.Repeat("b", MaxDescription+1)), MaxDescription)
	// A multi-byte tail is cut on a rune boundary, never mid-character.
	multi := TruncateDescription(strings.Repeat("é", 100))
	assert.LessOrEqual(t, len(multi), MaxDescription)
	assert.True(t, strings.HasSuffix(multi, "..."))
	assert.True(t, isValidUTF8(multi))
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestStateForMapping(t *testing.T) {
	cases := []struct {
		status model.Status
		concl  model.Conclusion
		want   State
	}{
		{model.StatusQueued, "", StatePending},
		{model.StatusInProgress, "", StatePending},
		{model.StatusWaiting, "", StatePending},
		{model.StatusCompleted, model.ConclusionSuccess, StateSuccess},
		{model.StatusCompleted, model.ConclusionFailure, StateFailure},
		{model.StatusCompleted, model.ConclusionTimedOut, StateFailure},
		// The deviation that matters: infra and config are "error", the legacy
		// API's slot for "not your build".
		{model.StatusCompleted, model.ConclusionInfraFailure, StateError},
		{model.StatusCompleted, model.ConclusionConfigError, StateError},
		{model.StatusCompleted, model.ConclusionActionRequired, StateError},
		{model.StatusCompleted, model.ConclusionCancelled, StateError},
		{model.StatusCompleted, model.ConclusionSkipped, StateSuccess},
		{model.StatusCompleted, model.ConclusionNeutral, StateSuccess},
		{model.StatusCompleted, model.ConclusionStale, StateSuccess},
		{model.StatusCompleted, model.Conclusion("invented"), StateError},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, StateFor(c.status, c.concl), "%s/%s", c.status, c.concl)
	}
}

func TestPostValidation(t *testing.T) {
	got, r := newFake(t, Options{})
	ctx := context.Background()

	_, err := r.Post(ctx, Status{SHA: sha, Context: "c", State: StateSuccess})
	require.ErrorContains(t, err, "repo owner/name")

	_, err = r.Post(ctx, Status{Repo: repo, Context: "c", State: StateSuccess})
	require.ErrorContains(t, err, "no SHA")

	_, err = r.Post(ctx, Status{Repo: repo, SHA: sha, State: StateSuccess})
	require.ErrorContains(t, err, "no context")

	_, err = r.Post(ctx, Status{Repo: repo, SHA: sha, Context: strings.Repeat("c", MaxContext+1), State: StateSuccess})
	require.ErrorContains(t, err, "limit 255")

	_, err = r.Post(ctx, Status{Repo: repo, SHA: sha, Context: "c", State: State("green")})
	require.ErrorContains(t, err, "error/failure/pending/success")

	assert.Empty(t, *got)
}

func TestPostSurfacesAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"No commit found for SHA"}`))
	}))
	defer srv.Close()
	cli, err := gh.NewClient(gh.Options{BaseURL: srv.URL, Tokens: gh.StaticToken("t"), MaxRetries: -1})
	require.NoError(t, err)
	r := NewReporter(cli, Options{})
	_, err = r.Post(context.Background(), Status{Repo: repo, SHA: sha, Context: "build", State: StatePending})
	require.ErrorContains(t, err, `posting "build" to wow-look-at-my/ci-platform@`)
	require.ErrorContains(t, err, "No commit found")
}

func TestPostWithoutClientFails(t *testing.T) {
	r := NewReporter(nil, Options{})
	_, err := r.Post(context.Background(), Status{Repo: repo, SHA: sha, Context: "c", State: StateSuccess})
	require.ErrorContains(t, err, "no GitHub client")
}

// The commit status context is the check run name, so protection rules written
// against either surface keep matching.
func TestPostJobUsesTheJobNameAsContext(t *testing.T) {
	got, r := newFake(t, Options{})
	job := &model.Job{
		Name:       "publish (claude-host/agent-host, Dockerfile)",
		Status:     model.StatusCompleted,
		Conclusion: model.ConclusionInfraFailure,
	}
	_, err := r.PostJob(context.Background(), repo, sha, job, "registry returned 503", "https://ci.example/jobs/9")
	require.NoError(t, err)
	require.Len(t, *got, 1)
	assert.Equal(t, job.Name, (*got)[0].Body["context"])
	assert.Equal(t, "error", (*got)[0].Body["state"])

	_, err = r.PostJob(context.Background(), repo, sha, nil, "", "")
	require.ErrorContains(t, err, "nil job")
}

func TestPostJobRefusesAllBuildsJobName(t *testing.T) {
	got, r := newFake(t, Options{})
	job := &model.Job{Name: AllBuildsContext, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess}
	_, err := r.PostJob(context.Background(), repo, sha, job, "", "")
	require.ErrorIs(t, err, ErrForbiddenContext)
	assert.Empty(t, *got)
}
