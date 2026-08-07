// Package model holds the vocabulary every other package speaks: run/job/step
// identity, status, conclusion, and the failure classification that the whole
// platform exists to get right.
package model

import "fmt"

// Status is the lifecycle phase of a run, job, or step. It mirrors the Checks
// API status values so the reporter can pass them through unchanged.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	// StatusWaiting covers a job held by a concurrency group, an environment
	// protection rule, or a fork-PR approval gate. GitHub has "waiting" too.
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

// Conclusion is the outcome of a completed unit of work.
//
// ConclusionInfraFailure is ours, not GitHub's: it is the whole point of the
// platform. It maps onto the Checks API as "action_required" so that it is
// visually distinct from a red build (see internal/github.ConclusionToCheck),
// because a red build must mean the user's code is broken.
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

// IsFailure reports whether c should stop dependents from running. Skipped is
// deliberately excluded: a skipped job does not fail its dependents, but it is
// also never counted as success by the aggregate (see Aggregate).
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

// FailureClass answers the question that GitHub Actions never answers: whose
// fault was it? Every non-success outcome carries one.
type FailureClass string

const (
	// ClassNone is the zero value, used on success.
	ClassNone FailureClass = ""
	// ClassUser means a command the user wrote exited non-zero. Never retried.
	ClassUser FailureClass = "user"
	// ClassInfra means the platform, the network, or a dependency of the
	// platform failed. Retried with backoff by default.
	ClassInfra FailureClass = "infra"
	// ClassConfig means the workflow itself is wrong: unparseable YAML, an
	// unresolvable action ref, an unsupported key. Never retried; retrying
	// cannot help.
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

// CancelActor identifies who or what cancelled a unit of work. Every
// cancellation records one; there is no path that cancels without an actor.
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
	// Sentence is a complete human sentence shown verbatim in the UI and in
	// the check run's output. Not a code, not an enum name.
	Sentence string `json:"sentence"`
	// TriggeredBy is the login of the user, or the ID of the run/group that
	// caused it. Optional, because a timeout has no principal.
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
		// Everything was skipped. Saying "success" here is the
		// zero-work-satisfies-a-required-check lie.
		return ConclusionSkipped
	case sawSkipped:
		// A mix. Report neutral rather than laundering skips into a green.
		return ConclusionNeutral
	default:
		return ConclusionSuccess
	}
}
