// Package sandbox builds one Docker-in-Docker container per job: a privileged
// container running its own dockerd, with its own image store and network, so
// nothing a job builds or pulls is visible to the next one.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

// DefaultImage is the DinD image used when none is configured.
const DefaultImage = "docker:27-dind"

// Options configures one job's sandbox.
type Options struct {
	JobID   int64
	Attempt int

	// Image is the Docker-in-Docker image to run.
	Image string
	// DockerHost is the OUTER daemon. The job container never sees it.
	DockerHost string
	// ImageCacheVolume is the shared volume mounted at the inner daemon's
	// /var/lib/docker, so a cold pull is paid once per runner rather than once
	// per job.
	ImageCacheVolume string
	// LockDir holds the cache volume's lock file. Two dockerds sharing one
	// graph directory corrupt it, so the volume is used under an exclusive
	// lock rather than silently shared.
	LockDir string

	// WorkspaceDir and TempDir are paths inside the container.
	WorkspaceDir string
	TempDir      string

	// SetupTimeout bounds the whole setup phase. Exceeding it is an infra
	// failure; there is no path where setup hangs forever.
	SetupTimeout time.Duration
	// ReadyPoll is how often the inner dockerd is probed.
	ReadyPoll time.Duration

	// Docker is the CLI wrapper; a fake in tests.
	Docker Docker
	// Log receives platform-level narration of setup and teardown.
	Log func(string)
	// NamePrefix names the container and workspace volume.
	NamePrefix string
}

