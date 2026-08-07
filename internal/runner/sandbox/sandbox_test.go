package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

// fakeDocker records every CLI invocation and lets a test script the answers.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	// respond returns (exitCode, stdout, err) for a call; nil means success.
	respond func(args []string, n int) (int, string, error)
}

func (f *fakeDocker) Run(_ context.Context, in Invocation) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in.Args)
	n := len(f.calls)
	respond := f.respond
	f.mu.Unlock()

	code, out, err := 0, "", error(nil)
	if respond != nil {
		code, out, err = respond(in.Args, n)
	}
	// A healthy inner daemon reports a version; a test that wants an unhealthy
	// one says so explicitly.
	if code == 0 && err == nil && out == "" && isVersionProbe(in.Args) {
		out = "27.0.0\n"
	}
	if out != "" && in.Stdout != nil {
		_, _ = in.Stdout.Write([]byte(out))
	}
	return code, err
}

func isVersionProbe(args []string) bool {
	return len(args) > 3 && args[0] == "exec" && args[2] == "docker" && args[3] == "version"
}

func (f *fakeDocker) joined() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (f *fakeDocker) find(sub string) (string, bool) {
	for _, c := range f.joined() {
		if strings.Contains(c, sub) {
			return c, true
		}
	}
	return "", false
}

func newTestOptions(t *testing.T, d Docker) Options {
	t.Helper()
	return Options{
		JobID: 42, Attempt: 1,
		Image:            "docker:test-dind",
		ImageCacheVolume: "ci-image-cache",
		LockDir:          t.TempDir(),
		Docker:           d,
		SetupTimeout:     2 * time.Second,
		ReadyPoll:        time.Millisecond,
	}
}

func TestCreateBuildsAnIsolatedSandbox(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		if len(args) > 3 && args[0] == "exec" && args[2] == "docker" && args[3] == "image" {
			return 0, "sha256:abc\n", nil // a warm cache
		}
		return 0, "", nil
	}}

	var logs []string
	opts := newTestOptions(t, d)
	opts.Log = func(m string) { logs = append(logs, m) }

	c, report, err := Create(context.Background(), opts)
	require.NoError(t, err)
	defer c.Close(context.Background())

	assert.Equal(t, "ci-job-42-1", c.Name())
	assert.Equal(t, "/workspace", c.WorkspaceDir())

	run, ok := d.find("run -d")
	require.True(t, ok, "calls: %v", d.joined())
	assert.Contains(t, run, "--privileged")
	assert.Contains(t, run, "ci-image-cache:/var/lib/docker")
	assert.Contains(t, run, "ci-job-ws-42-1:/workspace")
	assert.Contains(t, run, "docker:test-dind")
	assert.NotContains(t, run, "/var/run/docker.sock",
		"a job container must never see the host docker socket")
	assert.NotContains(t, run, "job_token")

	_, ok = d.find("image inspect docker:test-dind")
	assert.True(t, ok, "the image is only pulled when absent")
	_, ok = d.find("docker version")
	assert.True(t, ok, "readiness is probed inside the container")

	require.NotNil(t, report)
	for _, key := range []string{"image_cache_volume", "workspace_volume", "image_pull", "container_create", "dockerd_ready", "workspace_prepare"} {
		assert.Contains(t, report.Breakdown, key, "setup breakdown must be measurable, not inferred")
	}
	assert.True(t, report.CacheWarm)
	assert.NotZero(t, report.Total)
	assert.Contains(t, strings.Join(logs, "\n"), "ready")
}

func TestCreateReportsColdCache(t *testing.T) {
	d := &fakeDocker{}
	c, report, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())
	assert.False(t, report.CacheWarm, "an empty image store is a cold cache, whatever the volume's age")
}

