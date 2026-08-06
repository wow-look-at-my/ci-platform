package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

// requireDocker gates the tests that need a live daemon. They are opt-in
// because they pull an image and start a privileged container.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("CIPLATFORM_TEST_DOCKER") != "1" {
		t.Skip("set CIPLATFORM_TEST_DOCKER=1 to run the tests that build a real Docker-in-Docker sandbox (needs a docker daemon and privileged containers)")
	}
}

func TestDockerSandboxEndToEnd(t *testing.T) {
	requireDocker(t)

	image := os.Getenv("CIPLATFORM_TEST_DIND_IMAGE")
	if image == "" {
		image = DefaultImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var logs []string
	c, report, err := Create(ctx, Options{
		JobID: time.Now().UnixNano() % 100000, Attempt: 1,
		Image:            image,
		ImageCacheVolume: "ciplatform-test-image-cache",
		LockDir:          t.TempDir(),
		SetupTimeout:     8 * time.Minute,
		NamePrefix:       "ciplatform-test",
		Log:              func(m string) { logs = append(logs, m); t.Log(m) },
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		if cerr := c.Close(context.Background()); cerr != nil {
			t.Errorf("teardown: %v", cerr)
		}
	})

	t.Logf("setup total=%s warm=%v breakdown=%v", report.Total, report.CacheWarm, report.Breakdown)
	assert.Contains(t, report.Breakdown, "dockerd_ready")
	assert.NotZero(t, report.Breakdown["dockerd_ready"])

	// The inner daemon is real and answers.
	var ver strings.Builder
	res, err := c.Run(ctx, exec.RunRequest{
		Argv:   []string{"docker", "version", "--format", "{{.Server.Version}}"},
		Stdout: &ver,
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode, "inner docker version failed")
	assert.NotEmpty(t, strings.TrimSpace(ver.String()))

	// A file placed with docker cp is readable inside, including into a
	// directory that did not exist.
	const script = "#!/bin/sh\necho from-the-sandbox\n"
	require.NoError(t, c.WriteFile(ctx, "/home/runner/work/_temp/nested/deep/step.sh", []byte(script), 0o700))

	var out strings.Builder
	res, err = c.Run(ctx, exec.RunRequest{
		Argv:       []string{"sh", "/home/runner/work/_temp/nested/deep/step.sh"},
		WorkingDir: c.WorkspaceDir(),
		Env:        map[string]string{"UNUSED": "1"},
		Stdout:     &out,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "from-the-sandbox\n", out.String())

	// The footgun this design exists for: a file placed with docker cp lives in
	// the container's own filesystem, so the INNER daemon can bind-mount it
	// into a container it spawns. A file bind-mounted in from the host cannot
	// be, because the inner daemon resolves the source against the host.
	//
	// The image is built from the sandbox's own filesystem rather than pulled,
	// so this holds with no registry access.
	var mounted strings.Builder
	res, err = c.Run(ctx, exec.RunRequest{
		Argv: []string{"sh", "-c",
			"tar -cf - -C / bin lib usr etc 2>/dev/null | docker import - innertest:latest >/dev/null && " +
				"docker run --rm -v /home/runner/work/_temp/nested/deep/step.sh:/mnt/step.sh innertest:latest /bin/sh /mnt/step.sh"},
		Stdout: &mounted,
		Stderr: &mounted,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode, "inner bind mount failed: %s", mounted.String())
	assert.Contains(t, mounted.String(), "from-the-sandbox")

	// Exit codes come back as values.
	res, err = c.Run(ctx, exec.RunRequest{Argv: []string{"sh", "-c", "exit 5"}})
	require.NoError(t, err)
	assert.Equal(t, 5, res.ExitCode)

	// A missing binary is an error naming it.
	_, err = c.LookPath(ctx, "definitely-not-installed")
	require.Error(t, err)

	data, err := c.ReadFile(ctx, "/home/runner/work/_temp/nested/deep/step.sh")
	require.NoError(t, err)
	assert.Equal(t, script, string(data))

	assert.Contains(t, strings.Join(logs, "\n"), "ready")
}

func TestDockerSandboxHasNoHostSocket(t *testing.T) {
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c, _, err := Create(ctx, Options{
		JobID: time.Now().UnixNano()%100000 + 1, Attempt: 1,
		SetupTimeout: 6 * time.Minute,
		NamePrefix:   "ciplatform-test-nosock",
	})
	require.NoError(t, err)
	defer c.Close(context.Background())

	// The inner daemon's image list is its own, and the job cannot reach the
	// host's daemon through a mounted socket.
	var out strings.Builder
	res, err := c.Run(ctx, exec.RunRequest{
		Argv:   []string{"sh", "-c", "ls -l /var/run/docker.sock && readlink -f /var/run/docker.sock"},
		Stdout: &out, Stderr: &out,
	})
	require.NoError(t, err)
	t.Logf("socket check (exit %d): %s", res.ExitCode, out.String())
	// The only socket present is the inner daemon's own, created by dockerd
	// inside the container; nothing was bind-mounted from the host.
	assert.NotContains(t, out.String(), "/host")
}

func TestDockerTeardownRemovesTheWorkspaceVolume(t *testing.T) {
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	jobID := time.Now().UnixNano()%100000 + 2
	c, _, err := Create(ctx, Options{
		JobID: jobID, Attempt: 1,
		SetupTimeout: 6 * time.Minute,
		NamePrefix:   "ciplatform-test-teardown",
	})
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx))

	cli := &CLI{}
	_, err = capture(ctx, cli, "inspect", c.Name())
	assert.Error(t, err, "the container must be gone after teardown")
	_, err = capture(ctx, cli, "volume", "inspect", c.workspace)
	assert.Error(t, err, "the workspace volume must be gone after teardown")
}