func (o *Options) applyDefaults() {
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.WorkspaceDir == "" {
		o.WorkspaceDir = "/workspace"
	}
	if o.TempDir == "" {
		o.TempDir = "/home/runner/work/_temp"
	}
	if o.SetupTimeout <= 0 {
		o.SetupTimeout = 5 * time.Minute
	}
	if o.ReadyPoll <= 0 {
		o.ReadyPoll = 500 * time.Millisecond
	}
	if o.NamePrefix == "" {
		o.NamePrefix = "ci-job"
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.Docker == nil {
		o.Docker = &CLI{Host: o.DockerHost}
	}
}

// SetupReport is the measured cost of building the sandbox, so "setup took
// 5m30s" is a number an operator can read rather than infer.
type SetupReport struct {
	Breakdown map[string]time.Duration
	CacheWarm bool
	Total     time.Duration
}

// Stage is one named phase of setup, for building the breakdown.
type stopwatch struct {
	report *SetupReport
	start  time.Time
}

func (r *SetupReport) begin() stopwatch { return stopwatch{report: r, start: time.Now()} }

func (s stopwatch) mark(name string) {
	s.report.Breakdown[name] = time.Since(s.start)
}

// Error is a setup or teardown failure with the stage that produced it, so the
// classifier is given a phase and the log names what broke.
type Error struct {
	Stage    string
	Err      error
	TimedOut bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("sandbox %s failed: %v", e.Stage, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Container is a live job sandbox. It implements exec.Sandbox.
type Container struct {
	opts       Options
	name       string
	workspace  string // volume name
	network    string
	lock       *fileLock
	removed    bool
	hostTmpDir string
	// Only what was actually created is removed, so teardown never reports a
	// failure for a resource that never existed.
	madeWorkspace bool
	madeNetwork   bool
	madeContainer bool
}

var _ exec.Sandbox = (*Container)(nil)

// Name is the container name, for logs and for an operator to attach to.
func (c *Container) Name() string { return c.name }

// WorkspaceDir is the job's working directory inside the container.
func (c *Container) WorkspaceDir() string { return c.opts.WorkspaceDir }

// TempDir is where env files and step scripts are written inside the container.
func (c *Container) TempDir() string { return c.opts.TempDir }

// Create builds the sandbox and blocks until the inner dockerd answers. Every
// failure is an *Error naming the stage; a partially built sandbox is torn down
// before returning so a failed setup never leaks a container or a volume.
func Create(ctx context.Context, opts Options) (_ *Container, report *SetupReport, err error) {
	opts.applyDefaults()
	report = &SetupReport{Breakdown: map[string]time.Duration{}}
	total := time.Now()
	defer func() { report.Total = time.Since(total) }()

	ctx, cancel := context.WithTimeout(ctx, opts.SetupTimeout)
	defer cancel()

	// Deliberately a local, not the named return: returning nil on failure must
	// not blind the cleanup defer to the container it has to remove.
	c := &Container{
		opts:      opts,
		name:      fmt.Sprintf("%s-%d-%d", opts.NamePrefix, opts.JobID, opts.Attempt),
		workspace: fmt.Sprintf("%s-ws-%d-%d", opts.NamePrefix, opts.JobID, opts.Attempt),
		network:   fmt.Sprintf("%s-net-%d-%d", opts.NamePrefix, opts.JobID, opts.Attempt),
	}
	defer func() {
		if err != nil {
			// A half-built sandbox is torn down here; the caller only ever
			// holds a container it can use.
			_ = c.Close(context.WithoutCancel(ctx))
		}
	}()

	if opts.ImageCacheVolume != "" {
		sw := report.begin()
		lock, lerr := acquireLock(ctx, filepath.Join(opts.LockDir, opts.ImageCacheVolume+".lock"), opts.ReadyPoll)
		if lerr != nil {
			return nil, report, setupErr(ctx, "image_cache_lock", lerr)
		}
		c.lock = lock
		if _, ierr := capture(ctx, opts.Docker, "volume", "inspect", opts.ImageCacheVolume); ierr != nil {
			if _, cerr := capture(ctx, opts.Docker, "volume", "create", opts.ImageCacheVolume); cerr != nil {
				return nil, report, setupErr(ctx, "image_cache_volume", cerr)
			}
			opts.Log(fmt.Sprintf("created image cache volume %s", opts.ImageCacheVolume))
		}
		sw.mark("image_cache_volume")
	}

	sw := report.begin()
	if _, cerr := capture(ctx, opts.Docker, "volume", "create", c.workspace); cerr != nil {
		return nil, report, setupErr(ctx, "workspace_volume", cerr)
	}
	c.madeWorkspace = true
	sw.mark("workspace_volume")

	sw = report.begin()
	// Pull only when the image is absent. An unconditional pull makes every job
	// depend on the registry being reachable, so a rate-limited registry fails
	// a job whose image is already on the host.
	if _, ierr := capture(ctx, opts.Docker, "image", "inspect", opts.Image); ierr != nil {
		if _, perr := capture(ctx, opts.Docker, "pull", opts.Image); perr != nil {
			return nil, report, setupErr(ctx, "image_pull", perr)
		}
		opts.Log("pulled sandbox image " + opts.Image)
	} else {
		opts.Log("sandbox image " + opts.Image + " already present; not pulled")
	}
	sw.mark("image_pull")

	sw = report.begin()
	// A network of its own. The Docker-in-Docker entrypoint always publishes an
	// unauthenticated daemon API on 2375 inside the container; on the shared
	// default bridge that is root access to this job from every other job.
	if _, nerr := capture(ctx, opts.Docker, "network", "create", c.network); nerr != nil {
		return nil, report, setupErr(ctx, "network_create", nerr)
	}
	c.madeNetwork = true
	sw.mark("network_create")

	sw = report.begin()
	args := []string{
		"run", "-d",
		"--name", c.name,
		"--network", c.network,
		// Privileged is what makes an inner dockerd possible at all; the
		// isolation comes from it being a throwaway container with its own
		// image store and network, not from dropping privileges.
		"--privileged",
		// TLS between the job and its own local daemon buys nothing and adds a
		// certificate dance to every readiness probe.
		"-e", "DOCKER_TLS_CERTDIR=",
		"-v", c.workspace + ":" + opts.WorkspaceDir,
	}
	if opts.ImageCacheVolume != "" {
		args = append(args, "-v", opts.ImageCacheVolume+":/var/lib/docker")
	}
	// No -v /var/run/docker.sock and no control-plane credentials: a job can
	// reach only its own inner daemon.
	args = append(args, opts.Image)
	if _, rerr := capture(ctx, opts.Docker, args...); rerr != nil {
		return nil, report, setupErr(ctx, "container_create", rerr)
	}
	c.madeContainer = true
	sw.mark("container_create")

	sw = report.begin()
	if derr := c.waitForDockerd(ctx); derr != nil {
		return nil, report, setupErr(ctx, "dockerd_ready", derr)
	}
	sw.mark("dockerd_ready")

	sw = report.begin()
	if merr := c.MkdirAll(ctx, opts.WorkspaceDir); merr != nil {
		return nil, report, setupErr(ctx, "workspace_prepare", merr)
	}
	if merr := c.MkdirAll(ctx, opts.TempDir); merr != nil {
		return nil, report, setupErr(ctx, "workspace_prepare", merr)
	}
	sw.mark("workspace_prepare")

	sw = report.begin()
	report.CacheWarm = c.probeCache(ctx)
	sw.mark("image_cache_probe")

	opts.Log(fmt.Sprintf("sandbox %s ready in %s (cache %s)", c.name,
		report.Total.Round(time.Millisecond), warmWord(report.CacheWarm)))
	return c, report, nil
}

func warmWord(warm bool) string {
	if warm {
		return "warm"
	}
	return "cold"
}

func setupErr(ctx context.Context, stage string, err error) *Error {
	return &Error{Stage: stage, Err: err, TimedOut: ctx.Err() != nil}
}

// waitForDockerd polls `docker info` inside the container until the inner
// daemon answers or the setup deadline passes. The bound is what stops a job
// from sitting in setup forever.
func (c *Container) waitForDockerd(ctx context.Context) error {
	var last error
	for {
		// `docker version` is the probe, not `docker info`: info exits 0 with
		// "Cannot connect to the Docker daemon" in its output, so probing with
		// it reports a dead daemon as ready.
		out, err := capture(ctx, c.opts.Docker, "exec", c.name, "docker", "version", "--format", "{{.Server.Version}}")
		switch {
		case err != nil:
			last = err
		case strings.TrimSpace(out) == "":
			last = fmt.Errorf("the inner daemon reported an empty server version")
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the inner docker daemon did not become ready within %s: %w",
				c.opts.SetupTimeout, last)
		case <-time.After(c.opts.ReadyPoll):
		}
	}
}

// probeCache asks the inner daemon whether it already has images. This is the
// honest reading of "warm": a volume that exists but holds nothing is cold.
func (c *Container) probeCache(ctx context.Context) bool {
	if c.opts.ImageCacheVolume == "" {
		return false
	}
	out, err := capture(ctx, c.opts.Docker, "exec", c.name, "docker", "image", "ls", "-q")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// Run executes a command inside the sandbox.
func (c *Container) Run(ctx context.Context, req exec.RunRequest) (exec.RunResult, error) {
	if len(req.Argv) == 0 {
		return exec.RunResult{}, fmt.Errorf("sandbox run: empty argv")
	}
	args := []string{"exec"}
	if req.WorkingDir != "" {
		args = append(args, "-w", req.WorkingDir)
	}
	for _, k := range sortedKeys(req.Env) {
		args = append(args, "-e", k+"="+req.Env[k])
	}
	args = append(args, c.name)
	args = append(args, req.Argv...)

	code, err := c.opts.Docker.Run(ctx, Invocation{
		Args:   args,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
	})
	if err != nil {
		return exec.RunResult{ExitCode: -1}, err
	}
	return exec.RunResult{ExitCode: code}, nil
}

// WriteFile places a file inside the sandbox.
func (c *Container) WriteFile(ctx context.Context, dest string, data []byte, mode fs.FileMode) error {
	tmp, err := c.hostTemp()
	if err != nil {
		return err
	}
	local := filepath.Join(tmp, "upload")
	if err := os.WriteFile(local, data, mode); err != nil {
		return err
	}
	defer os.Remove(local)
	if err := c.MkdirAll(ctx, path.Dir(dest)); err != nil {
		return err
	}
	return c.copyIn(ctx, local, dest)
}

// CopyInto places a host directory's contents at containerPath.
func (c *Container) CopyInto(ctx context.Context, hostDir, containerPath string) error {
	if err := c.MkdirAll(ctx, containerPath); err != nil {
		return err
	}
	return c.copyIn(ctx, strings.TrimSuffix(hostDir, "/")+"/.", containerPath)
}

// copyIn is the only way files get into the sandbox.
//
// A bind mount cannot be used: a file bind-mounted INTO this Docker-in-Docker
// container cannot be bind-mounted again into a container the inner dockerd
// spawns, because the inner daemon resolves the source path against the HOST
// filesystem, where it does not exist. `docker cp` copies the bytes into the
// container's own filesystem instead, which the inner daemon can then mount.
// The destination's parent must already exist: `docker cp` does not create
// parent directories, so every caller mkdir -p's first.
func (c *Container) copyIn(ctx context.Context, hostPath, dest string) error {
	_, err := capture(ctx, c.opts.Docker, "cp", hostPath, c.name+":"+dest)
	return err
}

// ReadFile reads a file back out of the sandbox.
func (c *Container) ReadFile(ctx context.Context, src string) ([]byte, error) {
	var out, errBuf bytes.Buffer
	code, err := c.opts.Docker.Run(ctx, Invocation{
		Args:   []string{"exec", c.name, "cat", src},
		Stdout: &out,
		Stderr: &errBuf,
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("reading %s from sandbox: %s", src, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// MkdirAll creates a directory inside the sandbox.
func (c *Container) MkdirAll(ctx context.Context, dir string) error {
	_, err := capture(ctx, c.opts.Docker, "exec", c.name, "mkdir", "-p", dir)
	return err
}

// RemoveAll deletes a path inside the sandbox.
func (c *Container) RemoveAll(ctx context.Context, target string) error {
	_, err := capture(ctx, c.opts.Docker, "exec", c.name, "rm", "-rf", target)
	return err
}

// LookPath resolves a binary on the sandbox's PATH. A missing binary is an
// error naming it, never an empty result the caller might treat as fine.
func (c *Container) LookPath(ctx context.Context, bin string) (string, error) {
	out, err := capture(ctx, c.opts.Docker, "exec", c.name, "sh", "-c", "command -v "+bin)
	if err != nil {
		return "", fmt.Errorf("%q is not installed in the sandbox image %s: %w", bin, c.opts.Image, err)
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return "", fmt.Errorf("%q is not installed in the sandbox image %s", bin, c.opts.Image)
	}
	return p, nil
}

// Close tears the sandbox down: the container, then the per-job workspace
// volume, then the cache lock. It runs every time, and every removal is logged
// with its result. A teardown failure is reported loudly and never swallowed,
// because a leaked container silently eats the next job's disk.
func (c *Container) Close(ctx context.Context) error {
	if c == nil || c.removed {
		return nil
	}
	c.removed = true
	var errs []string

	remove := func(made bool, what, name string, args ...string) {
		if !made {
			return
		}
		if _, err := capture(ctx, c.opts.Docker, args...); err != nil {
			errs = append(errs, fmt.Sprintf("removing %s %s: %v", what, name, err))
			c.opts.Log(fmt.Sprintf("TEARDOWN FAILED: could not remove %s %s: %v", what, name, err))
			return
		}
		c.opts.Log(fmt.Sprintf("removed %s %s", what, name))
	}

	// Container first: the network and volume it holds cannot be removed while
	// it is attached.
	remove(c.madeContainer, "container", c.name, "rm", "-f", "-v", c.name)
	remove(c.madeWorkspace, "workspace volume", c.workspace, "volume", "rm", "-f", c.workspace)
	remove(c.madeNetwork, "network", c.network, "network", "rm", c.network)

	if c.hostTmpDir != "" {
		if err := os.RemoveAll(c.hostTmpDir); err != nil {
			errs = append(errs, fmt.Sprintf("removing host staging dir: %v", err))
		}
	}
	if c.lock != nil {
		if err := c.lock.release(); err != nil {
			errs = append(errs, fmt.Sprintf("releasing image cache lock: %v", err))
		}
		c.lock = nil
	}

	if len(errs) > 0 {
		return fmt.Errorf("sandbox teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (c *Container) hostTemp() (string, error) {
	if c.hostTmpDir != "" {
		return c.hostTmpDir, nil
	}
	dir, err := os.MkdirTemp("", "ci-sandbox-")
	if err != nil {
		return "", err
	}
	c.hostTmpDir = dir
	return dir, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// discard is used where a writer is required but the output is not wanted.
var discard io.Writer = io.Discard
