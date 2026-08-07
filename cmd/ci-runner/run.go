package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/actions"
	"github.com/wow-look-at-my/ci-platform/internal/runner/agent"
	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
	"github.com/wow-look-at-my/ci-platform/internal/runner/sandbox"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// config is every setting the runner takes, from flags or environment.
type config struct {
	url           string
	token         string
	name          string
	id            string
	labels        string
	group         string
	capacity      int
	stateDir      string
	dockerHost    string
	imageCacheVol string
	sandboxImage  string
	actionsAPI    string
	// actionsToken is a separate credential from the control-plane token: it is
	// sent to an external host on every uses: resolution, so reusing the
	// control-plane token would hand that host our runner credential.
	actionsToken   string
	setupTimeout   time.Duration
	pollWait       time.Duration
	logFlush       time.Duration
	logBatch       int
	sandboxWorkDir string
}

func init() {
	register(&command{
		name:  "run",
		short: "register with the control plane and execute jobs",
		run:   runCommand,
	})
}

func runCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	var c config
	fs.StringVar(&c.url, "url", envOr("CI_CONTROL_PLANE_URL", ""), "control plane base URL (env CI_CONTROL_PLANE_URL)")
	fs.StringVar(&c.token, "token", envOr("CI_RUNNER_TOKEN", ""), "runner registration token (env CI_RUNNER_TOKEN)")
	fs.StringVar(&c.name, "name", envOr("CI_RUNNER_NAME", ""), "runner name, defaults to the hostname (env CI_RUNNER_NAME)")
	fs.StringVar(&c.id, "id", envOr("CI_RUNNER_ID", ""), "stable runner id; generated and persisted in the state dir when unset (env CI_RUNNER_ID)")
	fs.StringVar(&c.labels, "labels", envOr("CI_RUNNER_LABELS", ""), "comma-separated labels this runner accepts jobs for (env CI_RUNNER_LABELS)")
	fs.StringVar(&c.group, "group", envOr("CI_RUNNER_GROUP", ""), "runner group (env CI_RUNNER_GROUP)")
	fs.IntVar(&c.capacity, "capacity", envInt("CI_RUNNER_CAPACITY", 1), "how many jobs to run concurrently (env CI_RUNNER_CAPACITY)")
	fs.StringVar(&c.stateDir, "state-dir", envOr("CI_RUNNER_STATE_DIR", ""), "directory for the idempotency store and action cache (env CI_RUNNER_STATE_DIR)")
	fs.StringVar(&c.dockerHost, "docker-host", envOr("CI_RUNNER_DOCKER_HOST", os.Getenv("DOCKER_HOST")), "outer docker daemon (env CI_RUNNER_DOCKER_HOST, DOCKER_HOST)")
	fs.StringVar(&c.imageCacheVol, "image-cache-volume", envOr("CI_RUNNER_IMAGE_CACHE_VOLUME", ""), "shared docker volume for the inner daemon's image store (env CI_RUNNER_IMAGE_CACHE_VOLUME)")
	fs.StringVar(&c.sandboxImage, "sandbox-image", envOr("CI_RUNNER_SANDBOX_IMAGE", sandbox.DefaultImage), "Docker-in-Docker image (env CI_RUNNER_SANDBOX_IMAGE)")
	fs.StringVar(&c.actionsAPI, "actions-api-url", envOr("CI_RUNNER_ACTIONS_API_URL", ""), "GitHub-compatible API root used to fetch actions (env CI_RUNNER_ACTIONS_API_URL)")
	fs.StringVar(&c.actionsToken, "actions-token", envOr("CI_RUNNER_ACTIONS_TOKEN", ""), "credential for the actions API; never the control-plane token (env CI_RUNNER_ACTIONS_TOKEN)")
	fs.DurationVar(&c.setupTimeout, "setup-timeout", envDuration("CI_RUNNER_SETUP_TIMEOUT", 5*time.Minute), "bound on sandbox setup before it fails as infra (env CI_RUNNER_SETUP_TIMEOUT)")
	fs.DurationVar(&c.pollWait, "poll-wait", envDuration("CI_RUNNER_POLL_WAIT", 30*time.Second), "how long an acquire long-poll is held open (env CI_RUNNER_POLL_WAIT)")
	fs.DurationVar(&c.logFlush, "log-flush-interval", envDuration("CI_RUNNER_LOG_FLUSH_INTERVAL", 2*time.Second), "log batching interval (env CI_RUNNER_LOG_FLUSH_INTERVAL)")
	fs.IntVar(&c.logBatch, "log-batch-size", envInt("CI_RUNNER_LOG_BATCH_SIZE", 200), "lines per log batch (env CI_RUNNER_LOG_BATCH_SIZE)")
	fs.StringVar(&c.sandboxWorkDir, "workspace-dir", envOr("CI_RUNNER_WORKSPACE_DIR", "/workspace"), "workspace path inside the job container (env CI_RUNNER_WORKSPACE_DIR)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := c.validate(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(c.stateDir, 0o755); err != nil {
		return fmt.Errorf("creating -state-dir %s: %w", c.stateDir, err)
	}
	id, err := c.resolveID()
	if err != nil {
		return err
	}

	client, err := agent.NewClient(agent.ClientConfig{BaseURL: c.url, Token: c.token})
	if err != nil {
		return err
	}

	// Declared as the interface: a typed-nil *Resolver would satisfy the
	// agent's nil check and then panic on first use.
	var resolver exec.ActionResolver
	if c.actionsAPI != "" {
		resolver = actions.NewResolver(filepath.Join(c.stateDir, "actions"),
			actions.NewHTTPFetcher(c.actionsAPI, c.actionsToken))
	} else {
		log.Warn("no -actions-api-url configured: a step using `uses:` will fail as a config error naming the reference")
	}

	ag, err := agent.New(agent.Config{
		Client:           client,
		RunnerID:         id,
		Name:             c.name,
		Labels:           splitLabels(c.labels),
		Group:            c.group,
		Capacity:         c.capacity,
		StateDir:         c.stateDir,
		Version:          version,
		NewSandbox:       c.sandboxFactory(log),
		Actions:          resolver,
		NewEvaluator:     newEvaluator,
		PollWait:         c.pollWait,
		LogFlushInterval: c.logFlush,
		LogBatchSize:     c.logBatch,
		Logger:           log,
	})
	if err != nil {
		return err
	}
	defer ag.Close()

	log.Info("starting", "runner", id, "name", c.name, "labels", splitLabels(c.labels),
		"capacity", c.capacity, "control_plane", c.url, "api_version", protocol.APIVersion)
	return ag.Run(ctx)
}

