package model

import (
	"encoding/json"
	"time"
)

// Repo identifies a repository the platform serves.
type Repo struct {
	ID             int64  `json:"id"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	InstallationID int64  `json:"installation_id"`
	DefaultBranch  string `json:"default_branch"`
	Private        bool   `json:"private"`
}

// FullName is "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Run is one execution of one workflow file for one event.
type Run struct {
	ID       int64  `json:"id"`
	RepoID   int64  `json:"repo_id"`
	RepoFull string `json:"repo_full_name"`

	WorkflowName string `json:"workflow_name"`
	WorkflowPath string `json:"workflow_path"`
	// RunNumber increments per workflow per repo, like GHA's.
	RunNumber int64 `json:"run_number"`
	// Attempt starts at 1 and increments on re-run.
	Attempt int `json:"attempt"`

	Event      string `json:"event"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch,omitempty"`
	Actor      string `json:"actor"`
	// IsForkPR gates secrets, OIDC, and the approval requirement.
	IsForkPR bool `json:"is_fork_pr"`
	// Approved is set once a maintainer approves a fork PR run.
	Approved   bool   `json:"approved"`
	ApprovedBy string `json:"approved_by,omitempty"`

	CheckSuiteID int64 `json:"check_suite_id,omitempty"`

	Status     Status     `json:"status"`
	Conclusion Conclusion `json:"conclusion,omitempty"`
	// Cancel is set whenever Conclusion is cancelled. Never nil in that case.
	Cancel *CancelReason `json:"cancel,omitempty"`

	// EventPayload is the raw webhook body, kept so the github context can be
	// rebuilt on re-run without re-fetching.
	EventPayload json.RawMessage `json:"-"`
	// Inputs are workflow_dispatch inputs, already defaulted and type-checked.
	Inputs map[string]any `json:"inputs,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Job is one node of the run's DAG after matrix expansion: exactly one check
// run, exactly one runner, exactly one sandbox.
type Job struct {
	ID    int64 `json:"id"`
	RunID int64 `json:"run_id"`

	// Key is the workflow's jobs.<key>. Stable across matrix legs.
	Key string `json:"key"`
	// Name is the display name including any matrix suffix, e.g.
	// "publish (claude-host/agent-host, Dockerfile)". This is the check run
	// name, and existing branch protection matches on it.
	Name string `json:"name"`
	// MatrixKey is a stable identity for one leg, "" for an unmatrixed job.
	MatrixKey string         `json:"matrix_key,omitempty"`
	Matrix    map[string]any `json:"matrix,omitempty"`

	Needs   []string `json:"needs,omitempty"`
	Labels  []string `json:"labels"`
	Attempt int      `json:"attempt"`
	// MaxAttempts comes from the resolved retry policy.
	MaxAttempts int `json:"max_attempts"`

	Status     Status        `json:"status"`
	Conclusion Conclusion    `json:"conclusion,omitempty"`
	Class      FailureClass  `json:"failure_class,omitempty"`
	Cancel     *CancelReason `json:"cancel,omitempty"`

	// ConcurrencyGroup, when set, admits at most one job at a time.
	ConcurrencyGroup  string            `json:"concurrency_group,omitempty"`
	CancelInProgress  bool              `json:"cancel_in_progress,omitempty"`
	ContinueOnError   bool              `json:"continue_on_error,omitempty"`
	TimeoutMinutes    int               `json:"timeout_minutes,omitempty"`
	CheckRunID        int64             `json:"check_run_id,omitempty"`
	RunnerID          string            `json:"runner_id,omitempty"`
	Environment       string            `json:"environment,omitempty"`
	AwaitingApproval  bool              `json:"awaiting_approval,omitempty"`
	Outputs           map[string]string `json:"outputs,omitempty"`
	FailureExplained  string            `json:"failure_explanation,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	QueuedAt          *time.Time        `json:"queued_at,omitempty"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	SetupCompletedAt  *time.Time        `json:"setup_completed_at,omitempty"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	LeaseExpiresAt    *time.Time        `json:"lease_expires_at,omitempty"`
	LastHeartbeatAt   *time.Time        `json:"last_heartbeat_at,omitempty"`
	RequeueCount      int               `json:"requeue_count"`
	InfraRetryCount   int               `json:"infra_retry_count"`
	ClassificationLog []string          `json:"classification_log,omitempty"`
}

// Timing is the queued/setup/execute breakdown surfaced on every job page. The
// "job setup took 5m30s with nothing to explain it" incident is why setup is a
// first-class measured phase rather than something inferred from timestamps.
type Timing struct {
	QueuedFor  time.Duration `json:"queued_for"`
	SetupFor   time.Duration `json:"setup_for"`
	ExecuteFor time.Duration `json:"execute_for"`
	TotalFor   time.Duration `json:"total_for"`
}

// Timing computes the phase breakdown from whatever timestamps exist so far.
func (j Job) Timing(now time.Time) Timing {
	var t Timing
	end := func(p *time.Time) time.Time {
		if p != nil {
			return *p
		}
		return now
	}
	from := j.CreatedAt
	if j.QueuedAt != nil {
		from = *j.QueuedAt
	}
	if j.StartedAt != nil {
		t.QueuedFor = j.StartedAt.Sub(from)
		t.SetupFor = end(j.SetupCompletedAt).Sub(*j.StartedAt)
		if j.SetupCompletedAt != nil {
			t.ExecuteFor = end(j.CompletedAt).Sub(*j.SetupCompletedAt)
		}
	} else {
		t.QueuedFor = now.Sub(from)
	}
	t.TotalFor = end(j.CompletedAt).Sub(from)
	for _, d := range []*time.Duration{&t.QueuedFor, &t.SetupFor, &t.ExecuteFor, &t.TotalFor} {
		if *d < 0 {
			*d = 0
		}
	}
	return t
}

