package runnerhost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/runner/sandbox"
)

// fakeDocker records every CLI call and answers the queries the supervisor
// makes. Shelling out to the real docker is not what these tests are about;
// what the supervisor decides to run is.
type fakeDocker struct {
	calls []string
	// state answers `inspect -f {{.State.Status}}` per container name. A name
	// that is absent answers as no such container.
	state map[string]string
	// imageID answers `image inspect`.
	imageID string
	// fail makes any call whose joined args contain this substring fail.
	fail string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{state: map[string]string{}, imageID: "sha256:aaaa"}
}

func (f *fakeDocker) Run(_ context.Context, in sandbox.Invocation) (int, error) {
	joined := strings.Join(in.Args, " ")
	f.calls = append(f.calls, joined)

	if f.fail != "" && strings.Contains(joined, f.fail) {
		writeTo(in.Stderr, "docker: deliberate test failure")
		return 1, nil
	}

	switch {
	case in.Args[0] == "inspect":
		name := in.Args[len(in.Args)-1]
		state, ok := f.state[name]
		if !ok {
			writeTo(in.Stderr, "Error: No such object: "+name)
			return 1, nil
		}
		writeTo(in.Stdout, state+"\n")
	case joined == "image inspect -f {{.Id}} runner:latest":
		writeTo(in.Stdout, f.imageID+"\n")
	case in.Args[0] == "create":
		f.state[nameFlag(in.Args)] = "created"
	case in.Args[0] == "start":
		f.state[in.Args[1]] = "running"
	case in.Args[0] == "stop":
		f.state[in.Args[len(in.Args)-1]] = "exited"
	case in.Args[0] == "rm":
		delete(f.state, in.Args[len(in.Args)-1])
	}
	return 0, nil
}

func writeTo(w io.Writer, s string) {
	if w != nil {
		_, _ = io.WriteString(w, s)
	}
}

func nameFlag(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeDocker) ran(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func testHost(t *testing.T, d sandbox.Docker, mutate ...func(*Options)) *Host {
	t.Helper()
	opts := Options{
		Docker: d, ControlPlaneURL: "http://192.168.1.84:8080", Runners: 2,
		Labels: []string{"self-hosted", "linux"}, Name: "basement",
		HostKeyPath: "/var/lib/ci-runner-host/host.key", RunnerImage: "runner:latest",
		DrainTimeout: 2 * time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mutate {
		m(&opts)
	}
	h, err := New(opts)
	require.NoError(t, err)
	return h
}

func TestNew_RefusesAPoolItCannotRun(t *testing.T) {
	base := Options{
		Docker: newFakeDocker(), ControlPlaneURL: "http://x", Runners: 1,
		Labels: []string{"linux"}, Name: "n", HostKeyPath: "k", RunnerImage: "i",
	}
	_, err := New(base)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		break_ func(*Options)
		want   string
	}{
		{"no docker", func(o *Options) { o.Docker = nil }, "Docker is required"},
		{"no url", func(o *Options) { o.ControlPlaneURL = "" }, "control plane URL is required"},
		{"no name", func(o *Options) { o.Name = "" }, "host name is required"},
		{"no key", func(o *Options) { o.HostKeyPath = "" }, "host key path is required"},
		{"no image", func(o *Options) { o.RunnerImage = "" }, "runner image is required"},
		{"no labels", func(o *Options) { o.Labels = nil }, "at least one label"},
		{"no runners", func(o *Options) { o.Runners = 0 }, "does nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.break_(&o)
			_, err := New(o)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestReconcile_StartsThePool(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d)

	require.NoError(t, h.reconcile(context.Background()))

	assert.Equal(t, "running", d.state["ci-runner-basement-0"])
	assert.Equal(t, "running", d.state["ci-runner-basement-1"])
	assert.True(t, d.ran("volume create ci-runner-basement-0-images"))
	// The key is copied in rather than mounted or handed over in the
	// environment, where `docker inspect` would show it.
	assert.True(t, d.ran("cp /var/lib/ci-runner-host/host.key ci-runner-basement-0:/var/lib/ci-runner/host.key"))
	assert.False(t, d.ran("host.key:/"), "the key must not be bind-mounted")
	assert.True(t, d.ran("CI_CONTROL_PLANE_URL=http://192.168.1.84:8080"))
	assert.True(t, d.ran("--restart no"), "docker restarting a runner would skip the drain and the fresh storage")
}

// A supervisor that is restarted must adopt what is already running, or an
// image update would double the pool every time.
func TestReconcile_AdoptsRunnersThatAreAlreadyUp(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d)
	require.NoError(t, h.reconcile(context.Background()))
	created := d.count("create --name")

	next := testHost(t, d)
	require.NoError(t, next.reconcile(context.Background()))

	assert.Equal(t, created, d.count("create --name"), "a second supervisor created runners again")
}

// A runner that died leaves storage that is no longer fresh, so it is replaced
// rather than restarted in place.
func TestReconcile_ReplacesADeadRunner(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d)
	require.NoError(t, h.reconcile(context.Background()))

	d.state["ci-runner-basement-1"] = "exited"
	d.calls = nil
	require.NoError(t, h.reconcile(context.Background()))

	assert.True(t, d.ran("rm -f -v ci-runner-basement-1"))
	assert.True(t, d.ran("create --name ci-runner-basement-1"))
	assert.Equal(t, "running", d.state["ci-runner-basement-1"])
	assert.False(t, d.ran("rm -f -v ci-runner-basement-0"), "the healthy runner was disturbed")
}

// The whole point of the update path: a build in flight is finished, not killed.
func TestUpdate_DrainsEachRunnerBeforeReplacingIt(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d)
	require.NoError(t, h.reconcile(context.Background()))

	d.imageID = "sha256:bbbb"
	d.calls = nil
	require.NoError(t, h.update(context.Background()))

	// stop -t 7200 is the drain: docker asks politely and waits two hours for
	// the job to finish before it would resort to killing anything.
	assert.True(t, d.ran("stop -t 7200 ci-runner-basement-0"))
	assert.True(t, d.ran("stop -t 7200 ci-runner-basement-1"))
	assert.Equal(t, 2, d.count("create --name"))

	// One at a time: the first runner is back before the second is touched, so
	// the host never drops to no capacity.
	first := indexOf(d.calls, "start ci-runner-basement-0")
	second := indexOf(d.calls, "stop -t 7200 ci-runner-basement-1")
	assert.Less(t, first, second, "both runners were taken down before either came back")
}