// validate rejects a half-configured runner at startup, naming the flag. There
// is no mode where the runner starts without knowing where to get work.
func (c *config) validate() error {
	var missing []string
	if strings.TrimSpace(c.url) == "" {
		missing = append(missing, "-url (or CI_CONTROL_PLANE_URL)")
	}
	if strings.TrimSpace(c.token) == "" {
		missing = append(missing, "-token (or CI_RUNNER_TOKEN)")
	}
	if strings.TrimSpace(c.stateDir) == "" {
		missing = append(missing, "-state-dir (or CI_RUNNER_STATE_DIR)")
	}
	if strings.TrimSpace(c.labels) == "" {
		missing = append(missing, "-labels (or CI_RUNNER_LABELS)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.capacity < 1 {
		return fmt.Errorf("-capacity must be at least 1, got %d", c.capacity)
	}
	if c.name == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("-name is unset and the hostname could not be read: %w", err)
		}
		c.name = host
	}
	return nil
}

// resolveID keeps the runner's identity stable across restarts, so the control
// plane sees one runner rather than a new one per process.
func (c *config) resolveID() (string, error) {
	if c.id != "" {
		return c.id, nil
	}
	path := filepath.Join(c.stateDir, "runner-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	id := uuid.NewString()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return id, nil
}

// sandboxFactory builds one Docker-in-Docker sandbox per job.
func (c *config) sandboxFactory(log *slog.Logger) agent.SandboxFactory {
	return func(ctx context.Context, a *protocol.Assignment) (agent.Sandbox, *agent.SetupReport, error) {
		timeout := c.setupTimeout
		if d := a.SetupTimeout.D(); d > 0 {
			timeout = d
		}
		box, report, err := sandbox.Create(ctx, sandbox.Options{
			JobID:            a.JobID,
			Attempt:          a.Attempt,
			Image:            c.sandboxImage,
			DockerHost:       c.dockerHost,
			ImageCacheVolume: c.imageCacheVol,
			LockDir:          c.stateDir,
			WorkspaceDir:     c.sandboxWorkDir,
			SetupTimeout:     timeout,
			Log:              func(msg string) { log.Info(msg, "job", a.JobID, "attempt", a.Attempt) },
		})
		if err != nil {
			return nil, toAgentReport(report), err
		}
		return box, toAgentReport(report), nil
	}
}

func toAgentReport(r *sandbox.SetupReport) *agent.SetupReport {
	if r == nil {
		return nil
	}
	return &agent.SetupReport{Breakdown: r.Breakdown, CacheWarm: r.CacheWarm, Total: r.Total}
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

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		// A set-but-unparseable value is a typo the operator needs to see, not
		// a silent revert to the default.
		fmt.Fprintf(os.Stderr, "ci-runner: %s=%q is not a number\n", key, v)
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
		fmt.Fprintf(os.Stderr, "ci-runner: %s=%q is not a duration: %v\n", key, v, err)
		os.Exit(2)
	}
	return d
}

// newEvaluator adapts the workflow expression evaluator to the executor's
// interface. A step's `if:` is evaluated here rather than by the control plane
// because it depends on the outcome of earlier steps in the same job.
func newEvaluator(contexts map[string]any, status exec.Status) exec.Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success:   status.Success,
		Failure:   status.Failure,
		Cancelled: status.Cancelled,
	})
}