// Step is one step of one job attempt.
type Step struct {
	ID     int64 `json:"id"`
	JobID  int64 `json:"job_id"`
	Number int   `json:"number"`

	Name       string       `json:"name"`
	StepID     string       `json:"step_id,omitempty"`
	Status     Status       `json:"status"`
	Conclusion Conclusion   `json:"conclusion,omitempty"`
	Class      FailureClass `json:"failure_class,omitempty"`
	ExitCode   int          `json:"exit_code"`

	ContinueOnError bool              `json:"continue_on_error,omitempty"`
	Outputs         map[string]string `json:"outputs,omitempty"`
	Attempt         int               `json:"attempt"`

	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// LogStart and LogEnd bound this step's slice of the job log, so the UI can
	// deep-link to the failing step without a separate log per step.
	LogStart int64 `json:"log_start"`
	LogEnd   int64 `json:"log_end"`
}

// Duration is the wall time of the step so far.
func (s Step) Duration(now time.Time) time.Duration {
	if s.StartedAt == nil {
		return 0
	}
	if s.CompletedAt != nil {
		return s.CompletedAt.Sub(*s.StartedAt)
	}
	return now.Sub(*s.StartedAt)
}

// AnnotationLevel mirrors the Checks API annotation levels.
type AnnotationLevel string

const (
	AnnotationNotice  AnnotationLevel = "notice"
	AnnotationWarning AnnotationLevel = "warning"
	AnnotationFailure AnnotationLevel = "failure"
)

// Annotation is a file/line-anchored message rendered on the PR diff.
type Annotation struct {
	ID        int64           `json:"id"`
	JobID     int64           `json:"job_id"`
	Path      string          `json:"path"`
	StartLine int             `json:"start_line"`
	EndLine   int             `json:"end_line"`
	StartCol  int             `json:"start_column,omitempty"`
	EndCol    int             `json:"end_column,omitempty"`
	Level     AnnotationLevel `json:"annotation_level"`
	Message   string          `json:"message"`
	Title     string          `json:"title,omitempty"`
	RawDetail string          `json:"raw_details,omitempty"`
}

// RunnerState is the fleet-page view of an agent.
type RunnerState string

const (
	RunnerIdle    RunnerState = "idle"
	RunnerBusy    RunnerState = "busy"
	RunnerOffline RunnerState = "offline"
	RunnerDrained RunnerState = "drained"
)

// Runner is one agent host.
type Runner struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Labels        []string    `json:"labels"`
	Group         string      `json:"group,omitempty"`
	State         RunnerState `json:"state"`
	CurrentJobID  int64       `json:"current_job_id,omitempty"`
	Capacity      int         `json:"capacity"`
	Version       string      `json:"version"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`
	FirstSeenAt   time.Time   `json:"first_seen_at"`
	LastHeartbeat time.Time   `json:"last_heartbeat"`
}

// HeartbeatAge is how stale this runner's liveness signal is.
func (r Runner) HeartbeatAge(now time.Time) time.Duration { return now.Sub(r.LastHeartbeat) }

// LogLine is one line of job output. Lines are append-only and never rewritten,
// including across retries: every attempt keeps its own log.
type LogLine struct {
	// Seq is monotonic per (job, attempt) and is the deep-link anchor.
	Seq       int64     `json:"seq"`
	Timestamp time.Time `json:"ts"`
	// StepNumber attributes the line to a step, 0 for platform-emitted lines.
	StepNumber int `json:"step"`
	// Stream is "stdout", "stderr", or "platform".
	Stream string `json:"stream"`
	Text   string `json:"text"`
	// Group is the ::group:: nesting title in effect, "" at top level.
	Group string `json:"group,omitempty"`
}

// Artifact is an uploaded build output.
type Artifact struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	JobID       int64      `json:"job_id,omitempty"`
	Name        string     `json:"name"`
	SizeBytes   int64      `json:"size_bytes"`
	Digest      string     `json:"digest"`
	StorageKey  string     `json:"-"`
	ExpiresAt   time.Time  `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	Finalized   bool       `json:"finalized"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// CacheEntry is one actions/cache object.
type CacheEntry struct {
	ID           int64     `json:"id"`
	RepoID       int64     `json:"repo_id"`
	Key          string    `json:"key"`
	Version      string    `json:"version"`
	Ref          string    `json:"ref"`
	SizeBytes    int64     `json:"size_bytes"`
	StorageKey   string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"`
	Finalized    bool      `json:"finalized"`
}

// CacheEvent records a hit, miss, or eviction. Eviction is recorded explicitly
// because a cache that silently drops entries is indistinguishable from one
// that is simply slow.
type CacheEvent struct {
	ID        int64     `json:"id"`
	RepoID    int64     `json:"repo_id"`
	Key       string    `json:"key"`
	Kind      string    `json:"kind"` // hit | miss | store | evict
	MatchedOn string    `json:"matched_on,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	At        time.Time `json:"at"`
}
