// Command runner-host keeps a pool of runners on one machine: it generates the
// machine's identity key, enrols it once, and from then on keeps N runners
// running, up to date, and on storage that is fresh per runner.
//
// It is the thing an operator installs. Setting one up is meant to be rare and
// boring: point it at a control plane, read the fingerprint it prints, approve
// that fingerprint in the dashboard, and never think about it again.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/agent"
	"github.com/wow-look-at-my/ci-platform/internal/runner/sandbox"
	"github.com/wow-look-at-my/ci-platform/internal/runnerhost"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// SIGTERM stops the supervisor and deliberately leaves the runners running,
	// so updating this image never interrupts a build.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "runner-host: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	url          string
	name         string
	labels       string
	runners      int
	stateDir     string
	runnerImage  string
	sandboxImage string
	dockerHost   string
	actionsAPI   string
	reconcile    time.Duration
	update       time.Duration
	drain        time.Duration
	showVersion  bool
}

func run(ctx context.Context, args []string) error {
	var c config
	fs := flag.NewFlagSet("runner-host", flag.ContinueOnError)
	fs.StringVar(&c.url, "url", envOr("CI_CONTROL_PLANE_URL", ""),
		"control plane base URL: the coordinator's address on a LAN, or the public URL over the internet (env CI_CONTROL_PLANE_URL)")
	fs.StringVar(&c.name, "name", envOr("CI_RUNNER_HOST_NAME", ""),
		"name for this machine, defaults to the hostname (env CI_RUNNER_HOST_NAME)")
	fs.StringVar(&c.labels, "labels", envOr("CI_RUNNER_LABELS", "self-hosted,linux"),
		"comma-separated labels these runners accept jobs for (env CI_RUNNER_LABELS)")
	fs.IntVar(&c.runners, "runners", envInt("CI_RUNNER_HOST_RUNNERS", 2),
		"how many runners to keep running (env CI_RUNNER_HOST_RUNNERS)")
	fs.StringVar(&c.stateDir, "state-dir", envOr("CI_RUNNER_HOST_STATE_DIR", "/var/lib/ci-runner-host"),
		"where this host's identity key lives; keep it across restarts or the host has to be approved again (env CI_RUNNER_HOST_STATE_DIR)")
	fs.StringVar(&c.runnerImage, "runner-image", envOr("CI_RUNNER_HOST_RUNNER_IMAGE", "oci.pazer.build/ci-platform/runner:latest"),
		"runner image to keep the pool on (env CI_RUNNER_HOST_RUNNER_IMAGE)")
	fs.StringVar(&c.sandboxImage, "sandbox-image", envOr("CI_RUNNER_SANDBOX_IMAGE", ""),
		"Docker-in-Docker image each job runs in (env CI_RUNNER_SANDBOX_IMAGE)")
	fs.StringVar(&c.dockerHost, "docker-host", envOr("CI_RUNNER_HOST_DOCKER_HOST", os.Getenv("DOCKER_HOST")),
		"docker daemon to run the pool on (env CI_RUNNER_HOST_DOCKER_HOST, DOCKER_HOST)")
	fs.StringVar(&c.actionsAPI, "actions-api-url", envOr("CI_RUNNER_ACTIONS_API_URL", "https://api.github.com"),
		"GitHub-compatible API root the runners fetch actions from (env CI_RUNNER_ACTIONS_API_URL)")
	fs.DurationVar(&c.reconcile, "reconcile-interval", envDuration("CI_RUNNER_HOST_RECONCILE_INTERVAL", 15*time.Second),
		"how often the pool is checked against what should be running (env CI_RUNNER_HOST_RECONCILE_INTERVAL)")
	fs.DurationVar(&c.update, "update-interval", envDuration("CI_RUNNER_HOST_UPDATE_INTERVAL", 15*time.Minute),
		"how often to look for a newer runner image (env CI_RUNNER_HOST_UPDATE_INTERVAL)")
	fs.DurationVar(&c.drain, "drain-timeout", envDuration("CI_RUNNER_HOST_DRAIN_TIMEOUT", 6*time.Hour),
		"how long a runner may take to finish the job it is holding when it is replaced (env CI_RUNNER_HOST_DRAIN_TIMEOUT)")
	fs.BoolVar(&c.showVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.showVersion {
		fmt.Printf("runner-host %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return fmt.Errorf("creating -state-dir %s: %w", c.stateDir, err)
	}

	keyPath := filepath.Join(c.stateDir, "host.key")
	if err := c.enrol(ctx, keyPath, log); err != nil {
		return err
	}

	host, err := runnerhost.New(runnerhost.Options{
		Docker:            &sandbox.CLI{Host: c.dockerHost},
		ControlPlaneURL:   c.url,
		Runners:           c.runners,
		Labels:            splitLabels(c.labels),
		Name:              c.name,
		HostKeyPath:       keyPath,
		RunnerImage:       c.runnerImage,
		SandboxImage:      c.sandboxImage,
		DockerHost:        c.dockerHost,
		ActionsAPIURL:     c.actionsAPI,
		ReconcileInterval: c.reconcile,
		UpdateInterval:    c.update,
		DrainTimeout:      c.drain,
		Logger:            log,
	})
	if err != nil {
		return err
	}

	log.Info("supervising runners", "host", c.name, "runners", c.runners,
		"control_plane", c.url, "image", c.runnerImage, "version", version)
	return host.Run(ctx)
}

// enrol offers this machine's key and waits until it is approved.
//
// It runs before any runner starts so the fingerprint is the first thing in the
// log, rather than buried under N runners each reporting the same refusal.
func (c *config) enrol(ctx context.Context, keyPath string, log *slog.Logger) error {
	key, created, err := enrol.LoadOrCreateKey(keyPath)
	if err != nil {
		return err
	}
	creds, err := agent.NewCredentials(agent.CredentialsConfig{
		BaseURL: c.url, Key: key, RunnerID: c.name,
	})
	if err != nil {
		return err
	}
	if created {
		log.Info("generated this host's identity key", "path", keyPath, "fingerprint", creds.Fingerprint())
	}

	resp, err := creds.Enrol(ctx, c.name, runtime.GOOS, runtime.GOARCH, version, splitLabels(c.labels))
	if err != nil {
		return fmt.Errorf("enrolling with %s: %w", c.url, err)
	}
	log.Info("enrolled", "fingerprint", resp.Fingerprint, "state", resp.State, "message", resp.Message)

	for {
		if _, err := creds.Token(ctx); err == nil {
			log.Info("this host is approved", "fingerprint", creds.Fingerprint())
			return nil
		} else if !errors.Is(err, agent.ErrNotApproved) {
			return err
		}
		log.Warn("waiting for an operator to approve this host",
			"fingerprint", creds.Fingerprint(), "control_plane", c.url)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func (c *config) validate() error {
	var missing []string
	if strings.TrimSpace(c.url) == "" {
		missing = append(missing, "-url (or CI_CONTROL_PLANE_URL)")
	}
	if strings.TrimSpace(c.labels) == "" {
		missing = append(missing, "-labels (or CI_RUNNER_LABELS)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.name == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("-name is unset and the hostname could not be read: %w", err)
		}
		c.name = host
	}
	// The name becomes a container and volume name prefix, so it has to be one.
	if strings.ContainsAny(c.name, " /:") {
		return fmt.Errorf("-name %q cannot contain a space, a slash, or a colon: it names this host's containers", c.name)
	}
	if c.runners < 1 {
		return fmt.Errorf("-runners must be at least 1, got %d", c.runners)
	}
	return nil
}

func splitLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt and envDuration reject a value that is set but unparseable rather than
// reverting to the default: a typo an operator cannot see is worse than a stop.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "runner-host: %s=%q is not a number\n", key, v)
		os.Exit(2)
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner-host: %s=%q is not a duration: %v\n", key, v, err)
		os.Exit(2)
	}
	return d
}
