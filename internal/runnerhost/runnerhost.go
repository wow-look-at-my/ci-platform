// Package runnerhost supervises a pool of runner containers on one machine.
//
// It exists to answer the questions every self-hosted CI fleet asks and most
// answer badly: how do N runners stay up to date, how does each get storage
// that is genuinely fresh per job and genuinely cleaned up afterwards, and how
// does an update avoid killing a build that is halfway through.
//
// The answers here: one deterministic container per slot, so a supervisor
// restart adopts what is already running instead of doubling it; the container's
// own writable layer as the runner's state, so removing the container is the
// cleanup; and `docker stop` with a long timeout to restart, because the agent
// finishes the job it is holding when it is asked to stop.
//
// see docs/runners.md
package runnerhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/runner/sandbox"
)

// Options configures the supervisor.
type Options struct {
	Docker sandbox.Docker
	// ControlPlaneURL is where the runners get their work. On a LAN this is the
	// coordinator's address directly; over the internet it is the public URL.
	ControlPlaneURL string
	// Runners is how many to keep running.
	Runners int
	Labels  []string
	// Name identifies this machine, and is the prefix of every container and
	// volume it owns.
	Name string
	// HostKeyPath is the identity all this host's runners share, copied into
	// each container rather than passed as an environment variable, so it is
	// not visible in `docker inspect`.
	HostKeyPath string
	// RunnerImage is the agent image. RunnerImageUpdates checks for a newer one.
	RunnerImage string
	// SandboxImage is the Docker-in-Docker image each job runs in.
	SandboxImage string
	// DockerHost is the daemon the runners themselves talk to. Empty means the
	// default socket, mounted into each runner.
	DockerHost string
	// ActionsAPIURL is where `uses:` steps are fetched from.
	ActionsAPIURL string

	// ReconcileInterval is how often the pool is checked against what should be
	// running.
	ReconcileInterval time.Duration
	// UpdateInterval is how often a newer runner image is looked for.
	UpdateInterval time.Duration
	// DrainTimeout bounds how long a runner may take to finish the job it holds
	// when it is asked to stop. It is hours rather than seconds: the whole point
	// is that an update does not kill a build.
	DrainTimeout time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

// Host is the supervisor.
type Host struct {
	opts Options
	log  *slog.Logger
	// imageID is the runner image the pool was last built from. An empty value
	// means it has not been resolved yet.
	imageID string
}

// New validates the options.
func New(opts Options) (*Host, error) {
	switch {
	case opts.Docker == nil:
		return nil, errors.New("runnerhost: Docker is required")
	case strings.TrimSpace(opts.ControlPlaneURL) == "":
		return nil, errors.New("runnerhost: the control plane URL is required")
	case strings.TrimSpace(opts.Name) == "":
		return nil, errors.New("runnerhost: a host name is required; it names every container this manages")
	case strings.TrimSpace(opts.HostKeyPath) == "":
		return nil, errors.New("runnerhost: the host key path is required")
	case strings.TrimSpace(opts.RunnerImage) == "":
		return nil, errors.New("runnerhost: the runner image is required")
	case len(opts.Labels) == 0:
		return nil, errors.New("runnerhost: at least one label is required, or these runners are offered no work")
	case opts.Runners < 1:
		return nil, fmt.Errorf("runnerhost: -runners is %d; a host that runs no runners does nothing", opts.Runners)
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = 15 * time.Second
	}
	if opts.UpdateInterval <= 0 {
		opts.UpdateInterval = 15 * time.Minute
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 6 * time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Host{opts: opts, log: opts.Logger}, nil
}

// container is the deterministic name of one slot's runner.
func (h *Host) container(slot int) string {
	return "ci-runner-" + h.opts.Name + "-" + strconv.Itoa(slot)
}

// imageCacheVolume is the slot's Docker image cache. It deliberately survives a
// runner restart -- it is a cache, and throwing it away means every job after an
// update re-pulls its images -- and is removed only when the slot itself goes.
func (h *Host) imageCacheVolume(slot int) string {
	return h.container(slot) + "-images"
}

// Run supervises the pool until ctx is cancelled.
//
// Cancelling does NOT stop the runners. A supervisor being restarted, by an
// image update or by an operator, must not take running jobs down with it; the
// next supervisor adopts the same containers by name.
func (h *Host) Run(ctx context.Context) error {
	if err := h.reconcile(ctx); err != nil {
		// A first pass that cannot even talk to Docker is a misconfiguration
		// worth failing on, rather than logging every fifteen seconds forever.
		return err
	}

	reconcile := time.NewTicker(h.opts.ReconcileInterval)
	defer reconcile.Stop()
	update := time.NewTicker(h.opts.UpdateInterval)
	defer update.Stop()

	for {
		select {
		case <-ctx.Done():
			h.log.Info("supervisor stopping; runners are left running so no job is interrupted")
			return nil
		case <-reconcile.C:
			if err := h.reconcile(ctx); err != nil && ctx.Err() == nil {
				h.log.Error("reconcile failed", "err", err)
			}
		case <-update.C:
			if err := h.update(ctx); err != nil && ctx.Err() == nil {
				h.log.Error("update check failed", "err", err)
			}
		}
	}
}

// reconcile makes what is running match what should be.
func (h *Host) reconcile(ctx context.Context) error {
	if h.imageID == "" {
		id, err := h.resolveImage(ctx)
		if err != nil {
			return err
		}
		h.imageID = id
	}
	for slot := range h.opts.Runners {
		if err := h.ensureSlot(ctx, slot); err != nil {
			return fmt.Errorf("slot %d: %w", slot, err)
		}
	}
	return h.removeExtraSlots(ctx)
}

// ensureSlot starts the slot's runner if it is not already up.
func (h *Host) ensureSlot(ctx context.Context, slot int) error {
	name := h.container(slot)
	state, err := h.inspect(ctx, name)
	switch {
	case err != nil:
		return err
	case state == "running":
		return nil
	case state == "":
		return h.create(ctx, slot)
	default:
		// Exited, dead, or created-but-never-started. Whatever it was, its
		// storage is not fresh any more, so the container goes and a new one
		// takes its place rather than being restarted in place.
		h.log.Warn("runner is not running; replacing it", "container", name, "state", state)
		if err := h.remove(ctx, name); err != nil {
			return err
		}
		return h.create(ctx, slot)
	}
}

// removeExtraSlots tidies up after -runners is reduced. Each is drained, so
// shrinking a pool does not kill a job either.
func (h *Host) removeExtraSlots(ctx context.Context) error {
	for slot := h.opts.Runners; slot < h.opts.Runners+8; slot++ {
		name := h.container(slot)
		state, err := h.inspect(ctx, name)
		if err != nil {
			return err
		}
		if state == "" {
			continue
		}
		h.log.Info("removing a runner beyond the configured pool size", "container", name)
		if err := h.drain(ctx, name); err != nil {
			return err
		}
		if err := h.remove(ctx, name); err != nil {
			return err
		}
		// The image cache belongs to a slot that no longer exists.
		if _, err := sandbox.Capture(ctx, h.opts.Docker, "volume", "rm", "-f", h.imageCacheVolume(slot)); err != nil {
			h.log.Warn("could not remove the image cache of a removed slot",
				"volume", h.imageCacheVolume(slot), "err", err)
		}
	}
	return nil
}

// update pulls the runner image and, if it changed, rebuilds the pool one slot
// at a time so the host never goes to zero capacity mid-update.
func (h *Host) update(ctx context.Context) error {
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "pull", h.opts.RunnerImage); err != nil {
		return fmt.Errorf("pulling %s: %w", h.opts.RunnerImage, err)
	}
	id, err := h.resolveImage(ctx)
	if err != nil {
		return err
	}
	if id == h.imageID {
		return nil
	}

	h.log.Info("a newer runner image is available; draining and replacing each runner in turn",
		"image", h.opts.RunnerImage, "was", short(h.imageID), "now", short(id),
		"drain_timeout", h.opts.DrainTimeout)
	h.imageID = id
	for slot := range h.opts.Runners {
		name := h.container(slot)
		if err := h.drain(ctx, name); err != nil {
			return err
		}
		if err := h.remove(ctx, name); err != nil {
			return err
		}
		if err := h.create(ctx, slot); err != nil {
			return err
		}
	}
	return nil
}

// drain asks a runner to stop and waits for it. The agent stops taking work and
// finishes the job it holds, so the wait is bounded by the job, not by a signal
// handler; the timeout is the backstop for a runner that is genuinely wedged.
func (h *Host) drain(ctx context.Context, name string) error {
	seconds := int(h.opts.DrainTimeout / time.Second)
	h.log.Info("draining runner", "container", name, "timeout", h.opts.DrainTimeout)
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "stop", "-t", strconv.Itoa(seconds), name); err != nil {
		return fmt.Errorf("draining %s: %w", name, err)
	}
	return nil
}

