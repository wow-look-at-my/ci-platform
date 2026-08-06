// Package protocol is the wire contract between the control plane and runner
// agents. It is HTTP+JSON with long-polling rather than gRPC: the traffic is a
// handful of calls per job, and a JSON surface is testable with curl during an
// incident, which is worth more here than protobuf's efficiency.
//
// See docs/deviations.md for the record of this choice.
package protocol

import (
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// APIVersion is bumped on any breaking change; the control plane rejects a
// runner announcing a version it does not understand rather than silently
// misinterpreting its payloads.
const APIVersion = "1"

// Endpoint paths, all under /runner/v1.
const (
	PathRegister  = "/runner/v1/register"
	PathAcquire   = "/runner/v1/acquire"
	PathHeartbeat = "/runner/v1/heartbeat"
	PathLogs      = "/runner/v1/logs"
	PathStepStart = "/runner/v1/step/start"
	PathStepEnd   = "/runner/v1/step/end"
	PathComplete  = "/runner/v1/complete"
	PathRelease   = "/runner/v1/release"
	PathAnnotate  = "/runner/v1/annotate"
	PathSetup     = "/runner/v1/setup"
)

// RegisterRequest announces an agent to the control plane.
type RegisterRequest struct {
	APIVersion string   `json:"api_version"`
	RunnerID   string   `json:"runner_id"`
	Name       string   `json:"name"`
	Labels     []string `json:"labels"`
	Group      string   `json:"group,omitempty"`
	Capacity   int      `json:"capacity"`
	Version    string   `json:"version"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
}

// RegisterResponse carries back the negotiated lease parameters.
type RegisterResponse struct {
	LeaseTTL          Duration `json:"lease_ttl"`
	HeartbeatInterval Duration `json:"heartbeat_interval"`
	// LogFlushInterval bounds how long the agent may buffer log lines.
	LogFlushInterval Duration `json:"log_flush_interval"`
}

// AcquireRequest long-polls for work.
type AcquireRequest struct {
	RunnerID string   `json:"runner_id"`
	Labels   []string `json:"labels"`
	// Wait is how long the control plane may hold the request open.
	Wait Duration `json:"wait"`
}

// AcquireResponse returns an assignment, or Job == nil when the poll timed out.
type AcquireResponse struct {
	Assignment *Assignment `json:"assignment,omitempty"`
}

// Assignment is everything a runner needs to execute one job attempt without
// calling back for more. It is keyed by (RunID, JobID, Attempt): re-delivering
// the same key must never produce a second execution with side effects.
type Assignment struct {
	RunID   int64 `json:"run_id"`
	JobID   int64 `json:"job_id"`
	Attempt int   `json:"attempt"`
	// IdempotencyKey is "<run_id>/<job_id>/<attempt>"; the runner refuses to
	// start a key it has already started.
	IdempotencyKey string `json:"idempotency_key"`

	JobName   string   `json:"job_name"`
	JobKey    string   `json:"job_key"`
	RepoOwner string   `json:"repo_owner"`
	RepoName  string   `json:"repo_name"`
	HeadSHA   string   `json:"head_sha"`
	HeadRef   string   `json:"head_ref"`
	Labels    []string `json:"labels"`

	// Steps are fully resolved except for expressions that depend on earlier
	// steps in this same job, which the runner evaluates as it goes.
	Steps []StepSpec `json:"steps"`
	// Env is the job-level environment, already expression-evaluated.
	Env map[string]string `json:"env"`
	// Contexts carries the evaluated github/runner/needs/matrix/strategy/vars
	// contexts so the runner can finish evaluating step-level expressions.
	Contexts map[string]any `json:"contexts"`
	// Secrets are injected per job and masked by value in logs. They are never
	// written to the workspace.
	Secrets map[string]string `json:"secrets,omitempty"`

	Container *ContainerSpec            `json:"container,omitempty"`
	Services  map[string]*ContainerSpec `json:"services,omitempty"`

	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	// SetupTimeout bounds the "stuck in setup" state separately from execution.
	SetupTimeout Duration `json:"setup_timeout"`

	// JobToken is a per-job scoped bearer token for the artifact, cache, log,
	// and OIDC endpoints. It carries no repository write access and expires
	// with the job.
	JobToken  string `json:"job_token"`
	ServerURL string `json:"server_url"`
	// ServiceEnv is the environment the artifact, cache, and OIDC clients
	// discover their endpoints through. The control plane builds it because it
	// owns those URLs and mints the token; a runner deriving them itself would
	// be guessing at the server it is talking to.
	ServiceEnv map[string]string `json:"service_env,omitempty"`

	// Retry is the resolved job-level policy, so the runner can report an
	// attempt as retryable without asking.
	Retry model.RetryPolicy `json:"retry"`

	// DefaultShell and WorkingDirectory come from defaults.run.
	DefaultShell     string `json:"default_shell,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

// StepSpec is one step to execute.
type StepSpec struct {
	Number int    `json:"number"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	// IfExpr is left unevaluated: it depends on the outcome of earlier steps.
	IfExpr string `json:"if,omitempty"`
	// Uses is an action reference, resolved by the runner.
	Uses string            `json:"uses,omitempty"`
	Run  string            `json:"run,omitempty"`
	With map[string]string `json:"with,omitempty"`
	Env  map[string]string `json:"env,omitempty"`

	Shell            string             `json:"shell,omitempty"`
	WorkingDirectory string             `json:"working_directory,omitempty"`
	ContinueOnError  bool               `json:"continue_on_error,omitempty"`
	TimeoutMinutes   int                `json:"timeout_minutes,omitempty"`
	Retry            *model.RetryPolicy `json:"retry,omitempty"`
	// PreAction and PostAction mark synthesized steps from an action's
	// pre:/post: entrypoints, which run outside the normal if: rules.
	PreAction  bool `json:"pre,omitempty"`
	PostAction bool `json:"post,omitempty"`
}

// ContainerSpec is a resolved job or service container.
type ContainerSpec struct {
	Image       string            `json:"image"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Options     string            `json:"options,omitempty"`
}

// HeartbeatRequest extends the lease and reports liveness.
type HeartbeatRequest struct {
	RunnerID string `json:"runner_id"`
	JobID    int64  `json:"job_id,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	Phase    string `json:"phase,omitempty"` // setup | execute | teardown
}

// HeartbeatResponse carries control-plane-initiated instructions back, which is
// how a cancellation reaches a running job.
type HeartbeatResponse struct {
	// Cancel is non-nil when the control plane wants this job stopped, and it
	// always carries the reason: there is no unexplained cancellation path.
	Cancel *model.CancelReason `json:"cancel,omitempty"`
	// LeaseLost tells the runner the job was taken from it and it must stop
	// without reporting a result.
	LeaseLost bool `json:"lease_lost,omitempty"`
}

// SetupRequest reports the setup phase boundary and its cost breakdown, so
// "setup took 5m30s" is a measured number rather than an inference.
type SetupRequest struct {
	RunnerID string `json:"runner_id"`
	JobID    int64  `json:"job_id"`
	Attempt  int    `json:"attempt"`
	Phase    string `json:"phase"` // started | completed
	// Breakdown names where setup time went, e.g. {"image_pull": "45s"}.
	Breakdown map[string]Duration `json:"breakdown,omitempty"`
	// CacheWarm reports whether the image cache volume was already populated.
	CacheWarm bool `json:"cache_warm,omitempty"`
}

// LogBatch is a run of log lines. Batching is required: a per-line POST would
// swamp the control plane on a chatty build.
type LogBatch struct {
	RunnerID string          `json:"runner_id"`
	JobID    int64           `json:"job_id"`
	Attempt  int             `json:"attempt"`
	Lines    []model.LogLine `json:"lines"`
}

// StepStartRequest opens a step.
type StepStartRequest struct {
	RunnerID string `json:"runner_id"`
	JobID    int64  `json:"job_id"`
	Attempt  int    `json:"attempt"`
	Number   int    `json:"number"`
	Name     string `json:"name"`
	StepID   string `json:"step_id,omitempty"`
	LogStart int64  `json:"log_start"`
}

// StepEndRequest closes a step with its outcome and classification.
type StepEndRequest struct {
	RunnerID   string             `json:"runner_id"`
	JobID      int64              `json:"job_id"`
	Attempt    int                `json:"attempt"`
	Number     int                `json:"number"`
	Conclusion model.Conclusion   `json:"conclusion"`
	Class      model.FailureClass `json:"class"`
	// ClassReason is the recorded explanation of the classification decision,
	// e.g. "registry responded 524 (Cloudflare origin timeout) -> infra".
	ClassReason string            `json:"class_reason,omitempty"`
	ExitCode    int               `json:"exit_code"`
	Outputs     map[string]string `json:"outputs,omitempty"`
	LogEnd      int64             `json:"log_end"`
}

// CompleteRequest reports a finished job attempt.
type CompleteRequest struct {
	RunnerID   string             `json:"runner_id"`
	JobID      int64              `json:"job_id"`
	Attempt    int                `json:"attempt"`
	Conclusion model.Conclusion   `json:"conclusion"`
	Class      model.FailureClass `json:"class"`
	// ClassReason explains the classification in one human sentence.
	ClassReason string `json:"class_reason,omitempty"`
	// Explanation is shown as the job's headline failure message.
	Explanation string              `json:"explanation,omitempty"`
	Outputs     map[string]string   `json:"outputs,omitempty"`
	Cancel      *model.CancelReason `json:"cancel,omitempty"`
	// ClassificationLog records every classification decision made during the
	// attempt so the operator can see why something was called infra.
	ClassificationLog []string `json:"classification_log,omitempty"`
}

// CompleteResponse tells the runner whether the attempt will be retried, purely
// so the agent can log it; the control plane owns the decision.
type CompleteResponse struct {
	WillRetry   bool     `json:"will_retry"`
	NextAttempt int      `json:"next_attempt,omitempty"`
	RetryAfter  Duration `json:"retry_after,omitempty"`
}

// ReleaseRequest gives a job back voluntarily, e.g. on agent shutdown. The
// reason is required: a job that reappears in the queue must say why.
type ReleaseRequest struct {
	RunnerID string             `json:"runner_id"`
	JobID    int64              `json:"job_id"`
	Attempt  int                `json:"attempt"`
	Reason   model.CancelReason `json:"reason"`
}

// AnnotateRequest carries file/line diagnostics up from ::error file=... lines.
type AnnotateRequest struct {
	RunnerID    string             `json:"runner_id"`
	JobID       int64              `json:"job_id"`
	Attempt     int                `json:"attempt"`
	Annotations []model.Annotation `json:"annotations"`
}

// Duration is a time.Duration that marshals as a Go duration string, so the
// wire format is readable during an incident.
type Duration time.Duration

// MarshalJSON writes the duration as a quoted string like "30s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

// UnmarshalJSON accepts a quoted duration string or a nanosecond count.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 1 && b[0] == '"' {
		v, err := time.ParseDuration(string(b[1 : len(b)-1]))
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var ns int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return &time.ParseError{Layout: "duration", Value: string(b)}
		}
		ns = ns*10 + int64(c-'0')
	}
	*d = Duration(ns)
	return nil
}

// D converts to time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }
