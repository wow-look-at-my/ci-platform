// Package model holds the vocabulary every other package speaks: run/job/step
// identity, status, conclusion, and the failure classification that the whole
// platform exists to get right.
package model

import "fmt"

// Status is the lifecycle phase of a run, job, or step; mirrors the Checks API status values.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	// StatusWaiting covers a job held by a concurrency group, environment gate, or fork-PR approval.
	StatusWaiting Status = "waiting"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusInProgress, StatusCompleted, StatusWaiting:
		return true
	}
	return false
}

// Terminal reports whether no further transition is expected.
func (s Status) Terminal() bool { return s == StatusCompleted }

// Conclusion is the outcome of a completed unit of work. ConclusionInfraFailure maps to action_required.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionFailure        Conclusion = "failure"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionCancelled      Conclusion = "cancelled"
	ConclusionTimedOut       Conclusion = "timed_out"
	ConclusionActionRequired Conclusion = "action_required"
	ConclusionSkipped        Conclusion = "skipped"
	ConclusionStale          Conclusion = "stale"
	ConclusionInfraFailure   Conclusion = "infra_failure"
	ConclusionConfigError    Conclusion = "config_error"
)

// Valid reports whether c is a known conclusion.
func (c Conclusion) Valid() bool {
	switch c {
	case ConclusionSuccess, ConclusionFailure, ConclusionNeutral,
		ConclusionCancelled, ConclusionTimedOut, ConclusionActionRequired,
		ConclusionSkipped, ConclusionStale, ConclusionInfraFailure,
		ConclusionConfigError:
		return true
	}
	return false
}

// IsFailure reports whether c should stop dependents. Skipped is excluded: it
// never fails dependents, but Aggregate also never counts it as success.
func (c Conclusion) IsFailure() bool {
	switch c {
	case ConclusionFailure, ConclusionTimedOut, ConclusionInfraFailure,
		ConclusionConfigError, ConclusionActionRequired:
		return true
	}
	return false
}

// UserVisibleRed reports whether this conclusion should render as "your code is
// broken". Infra and config failures deliberately do not.
func (c Conclusion) UserVisibleRed() bool {
	return c == ConclusionFailure || c == ConclusionTimedOut
}

// FailureClass answers whose fault a non-success outcome was; every one carries a class.
type FailureClass string

const (
	// ClassNone is the zero value, used on success.
	ClassNone FailureClass = ""
	// ClassUser means a command the user wrote exited non-zero. Never retried.
	ClassUser FailureClass = "user"
	// ClassInfra means the platform, network, or a platform dependency failed. Retried with backoff.
	ClassInfra FailureClass = "infra"
	// ClassConfig means the workflow is wrong (bad YAML, action ref, unsupported key). Never retried.
	ClassConfig FailureClass = "config"
)

// Valid reports whether f is a known class.
func (f FailureClass) Valid() bool {
	switch f {
	case ClassNone, ClassUser, ClassInfra, ClassConfig:
		return true
	}
	return false
}

// Retryable reports whether the default retry policy retries this class.
func (f FailureClass) Retryable() bool { return f == ClassInfra }

// Conclusion maps a class to the conclusion it produces.
func (f FailureClass) Conclusion() Conclusion {
	switch f {
	case ClassNone:
		return ConclusionSuccess
	case ClassUser:
		return ConclusionFailure
	case ClassInfra:
		return ConclusionInfraFailure
	case ClassConfig:
		return ConclusionConfigError
	}
	return ConclusionFailure
}

// CancelActor identifies who or what cancelled a unit of work; every cancellation records one.
type CancelActor string

const (
	CancelActorUser             CancelActor = "user"
	CancelActorConcurrencyGroup CancelActor = "concurrency_group"
	CancelActorTimeout          CancelActor = "timeout"
	CancelActorRunnerLost       CancelActor = "runner_lost"
	CancelActorSupersededByRun  CancelActor = "superseded_by_newer_run"
	CancelActorDependencyFailed CancelActor = "dependency_failed"
	CancelActorShutdown         CancelActor = "control_plane_shutdown"
)

// Valid reports whether a is a known actor.
func (a CancelActor) Valid() bool {
	switch a {
	case CancelActorUser, CancelActorConcurrencyGroup, CancelActorTimeout,
		CancelActorRunnerLost, CancelActorSupersededByRun,
		CancelActorDependencyFailed, CancelActorShutdown:
		return true
	}
	return false
}

// CancelReason is the recorded, surfaced explanation for a cancellation. Both
// fields are required: constructing one without a sentence is a programming
// error that Validate rejects, because "cancelled with no reason anywhere" is
// the exact incident this platform was built to never repeat.
type CancelReason struct {
	Actor CancelActor `json:"actor"`
	// Sentence is a complete human sentence shown verbatim in the UI and check run output.
	Sentence string `json:"sentence"`
	// TriggeredBy is the login or run/group ID that caused it; optional, since a timeout has no principal.
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// Validate rejects a cancellation that would leave the user asking "why?".
func (r CancelReason) Validate() error {
	if !r.Actor.Valid() {
		return fmt.Errorf("cancel reason: unknown actor %q", r.Actor)
	}
	if r.Sentence == "" {
		return fmt.Errorf("cancel reason: actor %q has no explanation sentence", r.Actor)
	}
	return nil
}

// Aggregate reduces per-unit conclusions to one, honestly.
//
// The rules that matter: an empty set is NOT success (nothing ran, so nothing
// passed), and a skipped unit never upgrades to success. Failure ordering is by
// severity so the aggregate names the worst thing that happened.
func Aggregate(cs []Conclusion) Conclusion {
	if len(cs) == 0 {
		// Zero units of work cannot satisfy anything. Neutral, never success.
		return ConclusionNeutral
	}
	var sawSkipped, sawSuccess, sawCancelled, sawNeutral bool
	worst := Conclusion("")
	rank := func(c Conclusion) int {
		switch c {
		case ConclusionConfigError:
			return 5
		case ConclusionInfraFailure:
			return 4
		case ConclusionTimedOut:
			return 3
		case ConclusionFailure:
			return 2
		case ConclusionActionRequired:
			return 1
		}
		return 0
	}
	for _, c := range cs {
		switch {
		case c.IsFailure():
			if worst == "" || rank(c) > rank(worst) {
				worst = c
			}
		case c == ConclusionSkipped:
			sawSkipped = true
		case c == ConclusionCancelled:
			sawCancelled = true
		case c == ConclusionSuccess:
			sawSuccess = true
		case c == ConclusionNeutral, c == ConclusionStale:
			sawNeutral = true
		}
	}
	switch {
	case worst != "":
		return worst
	case sawCancelled:
		return ConclusionCancelled
	case sawNeutral:
		return ConclusionNeutral
	case sawSkipped && !sawSuccess:
		// Everything was skipped; reporting success would be the zero-work-satisfies-a-check lie.
		return ConclusionSkipped
	case sawSkipped:
		// A mix. Report neutral rather than laundering skips into a green.
		return ConclusionNeutral
	default:
		return ConclusionSuccess
	}
}
