// Package store defines the persistence contract for run metadata, the durable
// job queue, and the lease protocol that makes a lost runner a requeue rather
// than a failure.
//
// Two implementations exist: store/pg (Postgres, the production store) and
// store/mem (in-process, for tests and for a dev instance that says so loudly).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// ErrNotFound is returned by every getter for a missing row.
var ErrNotFound = errors.New("store: not found")

// ErrLeaseLost is returned when a lease operation is attempted by a runner that
// no longer holds the lease. The runner must stop work; the job has already
// been requeued elsewhere.
var ErrLeaseLost = errors.New("store: lease lost")

// ErrConflict is returned when an optimistic update loses a race.
var ErrConflict = errors.New("store: conflict")

// Store is the whole persistence surface. It is one interface rather than
// several because every implementation implements all of it, and splitting it
// would only spread the transaction boundary across packages.
type Store interface {
	Repos
	Runs
	Jobs
	Steps
	Queue
	Runners
	Annotations
	Artifacts
	Caches
	Secrets
	Events

	// Durable reports whether a control-plane restart preserves state. A false
	// answer is surfaced in /healthz and logged at startup, because a queue
	// that silently forgets is the failure mode this platform exists to avoid.
	Durable() bool
	// Migrate applies pending schema migrations.
	Migrate(ctx context.Context) error
	Close() error
}

// Repos stores the repositories the App is installed on.
type Repos interface {
	UpsertRepo(ctx context.Context, r *model.Repo) error
	GetRepo(ctx context.Context, id int64) (*model.Repo, error)
	GetRepoByName(ctx context.Context, owner, name string) (*model.Repo, error)
	ListRepos(ctx context.Context) ([]*model.Repo, error)
}

// RunFilter narrows a run listing for the run list page.
type RunFilter struct {
	RepoID     int64
	Branch     string
	Actor      string
	Event      string
	Status     model.Status
	Conclusion model.Conclusion
	Workflow   string
	Search     string
	Limit      int
	Offset     int
}

// Runs stores workflow runs.
type Runs interface {
	CreateRun(ctx context.Context, r *model.Run) error
	GetRun(ctx context.Context, id int64) (*model.Run, error)
	UpdateRun(ctx context.Context, r *model.Run) error
	ListRuns(ctx context.Context, f RunFilter) ([]*model.Run, error)
	CountRuns(ctx context.Context, f RunFilter) (int, error)
	// NextRunNumber allocates the per-repo, per-workflow run number.
	NextRunNumber(ctx context.Context, repoID int64, workflowPath string) (int64, error)
	// ListRunsForSHA finds every run against a commit, for check_suite rollup.
	ListRunsForSHA(ctx context.Context, repoID int64, sha string) ([]*model.Run, error)
}

// Jobs stores jobs.
type Jobs interface {
	CreateJob(ctx context.Context, j *model.Job) error
	GetJob(ctx context.Context, id int64) (*model.Job, error)
	UpdateJob(ctx context.Context, j *model.Job) error
	ListJobsForRun(ctx context.Context, runID int64) ([]*model.Job, error)
	// ListJobsInConcurrencyGroup returns live jobs sharing a group, oldest
	// first, so the scheduler can admit one and cancel or hold the rest.
	ListJobsInConcurrencyGroup(ctx context.Context, group string) ([]*model.Job, error)
}

// Steps stores per-attempt step rows.
type Steps interface {
	UpsertStep(ctx context.Context, s *model.Step) error
	ListSteps(ctx context.Context, jobID int64, attempt int) ([]*model.Step, error)
}

// QueuedJob is a job waiting for a runner, with the fields the dispatcher needs
// without loading the whole job.
type QueuedJob struct {
	JobID    int64
	RunID    int64
	Labels   []string
	Group    string
	QueuedAt time.Time
	// NotBefore holds a retry's backoff delay.
	NotBefore time.Time
	Priority  int
}

// QueueStats is the queue page's data.
type QueueStats struct {
	Depth          int            `json:"depth"`
	DepthByLabel   map[string]int `json:"depth_by_label"`
	OldestWaiting  time.Duration  `json:"oldest_waiting"`
	OldestJobID    int64          `json:"oldest_job_id,omitempty"`
	RunnersByLabel map[string]int `json:"runners_by_label"`
	IdleByLabel    map[string]int `json:"idle_by_label"`
	// StarvedLabels are labels with queued work and zero online runners. This
	// is the signal that explains "why has this been queued for five minutes".
	StarvedLabels []string  `json:"starved_labels"`
	At            time.Time `json:"at"`
}

// Queue is the durable job queue and the lease protocol.
//
// Dispatch is idempotent on (run_id, job_id, attempt): a job is never executed
// twice with side effects, even if the control plane restarts between handing
// out a lease and recording that it did.
type Queue interface {
	// Enqueue makes a job eligible for dispatch. Enqueuing a job that is
	// already queued or leased is a no-op, not an error.
	Enqueue(ctx context.Context, q QueuedJob) error
	// Dequeue atomically claims the highest-priority eligible job matching the
	// runner's labels and returns it with a lease held until now+ttl. Returns
	// ErrNotFound when nothing matches.
	Dequeue(ctx context.Context, runnerID string, labels []string, ttl time.Duration) (*model.Job, error)
	// Heartbeat extends the lease. ErrLeaseLost means the job was requeued.
	Heartbeat(ctx context.Context, runnerID string, jobID int64, ttl time.Duration) error
	// ReleaseLease drops a lease without completing the job, requeuing it.
	ReleaseLease(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error
	// ReapExpiredLeases requeues every job whose lease expired and returns
	// them. This is what turns "the runner disappeared" into a requeue rather
	// than a lost or failed job.
	ReapExpiredLeases(ctx context.Context, now time.Time) ([]*model.Job, error)
	// QueueStats powers the queue page and the starvation alarm.
	QueueStats(ctx context.Context, now time.Time) (*QueueStats, error)
	// QueueDepthHistory returns sampled depth over time for the queue chart.
	QueueDepthHistory(ctx context.Context, since time.Time) ([]QueueSample, error)
	// RecordQueueSample appends one depth sample.
	RecordQueueSample(ctx context.Context, s QueueSample) error
}

// QueueSample is one point on the queue-depth chart.
type QueueSample struct {
	At           time.Time      `json:"at"`
	Depth        int            `json:"depth"`
	DepthByLabel map[string]int `json:"depth_by_label,omitempty"`
	Busy         int            `json:"busy"`
	Idle         int            `json:"idle"`
}

// Runners stores the agent fleet.
type Runners interface {
	RegisterRunner(ctx context.Context, r *model.Runner) error
	RunnerHeartbeat(ctx context.Context, id string, at time.Time) error
	GetRunner(ctx context.Context, id string) (*model.Runner, error)
	ListRunners(ctx context.Context) ([]*model.Runner, error)
	// MarkOfflineRunners flips runners past the deadline to offline and returns
	// them, so their in-flight jobs can be requeued with a recorded reason.
	MarkOfflineRunners(ctx context.Context, deadline time.Time) ([]*model.Runner, error)
}

// Annotations stores file/line diagnostics for the PR diff view.
type Annotations interface {
	AddAnnotations(ctx context.Context, jobID int64, as []model.Annotation) error
	ListAnnotations(ctx context.Context, jobID int64) ([]model.Annotation, error)
}

// Artifacts stores artifact metadata; bytes live in the blob store.
type Artifacts interface {
	CreateArtifact(ctx context.Context, a *model.Artifact) error
	FinalizeArtifact(ctx context.Context, id int64, size int64, digest string) error
	GetArtifact(ctx context.Context, id int64) (*model.Artifact, error)
	FindArtifact(ctx context.Context, runID int64, name string) (*model.Artifact, error)
	ListArtifacts(ctx context.Context, runID int64) ([]*model.Artifact, error)
	DeleteExpiredArtifacts(ctx context.Context, now time.Time) ([]*model.Artifact, error)
}

// Caches stores actions/cache metadata and the event log.
type Caches interface {
	ReserveCache(ctx context.Context, e *model.CacheEntry) error
	FinalizeCache(ctx context.Context, id int64, size int64) error
	// LookupCache implements restore-keys semantics: exact key first, then each
	// prefix in order, newest match wins. matchedOn names which key hit.
	LookupCache(ctx context.Context, repoID int64, key string, restoreKeys []string, version, ref string) (*model.CacheEntry, string, error)
	GetCache(ctx context.Context, id int64) (*model.CacheEntry, error)
	TouchCache(ctx context.Context, id int64, at time.Time) error
	RecordCacheEvent(ctx context.Context, e model.CacheEvent) error
	ListCacheEvents(ctx context.Context, repoID int64, limit int) ([]model.CacheEvent, error)
	// EvictCaches enforces the per-repo quota, returning what it removed so the
	// eviction can be logged. Silent eviction is forbidden.
	EvictCaches(ctx context.Context, repoID int64, quotaBytes int64, now time.Time) ([]*model.CacheEntry, error)
	CacheUsage(ctx context.Context, repoID int64) (int64, error)
}

// Secrets stores org/repo/environment scoped secrets and vars.
type Secrets interface {
	PutSecret(ctx context.Context, scope, scopeKey, name string, ciphertext []byte) error
	// ResolveSecrets merges org, repo, and environment scopes in that order.
	ResolveSecrets(ctx context.Context, owner, repo, environment string) (map[string][]byte, error)
	DeleteSecret(ctx context.Context, scope, scopeKey, name string) error
	ListSecretNames(ctx context.Context, scope, scopeKey string) ([]string, error)
	PutVar(ctx context.Context, scope, scopeKey, name, value string) error
	ResolveVars(ctx context.Context, owner, repo, environment string) (map[string]string, error)
	DeleteVar(ctx context.Context, scope, scopeKey, name string) error
}

// Event is an audit record. Every cancellation, requeue, classification, and
// retry writes one, and the UI reads them back as the job's timeline.
type Event struct {
	ID      int64          `json:"id"`
	RunID   int64          `json:"run_id,omitempty"`
	JobID   int64          `json:"job_id,omitempty"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
	At      time.Time      `json:"at"`
}

// Events is the audit trail.
type Events interface {
	RecordEvent(ctx context.Context, e Event) error
	ListEvents(ctx context.Context, runID, jobID int64) ([]Event, error)
}
