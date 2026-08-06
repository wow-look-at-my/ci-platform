// Package e2e starts the real control-plane binary against a real Postgres and
// a fake GitHub, and drives events through it.
//
// It runs the shipped binary rather than wiring the packages up in-process, so
// what it proves is that the thing we deploy works, not that the pieces compose
// in a test harness.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/test/fakes"
)

const dbEnv = "CIPLATFORM_TEST_DATABASE_URL"

// controlPlane is a running instance under test.
type controlPlane struct {
	URL    string
	GitHub *fakes.GitHub
	cmd    *exec.Cmd
	out    *bytes.Buffer
	// repo is unique per test: the suite shares one Postgres, so a test that
	// listed every run would see its neighbours.
	repoID   int64
	repoName string
}

func start(t *testing.T, workflows map[string]string) *controlPlane {
	t.Helper()
	dsn := os.Getenv(dbEnv)
	if dsn == "" {
		t.Skipf("set %s to run the end-to-end suite, e.g. %s=postgres://postgres:postgres@127.0.0.1:5432/ciplatform?sslmode=disable",
			dbEnv, dbEnv)
	}

	gh := fakes.NewGitHub()
	t.Cleanup(gh.Close)
	for path, body := range workflows {
		gh.AddFile(path, body)
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app.pem")
	writeRSAKey(t, keyPath)

	bin := buildBinary(t)
	port := freePort(t)

	out := &bytes.Buffer{}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CIPLATFORM_LISTEN=127.0.0.1:"+port,
		// The hostname must satisfy the artifact client's isGhes() test.
		"CIPLATFORM_PUBLIC_URL=http://ci.localhost:"+port,
		"CIPLATFORM_DATABASE_URL="+dsn,
		"CIPLATFORM_GITHUB_API_URL="+gh.URL(),
		"CIPLATFORM_WEBHOOK_SECRET="+gh.WebhookSecret,
		"CIPLATFORM_APP_ID=12345",
		"CIPLATFORM_APP_PRIVATE_KEY_PATH="+keyPath,
		"CIPLATFORM_JOB_TOKEN_SECRET=0123456789abcdef0123456789abcdef",
		"CIPLATFORM_BLOB_ROOT="+filepath.Join(dir, "blobs"),
		"CIPLATFORM_OIDC_KEY_PATH="+filepath.Join(dir, "oidc"),
	)
	cmd.Stdout = out
	cmd.Stderr = out
	require.NoError(t, cmd.Start())

	cp := &controlPlane{
		URL: "http://127.0.0.1:" + port, GitHub: gh, cmd: cmd, out: out,
		repoID: nextRepoID(), repoName: "widget-" + strconv.FormatInt(nextID.Load(), 10),
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("control plane output:\n%s", out.String())
		}
	})
	cp.waitReady(t)
	return cp
}

// exited reports whether the process is already gone, so a startup failure
// fails the test immediately instead of after the full readiness timeout.
func (c *controlPlane) exited() bool {
	p, err := os.FindProcess(c.cmd.Process.Pid)
	if err != nil {
		return true
	}
	return p.Signal(syscall.Signal(0)) != nil
}

func (c *controlPlane) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(c.URL + "/.well-known/docker-updater/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
		}
		require.False(t, c.exited())

		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("control plane never became ready:\n%s", c.out.String())
}

// push delivers a signed push webhook, as GitHub would.
func (c *controlPlane) push(t *testing.T, ref, sha string, changed []string) {
	t.Helper()
	payload := map[string]any{
		"ref": ref, "after": sha, "before": "0000000",
		"repository": map[string]any{
			"id": c.repoID, "name": c.repoName, "full_name": "acme/" + c.repoName,
			"default_branch": "main", "owner": map[string]any{"login": "acme"},
		},
		"sender":       map[string]any{"login": "alex"},
		"installation": map[string]any{"id": 99},
		"head_commit":  map[string]any{"id": sha, "message": "do a thing", "modified": changed},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, c.URL+"/webhook", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", sha+"-"+ref)
	req.Header.Set("X-Hub-Signature-256", c.GitHub.SignWebhook(body))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "webhook rejected: %s", c.out.String())
}

// runs reads the API the way a gh-alike client would.
func (c *controlPlane) runs(t *testing.T) []map[string]any {
	t.Helper()
	resp, err := http.Get(c.URL + "/api/v1/runs?repo=acme/" + c.repoName)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		TotalCount int              `json:"total_count"`
		Runs       []map[string]any `json:"workflow_runs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Runs
}

func (c *controlPlane) waitForRuns(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []map[string]any
	for time.Now().Before(deadline) {
		last = c.runs(t)
		if len(last) >= n {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("expected %d runs, saw %d\n%s", n, len(last), c.out.String())
	return nil
}

const ciWorkflow = `name: CI
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: [self-hosted, linux]
    steps:
      - run: make build
  test:
    runs-on: [self-hosted, linux]
    needs: [build]
    steps:
      - run: make test
`

// A push on a matching branch produces a run with the workflow's jobs.
func TestPushCreatesARun(t *testing.T) {
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
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	resp, err := http.Get(cp.URL + "/.well-known/docker-updater/health")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	hz, err := http.Get(cp.URL + "/healthz")
	require.NoError(t, err)
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
	assert.True(t, health.StoreDurable, "the e2e suite runs against Postgres")
	assert.NotEmpty(t, health.Subsystems)
}

// The UI is served from the embedded bundle, so a deployment needs no asset
// pipeline at run time.
func TestWebUIIsServed(t *testing.T) {
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
	cp := start(t, map[string]string{".github/workflows/ci.yml": ciWorkflow})

	resp, err := http.Post(cp.URL+"/runner/v1/register", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// nextID keeps repo identities distinct across tests in one process.
var nextID atomic.Int64

func nextRepoID() int64 { return 100000 + nextID.Add(1) }

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ciplatform")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/wow-look-at-my/ci-platform/cmd/ciplatform")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building the control plane: %s", out)
	return bin
}

func writeRSAKey(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	return port
}