func TestCreateWithoutImageCacheVolume(t *testing.T) {
	d := &fakeDocker{}
	opts := newTestOptions(t, d)
	opts.ImageCacheVolume = ""
	c, report, err := Create(context.Background(), opts)
	require.NoError(t, err)
	defer c.Close(context.Background())

	run, _ := d.find("run -d")
	assert.NotContains(t, run, "/var/lib/docker")
	assert.False(t, report.CacheWarm)
}

func TestCreateFailsWhenDockerdNeverBecomesReady(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		if len(args) > 2 && args[0] == "exec" && args[2] == "docker" {
			return 1, "", nil // dockerd never answers
		}
		return 0, "", nil
	}}
	opts := newTestOptions(t, d)
	opts.SetupTimeout = 150 * time.Millisecond

	c, report, err := Create(context.Background(), opts)
	require.Error(t, err)
	assert.Nil(t, c, "a half-built sandbox is never handed back")

	var serr *Error
	require.True(t, errors.As(err, &serr))
	assert.Equal(t, "dockerd_ready", serr.Stage)
	assert.True(t, serr.TimedOut, "the wait is bounded; it must not hang")
	assert.Contains(t, err.Error(), "did not become ready")
	assert.NotNil(t, report)

	// The failed attempt still cleans up after itself.
	_, removed := d.find("rm -f")
	assert.True(t, removed)
}

func TestCreateFailureStages(t *testing.T) {
	cases := map[string]struct {
		match string
		stage string
	}{
		"volume create": {match: "volume create ci-job-ws", stage: "workspace_volume"},
		"image pull":    {match: "docker:test-dind", stage: "image_pull"},
		"container run": {match: "run -d", stage: "container_create"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
				if strings.Contains(strings.Join(args, " "), tc.match) {
					return 1, "", nil
				}
				return 0, "", nil
			}}
			_, _, err := Create(context.Background(), newTestOptions(t, d))
			require.Error(t, err)
			var serr *Error
			require.True(t, errors.As(err, &serr))
			assert.Equal(t, tc.stage, serr.Stage)
		})
	}
}

func TestCreateSurfacesDockerProcessFailure(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		return -1, "", errors.New("exec: \"docker\": executable file not found in $PATH")
	}}
	_, _, err := Create(context.Background(), newTestOptions(t, d))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executable file not found")
}

func TestWriteFileMkdirsBeforeDockerCp(t *testing.T) {
	d := &fakeDocker{}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())

	before := len(d.joined())
	require.NoError(t, c.WriteFile(context.Background(), "/home/runner/work/_temp/step.sh", []byte("echo hi"), 0o700))

	after := d.joined()[before:]
	require.GreaterOrEqual(t, len(after), 2)
	// docker cp does not create parent directories, so the mkdir must come
	// first or the copy fails.
	assert.Contains(t, after[0], "mkdir -p /home/runner/work/_temp")
	assert.Contains(t, after[1], "cp ")
	assert.Contains(t, after[1], "ci-job-42-1:/home/runner/work/_temp/step.sh")
}

func TestCopyIntoUsesDockerCpNotABindMount(t *testing.T) {
	d := &fakeDocker{}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())

	require.NoError(t, c.CopyInto(context.Background(), "/host/action", "/work/_actions/o/p/sha"))
	cp, ok := d.find("cp /host/action/.")
	require.True(t, ok, "calls: %v", d.joined())
	assert.Contains(t, cp, "ci-job-42-1:/work/_actions/o/p/sha")
	_, ok = d.find("mkdir -p /work/_actions/o/p/sha")
	assert.True(t, ok)
}

func TestRunBuildsDockerExec(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		if args[0] == "exec" && len(args) > 3 && args[len(args)-1] == "script.sh" {
			return 7, "output\n", nil
		}
		return 0, "", nil
	}}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())

	var out strings.Builder
	res, err := c.Run(context.Background(), exec.RunRequest{
		Argv:       []string{"bash", "script.sh"},
		Env:        map[string]string{"B": "2", "A": "1"},
		WorkingDir: "/workspace",
		Stdout:     &out,
	})
	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitCode, "a non-zero exit is a value, not an error")
	assert.Equal(t, "output\n", out.String())

	call, ok := d.find("bash script.sh")
	require.True(t, ok)
	assert.Contains(t, call, "-w /workspace")
	// Deterministic env ordering keeps the command reproducible in logs.
	assert.Contains(t, call, "-e A=1 -e B=2")
}

