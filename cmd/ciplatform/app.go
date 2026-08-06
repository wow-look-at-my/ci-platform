package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/api"
	"github.com/wow-look-at-my/ci-platform/internal/artifacts"
	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/blob/s3"
	"github.com/wow-look-at-my/ci-platform/internal/cachesvc"
	"github.com/wow-look-at-my/ci-platform/internal/config"
	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	ghapp "github.com/wow-look-at-my/ci-platform/internal/github/app"
	"github.com/wow-look-at-my/ci-platform/internal/ingest"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
	"github.com/wow-look-at-my/ci-platform/internal/logstore"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/oidc"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runnerapi"
	"github.com/wow-look-at-my/ci-platform/internal/scheduler"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
	"github.com/wow-look-at-my/ci-platform/internal/store/pg"
	"github.com/wow-look-at-my/ci-platform/internal/webui"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// app holds the wired control plane.
type app struct {
	cfg   *config.Config
	log   *slog.Logger
	store store.Store
	blobs blob.Store
	logs  *logstore.Log
	sched *scheduler.Scheduler
	mux   *http.ServeMux
}

func newApp(ctx context.Context, cfg *config.Config, log *slog.Logger) (*app, error) {
	a := &app{cfg: cfg, log: log, mux: http.NewServeMux()}

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	a.store = st
	if err := st.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	if a.blobs, err = openBlobs(cfg); err != nil {
		return nil, err
	}
	if a.logs, err = logstore.New(logstore.Options{Blob: a.blobs, KeyPrefix: "logs"}); err != nil {
		return nil, fmt.Errorf("log store: %w", err)
	}

	signer, err := jobtoken.New(jobtoken.Options{
		Key:    cfg.JobTokenSecret,
		Issuer: cfg.PublicURL.String(),
		Grace:  10 * time.Minute,
		Lookup: a.jobTokenLookup,
	})
	if err != nil {
		return nil, fmt.Errorf("job tokens: %w", err)
	}

	ghApp, err := ghapp.LoadApp(ghapp.Config{
		AppID:         cfg.AppID,
		PrivateKeyPEM: string(cfg.AppPrivateKey),
		BaseURL:       cfg.GitHubAPIURL.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}

	a.sched = scheduler.New(st, scheduler.Options{
		NewEval:             newEvaluator,
		MintJobToken:        signer.Mint,
		ServiceEnv:          a.serviceEnv,
		Notify:              a.notify,
		LeaseTTL:            cfg.LeaseTTL,
		SetupTimeout:        cfg.SetupTimeout,
		DefaultJobTimeout:   6 * time.Hour,
		RunTimeout:          cfg.RunTimeout,
		ServerURL:           cfg.PublicURL.String(),
		RequireForkApproval: cfg.RequireForkApproval,
	})

	if err := a.mount(ctx, cfg, st, signer, ghApp); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) mount(ctx context.Context, cfg *config.Config, st store.Store, signer *jobtoken.Signer, ghApp *ghapp.App) error {
	// The artifact client refuses to talk to a server whose hostname fails its
	// isGhes() test, so check it here rather than inside somebody's upload.
	if err := artifacts.ValidateServerURL(cfg.PublicURL.String()); err != nil {
		return err
	}

	arts, err := artifacts.New(artifacts.Options{
		Store: st, Blob: a.blobs, Signer: signer,
		BaseURL:              cfg.PublicURL.String(),
		DefaultRetentionDays: int(cfg.ArtifactRetention / (24 * time.Hour)),
		MaxRetentionDays:     int(cfg.ArtifactRetention / (24 * time.Hour)),
		RepoQuotaBytes:       cfg.ArtifactQuota,
		RepoUsage:            st.ArtifactUsage,
		SignedURLTTL:         time.Hour,
	})
	if err != nil {
		return fmt.Errorf("artifact service: %w", err)
	}

	caches, err := cachesvc.New(cachesvc.Options{
		Store: st, Blob: a.blobs, Signer: signer,
		BaseURL:        cfg.PublicURL.String(),
		RepoQuotaBytes: cfg.CacheQuota,
		SignedURLTTL:   time.Hour,
	})
	if err != nil {
		return fmt.Errorf("cache service: %w", err)
	}

	keys, err := oidc.NewFileKeyStore(cfg.OIDCKeyPath)
	if err != nil {
		return fmt.Errorf("oidc key store: %w", err)
	}
	// The keyring serves a retired key for as long as a token it signed can
	// still be valid, so the two TTLs have to agree.
	const idTokenTTL = 15 * time.Minute
	ring, err := oidc.NewKeyring(ctx, keys, oidc.KeyringOptions{TokenTTL: idTokenTTL})
	if err != nil {
		return fmt.Errorf("oidc keyring: %w", err)
	}
	idTokens, err := oidc.New(oidc.Options{
		Issuer:   cfg.PublicURL.String(),
		Keyring:  ring,
		Verifier: signer.Verifier(jobtoken.ScopeOIDCIssue),
		Lookup:   a.oidcLookup,
		TokenTTL: idTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("oidc service: %w", err)
	}

	runnerSrv, err := runnerapi.New(runnerapi.Options{
		Store: st, Scheduler: schedulerAdapter{a.sched}, Logs: a.logs,
		Token:             cfg.RunnerToken,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Logger:            a.log,
	})
	if err != nil {
		return fmt.Errorf("runner api: %w", err)
	}

	ing, err := ingest.New(ingest.Options{
		Store:     st,
		Files:     githubFiles{ghApp},
		Starter:   a.sched,
		NewEval:   newEvaluator,
		ServerURL: cfg.PublicURL.String(),
		Logger:    a.log,
	})
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}

	hooks, err := newWebhookHandler(cfg.WebhookSecret, ing, a.sched, st, a.log)
	if err != nil {
		return fmt.Errorf("webhook handler: %w", err)
	}

	apiSrv := api.New(api.Config{
		Store: st, Controller: a.sched, Logs: a.logs,
		Blobs: blobOpener{a.blobs},
	})

	ui, err := webui.New()
	if err != nil {
		return fmt.Errorf("web ui: %w", err)
	}

	a.mux.Handle("/webhook", hooks)
	a.mux.Handle("/runner/v1/", runnerSrv.Handler())
	a.mux.Handle("/api/v1/", apiSrv.Handler())
	a.mux.Handle("/healthz", apiSrv.Handler())
	a.mux.Handle("/.well-known/docker-updater/health", apiSrv.Handler())
	a.mux.Handle("/.well-known/jwks.json", idTokens.Handler())
	a.mux.Handle("/.well-known/openid-configuration", idTokens.Handler())
	a.mux.Handle(oidc.PathIDToken, idTokens.Handler())
	a.mux.Handle(artifacts.TwirpPrefix, arts.Handler())
	a.mux.Handle(artifacts.PathUpload, arts.Handler())
	a.mux.Handle(artifacts.PathDownload, arts.Handler())
	a.mux.Handle("/_apis/artifactcache/", caches.Handler())
	a.mux.Handle(cachesvc.PathDownload, caches.Handler())
	a.mux.Handle("/", ui)
	return nil
}

// Handler is the whole HTTP surface.
func (a *app) Handler() http.Handler { return a.mux }

// Close releases the store.
func (a *app) Close() {
	if a.store != nil {
		_ = a.store.Close()
	}
}

// RunBackground drives the scheduler's periodic work: lease reaping, timeouts,
// and queue sampling. Tick is idempotent, so a missed cycle costs nothing.
func (a *app) RunBackground(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	sample := time.NewTicker(30 * time.Second)
	defer sample.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.sched.Tick(ctx, time.Now()); err != nil {
				a.log.Error("scheduler tick failed", "err", err)
			}
		case <-sample.C:
			a.recordQueueSample(ctx)
		}
	}
}

