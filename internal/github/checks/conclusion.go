package checks

import "github.com/wow-look-at-my/ci-platform/internal/model"

// ConclusionToCheck maps a platform conclusion onto the Checks API, returning
// the wire conclusion and the prefix that opens output.title.
//
// Two mappings deviate from a naive pass-through, deliberately:
//
//	infra_failure -> action_required, not failure
//	config_error  -> action_required, not failure
//
// A red X on GitHub must mean the user's code is broken. A registry 5xx and a
// failing test rendering identically is the exact experience this platform
// exists to replace, so an infra or config failure gets a visually distinct
// state that does not accuse the commit. The cost is that action_required also
// blocks a required check, which is correct: neither outcome is a pass.
func ConclusionToCheck(c model.Conclusion) (conclusion string, titlePrefix string) {
	switch c {
	case model.ConclusionSuccess:
		return "success", "Success"
	case model.ConclusionFailure:
		return "failure", "Failed"
	case model.ConclusionTimedOut:
		return "timed_out", "Timed out"
	case model.ConclusionCancelled:
		return "cancelled", "Cancelled"
	case model.ConclusionSkipped:
		return "skipped", "Skipped"
	case model.ConclusionNeutral:
		return "neutral", "Neutral"
	case model.ConclusionStale:
		return "stale", "Stale"
	case model.ConclusionActionRequired:
		return "action_required", "Action required"
	case model.ConclusionInfraFailure:
		return "action_required", "Infrastructure failure"
	case model.ConclusionConfigError:
		return "action_required", "Workflow configuration error"
	}
	// An unknown conclusion is never laundered into success.
	return "neutral", "Unknown conclusion " + string(c)
}

// statusTitle is the title for a run that has not completed.
func statusTitle(s model.Status) string {
	switch s {
	case model.StatusQueued:
		return "Queued"
	case model.StatusInProgress:
		return "In progress"
	case model.StatusWaiting:
		return "Waiting"
	case model.StatusCompleted:
		return "Completed"
	}
	return string(s)
}