func (h *Host) remove(ctx context.Context, name string) error {
	// -v removes the container's anonymous volumes with it. The runner's state
	// is meant to die here: fresh storage per runner is the point.
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "rm", "-f", "-v", name); err != nil {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// create builds one runner: a container with no state of its own beyond its
// writable layer, the host key copied in, and the image cache it shares with
// nobody.
func (h *Host) create(ctx context.Context, slot int) error {
	name := h.container(slot)
	cache := h.imageCacheVolume(slot)
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "volume", "create", cache); err != nil {
		return fmt.Errorf("creating image cache volume %s: %w", cache, err)
	}

	args := []string{
		"create", "--name", name,
		"--label", "ci-platform.runner-host=" + h.opts.Name,
		"--label", "ci-platform.slot=" + strconv.Itoa(slot),
		// The supervisor decides when a runner goes away; Docker restarting it
		// behind our back would skip the draining and the fresh storage.
		"--restart", "no",
		"-e", "CI_CONTROL_PLANE_URL=" + h.opts.ControlPlaneURL,
		"-e", "CI_RUNNER_LABELS=" + strings.Join(h.opts.Labels, ","),
		"-e", "CI_RUNNER_NAME=" + h.opts.Name + "-" + strconv.Itoa(slot),
		// The id is stable per slot, so a replaced runner is the same runner to
		// the control plane rather than an ever-growing list of dead ones.
		"-e", "CI_RUNNER_ID=" + name,
		"-e", "CI_RUNNER_STATE_DIR=" + stateDir,
		"-e", "CI_RUNNER_HOST_KEY=" + stateDir + "/host.key",
		"-e", "CI_RUNNER_IMAGE_CACHE_VOLUME=" + cache,
	}
	if h.opts.SandboxImage != "" {
		args = append(args, "-e", "CI_RUNNER_SANDBOX_IMAGE="+h.opts.SandboxImage)
	}
	if h.opts.ActionsAPIURL != "" {
		args = append(args, "-e", "CI_RUNNER_ACTIONS_API_URL="+h.opts.ActionsAPIURL)
	}
	if h.opts.DockerHost != "" {
		args = append(args, "-e", "CI_RUNNER_DOCKER_HOST="+h.opts.DockerHost)
	} else {
		// A runner builds its job sandboxes on the same daemon this supervisor
		// uses, so it needs the socket.
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	args = append(args, h.opts.RunnerImage, "run")

	if _, err := sandbox.Capture(ctx, h.opts.Docker, args...); err != nil {
		return fmt.Errorf("creating %s: %w", name, err)
	}
	// Copied into the stopped container rather than mounted or passed as an
	// environment variable: a bind mount would need a path that means the same
	// thing inside this supervisor and on the host, and an environment variable
	// would put the private key in `docker inspect` output.
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "cp", h.opts.HostKeyPath, name+":"+stateDir+"/host.key"); err != nil {
		return fmt.Errorf("installing the host key into %s: %w", name, err)
	}
	if _, err := sandbox.Capture(ctx, h.opts.Docker, "start", name); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	h.log.Info("runner started", "container", name, "image", h.opts.RunnerImage, "labels", h.opts.Labels)
	return nil
}