func (a *app) recordQueueSample(ctx context.Context) {
	now := time.Now()
	stats, err := a.store.QueueStats(ctx, now)
	if err != nil {
		a.log.Error("queue stats failed", "err", err)
		return
	}
	runners, err := a.store.ListRunners(ctx)
	if err != nil {
		a.log.Error("list runners failed", "err", err)
		return
	}
	var busy, idle int
	for _, r := range runners {
		switch r.State {
		case model.RunnerBusy:
			busy++
		case model.RunnerIdle:
			idle++
		}
	}
	err = a.store.RecordQueueSample(ctx, store.QueueSample{
		At: now, Depth: stats.Depth, DepthByLabel: stats.DepthByLabel, Busy: busy, Idle: idle,
	})
	if err != nil {
		a.log.Error("record queue sample failed", "err", err)
	}
	// A label with queued work and no runner to take it is why a job sits for
	// five minutes with nothing to explain it, so say so rather than leaving
	// the operator to infer it from timestamps.
	if len(stats.StarvedLabels) > 0 {
		a.log.Warn("queued work has no runner to take it",
			"labels", stats.StarvedLabels, "depth", stats.Depth,
			"oldest_waiting", stats.OldestWaiting.String())
	}
}

// notify fires on a default-branch run that did not succeed. This is the alarm
// for the merged, green PR that never published.
func (a *app) notify(ctx context.Context, n scheduler.Notification) {
	a.log.Error("default-branch run did not succeed",
		"repo", n.Repo, "branch", n.Branch, "workflow", n.Workflow,
		"run", n.RunID, "sha", n.HeadSHA, "conclusion", n.Conclusion, "summary", n.Summary)
}

func openStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (store.Store, error) {
	if cfg.DatabaseURL == "memory" {
		if !cfg.AllowEphemeralStore {
			return nil, fmt.Errorf("CIPLATFORM_DATABASE_URL=memory loses every run, job, and queued item on restart; " +
				"set CIPLATFORM_ALLOW_EPHEMERAL_STORE=true to accept that, or point it at Postgres")
		}
		log.Warn("using the in-memory store: nothing survives a restart")
		return mem.New(), nil
	}
	st, err := pg.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return st, nil
}

func openBlobs(cfg *config.Config) (blob.Store, error) {
	switch cfg.BlobDriver {
	case "s3":
		return s3.New(s3.Config{
			Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKeyID: cfg.S3KeyID, SecretAccessKey: cfg.S3Secret, UsePathStyle: true,
		})
	case "disk":
		return disk.New(cfg.BlobRoot)
	default:
		return nil, fmt.Errorf("unknown blob driver %q", cfg.BlobDriver)
	}
}

// newEvaluator adapts the workflow expression evaluator to the plan and
// scheduler's interface.
func newEvaluator(contexts map[string]any, status plan.Status) plan.Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success:   status.Success,
		Failure:   status.Failure,
		Cancelled: status.Cancelled,
	})
}

// schedulerAdapter bridges the scheduler to runnerapi's restated Result type,
// which exists so runnerapi does not depend on the scheduler package.
type schedulerAdapter struct{ s *scheduler.Scheduler }

func (a schedulerAdapter) Acquire(ctx context.Context, runnerID string, labels []string, now time.Time) (*protocol.Assignment, error) {
	return a.s.Acquire(ctx, runnerID, labels, now)
}

func (a schedulerAdapter) JobCompleted(ctx context.Context, jobID int64, res runnerapi.SchedulerResult) error {
	return a.s.JobCompleted(ctx, jobID, scheduler.Result{
		Conclusion:        res.Conclusion,
		Class:             res.Class,
		ClassReason:       res.ClassReason,
		Explanation:       res.Explanation,
		Outputs:           res.Outputs,
		Cancel:            res.Cancel,
		ClassificationLog: res.ClassificationLog,
	})
}

func (a schedulerAdapter) JobSetupCompleted(ctx context.Context, jobID int64, at time.Time) error {
	return a.s.JobSetupCompleted(ctx, jobID, at)
}

func (a schedulerAdapter) ReleaseJob(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	return a.s.ReleaseJob(ctx, runnerID, jobID, reason)
}

// githubFiles reads workflow files with an installation-scoped client.
type githubFiles struct{ app *ghapp.App }

func (g githubFiles) client(installationID int64, repo gh.Repo) (*gh.Client, error) {
	if installationID == 0 {
		return nil, fmt.Errorf("no App installation recorded for %s/%s; the webhook delivery carried none",
			repo.Owner, repo.Name)
	}
	return g.app.InstallationClient(installationID, ghapp.TokenScope{
		Repositories: []string{repo.Name},
		Permissions:  map[string]string{"contents": "read", "checks": "write", "statuses": "write"},
	})
}

func (g githubFiles) ListWorkflowFiles(ctx context.Context, installationID int64, repo gh.Repo, ref string) ([]gh.WorkflowFile, error) {
	c, err := g.client(installationID, repo)
	if err != nil {
		return nil, err
	}
	return c.ListWorkflowFiles(ctx, repo, ref)
}

func (g githubFiles) GetFileContents(ctx context.Context, installationID int64, repo gh.Repo, path, ref string) (*gh.FileContent, error) {
	c, err := g.client(installationID, repo)
	if err != nil {
		return nil, err
	}
	return c.GetFileContents(ctx, repo, path, ref)
}

// blobOpener adapts the blob store to the API's artifact download.
type blobOpener struct{ blobs blob.Store }

func (b blobOpener) Open(ctx context.Context, a *model.Artifact) (io.ReadCloser, error) {
	return b.blobs.Get(ctx, a.StorageKey)
}

// serviceEnv is the environment a job's artifact, cache, and OIDC clients
// discover their endpoints through. Fork PRs get no ID-token variables at all,
// so the endpoint is not merely denied: it is not there to be found.
func (a *app) serviceEnv(runID, jobID int64, attempt int, token string) map[string]string {
	base := a.cfg.PublicURL.String()
	env := map[string]string{}

	retentionDays := int(a.cfg.ArtifactRetention / (24 * time.Hour))
	for k, v := range artifacts.RunnerEnv(base, base, runID, token, retentionDays) {
		env[k] = v
	}

	mode := "read"
	run, err := a.store.GetRun(context.Background(), runID)
	if err == nil && scheduler.OIDCAllowed(run) {
		mode = "write"
		for k, v := range oidc.RunnerEnv(base, token) {
			env[k] = v
		}
	}
	for k, v := range cachesvc.RunnerEnv(base, token, mode) {
		env[k] = v
	}
	return env
}