func TestUpdate_DoesNothingWhenTheImageIsUnchanged(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d)
	require.NoError(t, h.reconcile(context.Background()))

	d.calls = nil
	require.NoError(t, h.update(context.Background()))

	assert.False(t, d.ran("stop"), "an unchanged image restarted the pool for nothing")
	assert.Equal(t, 0, d.count("create --name"))
}

// Shrinking the pool is still a drain: the runner being removed may be holding
// a job.
func TestReconcile_DrainsASlotRemovedFromThePool(t *testing.T) {
	d := newFakeDocker()
	require.NoError(t, testHost(t, d, func(o *Options) { o.Runners = 3 }).reconcile(context.Background()))

	d.calls = nil
	require.NoError(t, testHost(t, d, func(o *Options) { o.Runners = 1 }).reconcile(context.Background()))

	assert.True(t, d.ran("stop -t 7200 ci-runner-basement-2"))
	assert.True(t, d.ran("rm -f -v ci-runner-basement-2"))
	assert.True(t, d.ran("volume rm -f ci-runner-basement-2-images"))
	assert.NotContains(t, d.state, "ci-runner-basement-1")
	assert.Equal(t, "running", d.state["ci-runner-basement-0"])
}

// Docker being broken and a container being absent are different situations,
// and only one of them means "create it".
func TestReconcile_ReportsADockerFailureRatherThanTreatingItAsAbsent(t *testing.T) {
	d := newFakeDocker()
	d.fail = "inspect -f {{.State.Status}}"
	h := testHost(t, d)

	err := h.reconcile(context.Background())
	require.Error(t, err)
	assert.False(t, d.ran("create --name"), "a broken docker made the supervisor build a second pool")
}

// Cancelling the supervisor must not take the fleet down with it.
func TestRun_LeavesRunnersAloneOnShutdown(t *testing.T) {
	d := newFakeDocker()
	h := testHost(t, d, func(o *Options) { o.ReconcileInterval = time.Hour; o.UpdateInterval = time.Hour })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, h.Run(ctx))

	assert.Equal(t, "running", d.state["ci-runner-basement-0"])
	assert.False(t, d.ran("stop"))
}

func TestRun_FailsFastWhenTheFirstPassCannotWork(t *testing.T) {
	d := newFakeDocker()
	d.fail = "image inspect"
	h := testHost(t, d)

	require.Error(t, h.Run(context.Background()))
}

func TestIsNoSuchObject(t *testing.T) {
	assert.True(t, isNoSuchObject(errors.New("Error: No such object: foo")))
	assert.True(t, isNoSuchObject(errors.New("Error response from daemon: No such container: foo")))
	assert.False(t, isNoSuchObject(errors.New("Cannot connect to the Docker daemon")))
}

func indexOf(calls []string, substr string) int {
	for i, c := range calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}
