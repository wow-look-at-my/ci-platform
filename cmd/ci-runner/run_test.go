package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/sandbox"
)

func TestValidateNamesEveryMissingFlag(t *testing.T) {
	c := config{capacity: 1}
	err := c.validate()
	require.Error(t, err, "a runner with no control plane must fail at startup, never idle green")
	for _, flag := range []string{"-url", "-token", "-state-dir", "-labels"} {
		assert.Contains(t, err.Error(), flag)
	}
}

func TestValidateAcceptsACompleteConfig(t *testing.T) {
	c := config{
		url: "https://ci.example.com", token: "t",
		stateDir: t.TempDir(), labels: "self-hosted", capacity: 2,
	}
	require.NoError(t, c.validate())
	assert.NotEmpty(t, c.name, "the hostname stands in for an unset -name")
}

func TestValidateRejectsBadCapacity(t *testing.T) {
	c := config{url: "u", token: "t", stateDir: "s", labels: "l", capacity: 0}
	err := c.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-capacity")
}

func TestResolveIDIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	c := config{stateDir: dir}

	first, err := c.resolveID()
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	second, err := (&config{stateDir: dir}).resolveID()
	require.NoError(t, err)
	assert.Equal(t, first, second, "a restart must not look like a new runner")

	explicit, err := (&config{stateDir: dir, id: "given"}).resolveID()
	require.NoError(t, err)
	assert.Equal(t, "given", explicit)
}

func TestResolveIDReportsAnUnreadableStateDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "runner-id"), 0o755))
	_, err := (&config{stateDir: dir}).resolveID()
	require.Error(t, err)
}

func TestSplitLabels(t *testing.T) {
	assert.Equal(t, []string{"self-hosted", "linux", "x64"}, splitLabels(" self-hosted, linux ,x64, "))
	assert.Nil(t, splitLabels("  "))
}

func TestEnvHelpers(t *testing.T) {
	assert.Equal(t, "fallback", envOr("CI_RUNNER_TEST_UNSET", "fallback"))
	t.Setenv("CI_RUNNER_TEST_SET", "value")
	assert.Equal(t, "value", envOr("CI_RUNNER_TEST_SET", "fallback"))
	t.Setenv("CI_RUNNER_TEST_EMPTY", "")
	assert.Equal(t, "fallback", envOr("CI_RUNNER_TEST_EMPTY", "fallback"))

	assert.Equal(t, 4, envInt("CI_RUNNER_TEST_UNSET_INT", 4))
	t.Setenv("CI_RUNNER_TEST_INT", "9")
	assert.Equal(t, 9, envInt("CI_RUNNER_TEST_INT", 4))

	assert.Equal(t, time.Minute, envDuration("CI_RUNNER_TEST_UNSET_DUR", time.Minute))
	t.Setenv("CI_RUNNER_TEST_DUR", "90s")
	assert.Equal(t, 90*time.Second, envDuration("CI_RUNNER_TEST_DUR", time.Minute))
}

func TestCommandsRegisterThemselves(t *testing.T) {
	for _, name := range []string{"run", "version"} {
		cmd, ok := registry[name]
		require.True(t, ok, "command %q did not register itself", name)
		assert.NotEmpty(t, cmd.short)
		assert.NotNil(t, cmd.run)
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	usage()
	require.NoError(t, w.Close())
	os.Stderr = orig

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	assert.Contains(t, out, "run")
	assert.Contains(t, out, "version")
	assert.True(t, strings.HasPrefix(out, "usage: ci-runner"))
}

func TestRunCommandFailsFastOnMissingConfig(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Env vars must not leak a real control plane into this test.
	for _, k := range []string{"CI_CONTROL_PLANE_URL", "CI_RUNNER_TOKEN", "CI_RUNNER_STATE_DIR", "CI_RUNNER_LABELS"} {
		t.Setenv(k, "")
	}
	err := runCommand(context.Background(), fs, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required configuration")
}

func TestVersionCommandPrints(t *testing.T) {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	require.NoError(t, registry["version"].run(context.Background(), fs, nil))
}

func TestToAgentReport(t *testing.T) {
	assert.Nil(t, toAgentReport(nil))
	got := toAgentReport(&sandbox.SetupReport{
		Breakdown: map[string]time.Duration{"dockerd_ready": time.Second},
		CacheWarm: true, Total: 2 * time.Second,
	})
	require.NotNil(t, got)
	assert.True(t, got.CacheWarm)
	assert.Equal(t, 2*time.Second, got.Total)
	assert.Equal(t, time.Second, got.Breakdown["dockerd_ready"])
}

func TestRunCommandWiresUpAndShutsDownCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == protocol.PathRegister {
			_ = json.NewEncoder(w).Encode(protocol.RegisterResponse{})
			return
		}
		// Every acquire poll comes back empty; the run ends on SIGTERM.
		_ = json.NewEncoder(w).Encode(protocol.AcquireResponse{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	err := runCommand(ctx, fs, []string{
		"-url", srv.URL,
		"-token", "tok",
		"-labels", "self-hosted,linux",
		"-state-dir", t.TempDir(),
		"-poll-wait", "10ms",
		"-actions-api-url", srv.URL,
	})
	require.NoError(t, err, "a cancelled context is a graceful shutdown, not a failure")
}
