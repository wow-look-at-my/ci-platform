// The behaviours this platform promises, checked against the shipped binary.
// The harness that starts one lives in harness_test.go.
package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The App is published, so anybody can install it. An install by an account
// the operator did not name must schedule nothing: this is the whole safety
// property of publishing, checked against the real binary rather than a unit.
func TestPushFromAnAccountThatIsNotServedCreatesNothing(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	cp.pushFrom(t, "a-total-stranger", "refs/heads/main",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"main.go"})

	// A run would appear within a second or so; give it longer than that before
	// concluding nothing happened.
	time.Sleep(2 * time.Second)
	assert.Empty(t, cp.allRuns(t), "a stranger's push scheduled work on the runners")
	assert.Contains(t, cp.out.String(), "refused a webhook delivery",
		"the refusal has to be visible to the operator, not silent")
}

// A push on a matching branch produces a run with the workflow's jobs.
func TestPushCreatesARun(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	cp.push(t, "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"main.go"})
	runs := cp.waitForRuns(t, 1)

	require.NotEmpty(t, runs)
	run := runs[0]
	assert.Equal(t, "CI", run["workflow_name"])
	assert.Equal(t, "push", run["event"])
	assert.Equal(t, "main", run["head_branch"])
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", run["head_sha"])
}

// The branch filter is applied. Before it was implemented, a workflow scoped to
// main ran on every branch, which is the silent-ignore failure this platform
// refuses everywhere else.
func TestPushToAFilteredBranchCreatesNoRun(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	cp.push(t, "refs/heads/feature/x", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"main.go"})

	// Give the control plane a moment to do the thing we are asserting it does
	// not do.
	time.Sleep(2 * time.Second)
	assert.Empty(t, cp.runs(t), "a workflow filtered to main must not run on a feature branch")
}

// An unsupported feature fails the run with a reason rather than being skipped:
// a workflow silently absent from a commit's checks looks exactly like one that
// passed.
func TestUnsupportedWorkflowFailsTheRunRatherThanVanishing(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{
		".github/workflows/svc.yml": `name: Services
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    container:
      image: node:20
    steps:
      - run: node --version
`,
	})

	cp.push(t, "refs/heads/main", "cccccccccccccccccccccccccccccccccccccccc", []string{"main.go"})
	runs := cp.waitForRuns(t, 1)

	require.NotEmpty(t, runs)
	assert.Equal(t, "config_error", runs[0]["conclusion"],
		"an unimplemented key must fail the run, not be ignored")
}

// A workflow that cannot be parsed also produces a visible failed run.
func TestUnparseableWorkflowFailsTheRun(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{
		".github/workflows/broken.yml": "name: Broken\non: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n        uses: actions/checkout@v4\n",
	})

	cp.push(t, "refs/heads/main", "dddddddddddddddddddddddddddddddddddddddd", nil)
	runs := cp.waitForRuns(t, 1)

	require.NotEmpty(t, runs)
	assert.Equal(t, "config_error", runs[0]["conclusion"])
}

// The health contract: the status-code-only endpoint stays 2xx, because
// answering non-2xx there is what makes an orchestrator roll back a release.
func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	resp, err := http.Get(cp.URL + "/.well-known/docker-updater/health")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	hz := cp.get(t, "/healthz")
	defer hz.Body.Close()
	var health struct {
		Status       string `json:"status"`
		StoreDurable bool   `json:"store_durable"`
		Subsystems   []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"subsystems"`
	}
	require.NoError(t, json.NewDecoder(hz.Body).Decode(&health))
	assert.True(t, health.StoreDurable, "the e2e suite runs against the durable store, not the in-memory one")
	assert.NotEmpty(t, health.Subsystems)
}

// The UI is served from the embedded bundle, so a deployment needs no asset
// pipeline at run time.
func TestWebUIIsServed(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	resp, err := http.Get(cp.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	js, err := http.Get(cp.URL + "/app.mjs")
	require.NoError(t, err)
	defer js.Body.Close()
	assert.Equal(t, http.StatusOK, js.StatusCode)
	assert.Contains(t, js.Header.Get("Content-Type"), "javascript")
}

// A webhook whose signature does not verify is rejected outright.
func TestWebhookSignatureIsRequired(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	req, err := http.NewRequest(http.MethodPost, cp.URL+"/webhook", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.GreaterOrEqual(t, resp.StatusCode, 400)
}

// The runner endpoint refuses an unauthenticated agent: it hands out job
// tokens and secrets.
func TestRunnerEndpointRequiresAToken(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	resp, err := http.Post(cp.URL+"/runner/v1/register", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// Every job container can reach this listener -- it has to, to upload an
// artifact -- so an ungated operator API would let any workflow, a fork PR's
// included, read every repository's logs and cancel every run. The credential
// is the boundary; these are the requests a job could make.
func TestOperatorAPIRefusesAnUncredentialedCaller(t *testing.T) {
	t.Parallel()
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/runs"},
		{http.MethodGet, "/api/v1/jobs/1/logs/raw"},
		{http.MethodGet, "/api/v1/jobs/1/logs/stream"},
		{http.MethodGet, "/api/v1/artifacts/1/download"},
		{http.MethodGet, "/api/v1/runners"},
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/api/v1/runs/1/cancel"},
		{http.MethodPost, "/api/v1/runs/1/rerun"},
		{http.MethodPost, "/api/v1/jobs/1/cancel"},
	} {
		t.Run(probe.method+" "+probe.path, func(t *testing.T) {
			req, err := http.NewRequest(probe.method, cp.URL+probe.path, bytes.NewReader([]byte(`{"reason":"x"}`)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}

	// Signing in with the credential yields a cookie that opens the same API,
	// which is how the UI's log tail and download links authenticate: an
	// EventSource cannot set a header.
	resp, err := http.Post(cp.URL+"/auth/login", "application/json",
		bytes.NewReader([]byte(`{"token":"`+operatorToken+`"}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)

	req, err := http.NewRequest(http.MethodGet, cp.URL+"/api/v1/runs", nil)
	require.NoError(t, err)
	req.AddCookie(cookies[0])
	authed, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer authed.Body.Close()
	assert.Equal(t, http.StatusOK, authed.StatusCode)

	// A wrong credential stays out.
	bad, err := http.Post(cp.URL+"/auth/login", "application/json", bytes.NewReader([]byte(`{"token":"guess"}`)))
	require.NoError(t, err)
	defer bad.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, bad.StatusCode)
}