// stateDir is where a runner keeps its state inside its own container. It is
// the container's writable layer, so it is gone when the container is.
const stateDir = "/var/lib/ci-runner"

// inspect returns the container's state, or "" when there is no such container.
func (h *Host) inspect(ctx context.Context, name string) (string, error) {
	out, err := sandbox.Capture(ctx, h.opts.Docker, "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		if isNoSuchObject(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspecting %s: %w", name, err)
	}
	return strings.TrimSpace(out), nil
}

// resolveImage reads the id of the runner image, pulling it if it is absent.
func (h *Host) resolveImage(ctx context.Context) (string, error) {
	out, err := sandbox.Capture(ctx, h.opts.Docker, "image", "inspect", "-f", "{{.Id}}", h.opts.RunnerImage)
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	if !isNoSuchObject(err) {
		return "", fmt.Errorf("inspecting %s: %w", h.opts.RunnerImage, err)
	}
	if _, perr := sandbox.Capture(ctx, h.opts.Docker, "pull", h.opts.RunnerImage); perr != nil {
		return "", fmt.Errorf("pulling %s: %w", h.opts.RunnerImage, perr)
	}
	out, err = sandbox.Capture(ctx, h.opts.Docker, "image", "inspect", "-f", "{{.Id}}", h.opts.RunnerImage)
	if err != nil {
		return "", fmt.Errorf("inspecting %s after pulling it: %w", h.opts.RunnerImage, err)
	}
	return strings.TrimSpace(out), nil
}

// isNoSuchObject distinguishes "it is not there" from "Docker is broken". The
// difference decides whether the supervisor creates something or gives up, and
// the CLI only says it in the message.
func isNoSuchObject(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such image")
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
