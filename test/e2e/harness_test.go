// Package e2e starts the real control-plane binary against a real SQLite file and
// a fake GitHub, and drives events through it.
//
// It runs the shipped binary rather than wiring the packages up in-process, so
// what it proves is that the thing we deploy works, not that the pieces compose
// in a test harness.
//
// Every test runs in parallel and every test starts its own control plane, on
// its own port, with its own database and its own fake GitHub. That is what
// keeps a package of process-spawning tests inside one 30-second budget: the
// alternative is having fewer of them, and each one here is a property worth
// keeping.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/test/fakes"
)

// operatorToken is what the suite signs its API reads with. The control plane
// refuses to start without one.
const operatorToken = "e2e-operator-token-0123456789"

// controlPlane is a running instance under test.
type controlPlane struct {
	URL    string
	GitHub *fakes.GitHub
	cmd    *exec.Cmd
	out    *bytes.Buffer
	// repo is unique per test: each control plane gets its own database file,
	// but distinct repositories keep a failure legible.
	repoID   int64
	repoName string
}

func start(t *testing.T, workflows map[string]string) *controlPlane {
	t.Helper()
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
		"CIPLATFORM_PUBLIC_URL=http://ci.localhost:"+port,
		// The stand-in GitHub is where the repositories are, so it is also
		// where a browser would be sent to sign in.
		"CIPLATFORM_GITHUB_SERVER_URL=http://github.localhost",
		"CIPLATFORM_ALLOWED_OWNERS=acme",
		"CIPLATFORM_ADMIN_LOGINS=PazerOP",
		"CIPLATFORM_OAUTH_CLIENT_ID=Iv1.e2e",
		"CIPLATFORM_OAUTH_CLIENT_SECRET=e2e-client-secret",
		"CIPLATFORM_DATABASE_URL="+filepath.Join(dir, "ciplatform.db"),
		"CIPLATFORM_GITHUB_API_URL="+gh.URL(),
		"CIPLATFORM_WEBHOOK_SECRET="+gh.WebhookSecret,
		"CIPLATFORM_APP_ID=12345",
		"CIPLATFORM_APP_PRIVATE_KEY_PATH="+keyPath,
		"CIPLATFORM_JOB_TOKEN_SECRET=0123456789abcdef0123456789abcdef",
		"CIPLATFORM_OPERATOR_TOKEN="+operatorToken,
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
	c.pushFrom(t, "acme", ref, sha, changed)
}

// pushFrom delivers a push from an arbitrary account, which is what a stranger
// who installed the published App would produce.
func (c *controlPlane) pushFrom(t *testing.T, owner, ref, sha string, changed []string) {
	t.Helper()
	payload := map[string]any{
		"ref": ref, "after": sha, "before": "0000000",
		"repository": map[string]any{
			"id": c.repoID, "name": c.repoName, "full_name": owner + "/" + c.repoName,
			"default_branch": "main", "owner": map[string]any{"login": owner},
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

// get reads an operator endpoint with the credential, the way a gh-alike
// client or the signed-in UI would.
func (c *controlPlane) get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// runs reads the API the way a gh-alike client would.
func (c *controlPlane) runs(t *testing.T) []map[string]any {
	t.Helper()
	return c.runsAt(t, "/api/v1/runs?repo=acme/"+c.repoName)
}

// allRuns asks without a repository filter, which is how a test proves nothing
// was created anywhere rather than nothing was created for one repository.
func (c *controlPlane) allRuns(t *testing.T) []map[string]any {
	t.Helper()
	return c.runsAt(t, "/api/v1/runs")
}

func (c *controlPlane) runsAt(t *testing.T, path string) []map[string]any {
	t.Helper()
	resp := c.get(t, path)
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

// nextID keeps repo identities distinct across tests in one process.
var nextID atomic.Int64

func nextRepoID() int64 { return 100000 + nextID.Add(1) }

var (
	binOnce sync.Once
	binDir  string
	binPath string
	binErr  error
)

// TestMain owns the one control-plane binary every test in this package runs.
//
// It is built once rather than per test because the whole package shares a
// 30-second budget, and a dozen `go build` invocations spend it on work that
// produces the same bytes every time.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

func buildBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "ciplatform-e2e")
		if binErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "ciplatform")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/wow-look-at-my/ci-platform/cmd/ciplatform")
		if out, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("building the control plane: %w: %s", err, out)
		}
	})
	require.NoError(t, binErr)
	return binPath
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