func TestRunRejectsEmptyArgv(t *testing.T) {
	d := &fakeDocker{}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())

	_, err = c.Run(context.Background(), exec.RunRequest{})
	require.Error(t, err)
}

func TestReadFileAndLookPath(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "cat /etc/present"):
			return 0, "contents", nil
		case strings.Contains(joined, "cat /etc/missing"):
			return 1, "", nil
		case strings.Contains(joined, "command -v node"):
			return 0, "/usr/local/bin/node\n", nil
		case strings.Contains(joined, "command -v python"):
			return 1, "", nil
		case strings.Contains(joined, "command -v empty"):
			return 0, "\n", nil
		}
		return 0, "", nil
	}}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())
	ctx := context.Background()

	data, err := c.ReadFile(ctx, "/etc/present")
	require.NoError(t, err)
	assert.Equal(t, "contents", string(data))

	_, err = c.ReadFile(ctx, "/etc/missing")
	require.Error(t, err)

	p, err := c.LookPath(ctx, "node")
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/node", p)

	_, err = c.LookPath(ctx, "python")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not installed in the sandbox image")

	_, err = c.LookPath(ctx, "empty")
	require.Error(t, err, "an empty result is a missing binary, not a found one")

	require.NoError(t, c.RemoveAll(ctx, "/tmp/x"))
	_, ok := d.find("rm -rf /tmp/x")
	assert.True(t, ok)
}

func TestCloseRemovesEverythingAndLogsIt(t *testing.T) {
	d := &fakeDocker{}
	var logs []string
	opts := newTestOptions(t, d)
	opts.Log = func(m string) { logs = append(logs, m) }

	c, _, err := Create(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, c.Close(context.Background()))

	_, ok := d.find("rm -f -v ci-job-42-1")
	assert.True(t, ok)
	_, ok = d.find("volume rm -f ci-job-ws-42-1")
	assert.True(t, ok)

	text := strings.Join(logs, "\n")
	assert.Contains(t, text, "removed container ci-job-42-1")
	assert.Contains(t, text, "removed workspace volume ci-job-ws-42-1")

	// Close is idempotent: a deferred teardown plus an explicit one is normal.
	before := len(d.joined())
	require.NoError(t, c.Close(context.Background()))
	assert.Equal(t, before, len(d.joined()))
}

func TestTeardownFailureIsLoudNotSwallowed(t *testing.T) {
	created := false
	d := &fakeDocker{}
	d.respond = func(args []string, n int) (int, string, error) {
		joined := strings.Join(args, " ")
		if !created {
			return 0, "", nil
		}
		if strings.HasPrefix(joined, "rm -f") || strings.HasPrefix(joined, "volume rm") {
			return 1, "", nil
		}
		return 0, "", nil
	}

	var logs []string
	opts := newTestOptions(t, d)
	opts.Log = func(m string) { logs = append(logs, m) }
	c, _, err := Create(context.Background(), opts)
	require.NoError(t, err)
	created = true

	err = c.Close(context.Background())
	require.Error(t, err, "a teardown failure is reported, never swallowed")
	assert.Contains(t, err.Error(), "removing container")
	assert.Contains(t, err.Error(), "removing workspace volume")
	assert.Contains(t, strings.Join(logs, "\n"), "TEARDOWN FAILED")
}

func TestImageCacheVolumeIsCreatedWhenAbsent(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		if strings.Join(args, " ") == "volume inspect ci-image-cache" {
			return 1, "", nil
		}
		return 0, "", nil
	}}
	c, _, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)
	defer c.Close(context.Background())
	_, ok := d.find("volume create ci-image-cache")
	assert.True(t, ok)
}

func TestConcurrentJobsSerializeOnTheSharedImageCache(t *testing.T) {
	// Two dockerds sharing one /var/lib/docker corrupt it, so the second
	// sandbox must wait rather than quietly share.
	lockDir := t.TempDir()
	d := &fakeDocker{}
	opts := newTestOptions(t, d)
	opts.LockDir = lockDir

	first, _, err := Create(context.Background(), opts)
	require.NoError(t, err)

	second := opts
	second.JobID = 43
	second.SetupTimeout = 100 * time.Millisecond
	_, _, err = Create(context.Background(), second)
	require.Error(t, err)
	var serr *Error
	require.True(t, errors.As(err, &serr))
	assert.Equal(t, "image_cache_lock", serr.Stage)

	require.NoError(t, first.Close(context.Background()))

	// Once released, the next job proceeds.
	third := opts
	third.JobID = 44
	c, _, err := Create(context.Background(), third)
	require.NoError(t, err)
	require.NoError(t, c.Close(context.Background()))
}

func TestCLIRunReportsExitCodeNotError(t *testing.T) {
	cli := &CLI{Binary: "sh"}
	var out strings.Builder
	code, err := cli.Run(context.Background(), Invocation{
		Args: []string{"-c", "echo hello; exit 3"}, Stdout: &out,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, code)
	assert.Equal(t, "hello\n", out.String())
}

func TestCLIRunReportsMissingBinaryAsError(t *testing.T) {
	cli := &CLI{Binary: "definitely-not-a-real-binary-xyz"}
	code, err := cli.Run(context.Background(), Invocation{Args: []string{"info"}})
	require.Error(t, err)
	assert.Equal(t, -1, code)
}

func TestCLIPassesDockerHost(t *testing.T) {
	cli := &CLI{Binary: "sh", Host: "tcp://198.51.100.1:2375"}
	var out strings.Builder
	code, err := cli.Run(context.Background(), Invocation{
		Args: []string{"-c", "printf %s \"$DOCKER_HOST\""}, Stdout: &out,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "tcp://198.51.100.1:2375", out.String())
}

func TestCaptureIncludesStderrInTheError(t *testing.T) {
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		return 2, "", nil
	}}
	_, err := capture(context.Background(), d, "volume", "create", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exited 2")
}

func TestSetupErrorMessage(t *testing.T) {
	e := &Error{Stage: "image_pull", Err: fmt.Errorf("boom")}
	assert.Equal(t, "sandbox image_pull failed: boom", e.Error())
	assert.EqualError(t, errors.Unwrap(e), "boom")
}

func TestReadinessRejectsADaemonThatAnswersEmpty(t *testing.T) {
	// `docker info` exits 0 with "Cannot connect to the Docker daemon" in its
	// output, which is why readiness is probed with `docker version` and an
	// empty server version is treated as not ready.
	d := &fakeDocker{respond: func(args []string, n int) (int, string, error) {
		if isVersionProbe(args) {
			return 0, "\n", nil
		}
		return 0, "", nil
	}}
	opts := newTestOptions(t, d)
	opts.SetupTimeout = 100 * time.Millisecond
	_, _, err := Create(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty server version")
}

func TestEachJobGetsItsOwnNetwork(t *testing.T) {
	d := &fakeDocker{}
	c, report, err := Create(context.Background(), newTestOptions(t, d))
	require.NoError(t, err)

	_, ok := d.find("network create ci-job-net-42-1")
	require.True(t, ok, "calls: %v", d.joined())
	run, _ := d.find("run -d")
	assert.Contains(t, run, "--network ci-job-net-42-1",
		"the DinD entrypoint exposes an unauthenticated API on 2375; on a shared bridge that is cross-job root access")
	assert.Contains(t, report.Breakdown, "network_create")

	require.NoError(t, c.Close(context.Background()))
	_, ok = d.find("network rm ci-job-net-42-1")
	assert.True(t, ok)
}
