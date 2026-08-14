package main

import (
	"context"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/config"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func testApp(t *testing.T) *app {
	t.Helper()
	return &app{
		cfg: &config.Config{
			PublicURL:         mustURL(t, "https://ci.pazer.build"),
			GitHubServerURL:   mustURL(t, "https://github.com"),
			ArtifactRetention: 90 * 24 * time.Hour,
		},
		store: mem.New(),
		log:   slog.Default(),
	}
}

func seedRun(t *testing.T, a *app, forkPR bool) *model.Run {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.store.UpsertRepo(ctx, &model.Repo{ID: 1, Owner: "PazerOP", Name: "ci-platform"}))
	run := &model.Run{
		RepoID: 1, RepoFull: "PazerOP/ci-platform", WorkflowName: "CI",
		WorkflowPath: ".github/workflows/ci.yml", RunNumber: 1, Attempt: 1,
		Event: "push", HeadSHA: "deadbeef", HeadBranch: "main", Actor: "PazerOP",
		Status: model.StatusInProgress, IsForkPR: forkPR, CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, a.store.CreateRun(ctx, run))
	return run
}

// The environment a job runs with carries two different URLs, and they are not
// interchangeable: the artifact endpoints are this platform, GITHUB_SERVER_URL
// is where the repositories are. Sending this platform as both puts it in front
// of every actions/checkout clone, and this map is applied over the job's base
// environment, so a wrong value here wins.
func TestServiceEnv_SeparatesThisPlatformFromGitHub(t *testing.T) {
	a := testApp(t)
	run := seedRun(t, a, false)

	env := a.serviceEnv(run.ID, 2, 1, "job-token")

	assert.Equal(t, "https://github.com", env["GITHUB_SERVER_URL"])
	assert.Equal(t, "https://ci.pazer.build", env["ACTIONS_RESULTS_URL"])
	assert.Equal(t, "https://ci.pazer.build/", env["ACTIONS_RUNTIME_URL"])
	assert.Equal(t, "job-token", env["ACTIONS_RUNTIME_TOKEN"])
}

// An ordinary deployment hostname is fine for the platform: the isGhes() rule
// the artifact client enforces is about GITHUB_SERVER_URL, which is github.com.
func TestServiceEnv_WorksOnAnOrdinaryHostname(t *testing.T) {
	a := testApp(t)
	run := seedRun(t, a, false)

	env := a.serviceEnv(run.ID, 2, 1, "job-token")

	require.NotEmpty(t, env["ACTIONS_RESULTS_URL"])
	assert.NotContains(t, env["ACTIONS_RESULTS_URL"], ".localhost")
	assert.NotContains(t, env["ACTIONS_RESULTS_URL"], ".ghe.com")
}

// A fork PR is a stranger's code, so the OIDC endpoint is not merely refused: it
// is not in the environment to be found.
func TestServiceEnv_ForkPRGetsNoIDTokenVariables(t *testing.T) {
	a := testApp(t)
	fork := seedRun(t, a, true)

	env := a.serviceEnv(fork.ID, 2, 1, "job-token")

	assert.NotContains(t, env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	assert.NotContains(t, env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
}

// An unset secret generates a key rather than failing, and says so: sessions
// ending at every restart is a consequence an operator has to be told about.
func TestSessionKey_GeneratesOneAndSaysSo(t *testing.T) {
	key, err := sessionKey(&config.Config{}, slog.Default())
	require.NoError(t, err)
	assert.Len(t, key, 32)

	configured, err := sessionKey(&config.Config{SessionSecret: "a-configured-secret"}, slog.Default())
	require.NoError(t, err)
	assert.Equal(t, []byte("a-configured-secret"), configured)
}
