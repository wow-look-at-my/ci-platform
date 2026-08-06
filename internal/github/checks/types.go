package checks

import (
	"fmt"
	"time"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// GitHub's documented limits on a check run write.
const (
	MaxAnnotationsPerRequest = 50
	MaxAnnotationMessage     = 64 * 1024
	MaxAnnotationTitle       = 255
	MaxOutputTitle           = 255
	MaxOutputText            = 65535
	MaxOutputSummary         = 65535
	MaxActions               = 3
	MaxActionLabel           = 20
	MaxActionDescription     = 40
	MaxActionIdentifier      = 20
)

// Action is a button rendered on the check run.
type Action struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Identifier  string `json:"identifier"`
}

// Identifiers the platform's own buttons use. They arrive back on a
// check_run.requested_action delivery.
const (
	ActionRerunJob        = "rerun_job"
	ActionRerunFailedJobs = "rerun_failed"
	ActionRerunAllJobs    = "rerun_all"
)

// DefaultActions are the buttons offered on a completed check run.
func DefaultActions() []Action {
	return []Action{
		{Label: "Re-run job", Description: "Re-run just this job", Identifier: ActionRerunJob},
		{Label: "Re-run failed", Description: "Re-run every failed job in this run", Identifier: ActionRerunFailedJobs},
	}
}

// Validate enforces GitHub's length limits rather than letting the API reject
// the whole update over a button.
func (a Action) Validate() error {
	switch {
	case a.Identifier == "":
		return fmt.Errorf("checks: action %q has no identifier", a.Label)
	case len(a.Identifier) > MaxActionIdentifier:
		return fmt.Errorf("checks: action identifier %q is %d chars, limit %d", a.Identifier, len(a.Identifier), MaxActionIdentifier)
	case a.Label == "":
		return fmt.Errorf("checks: action %q has no label", a.Identifier)
	case len(a.Label) > MaxActionLabel:
		return fmt.Errorf("checks: action label %q is %d chars, limit %d", a.Label, len(a.Label), MaxActionLabel)
	case len(a.Description) > MaxActionDescription:
		return fmt.Errorf("checks: action description %q is %d chars, limit %d", a.Description, len(a.Description), MaxActionDescription)
	}
	return nil
}

// Annotation is the wire form of a file-anchored diagnostic.
type Annotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	StartColumn     int    `json:"start_column,omitempty"`
	EndColumn       int    `json:"end_column,omitempty"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
	RawDetails      string `json:"raw_details,omitempty"`
}

// annotationFor converts and truncates one stored annotation. Columns are
// dropped on a multi-line span because the API rejects them there.
func annotationFor(a model.Annotation) Annotation {
	out := Annotation{
		Path:            a.Path,
		StartLine:       a.StartLine,
		EndLine:         a.EndLine,
		AnnotationLevel: string(a.Level),
		Message:         truncate(a.Message, MaxAnnotationMessage),
		Title:           truncate(a.Title, MaxAnnotationTitle),
		RawDetails:      truncate(a.RawDetail, MaxAnnotationMessage),
	}
	if out.AnnotationLevel == "" {
		out.AnnotationLevel = string(model.AnnotationFailure)
	}
	if out.StartLine == 0 {
		out.StartLine = 1
	}
	if out.EndLine < out.StartLine {
		out.EndLine = out.StartLine
	}
	if out.StartLine == out.EndLine {
		out.StartColumn, out.EndColumn = a.StartCol, a.EndCol
	}
	return out
}

// Update is one reported state of one job's check run. The same struct covers
// creation, progress, and completion.
type Update struct {
	// JobID is the coalescing key: one job is exactly one check run.
	JobID int64
	Repo  gh.Repo
	// Name is the check run name, which branch protection matches on.
	Name    string
	HeadSHA string
	// CheckRunID skips the create when the job already has one persisted.
	CheckRunID int64
	// ExternalID is the platform's job identity, echoed back on a re-run.
	ExternalID string
	DetailsURL string

	Status     model.Status
	Conclusion model.Conclusion

	StartedAt   *time.Time
	CompletedAt *time.Time

	Steps       []model.Step
	Annotations []model.Annotation
	Actions     []Action

	// Attempt and MaxAttempts render "Attempt 2 of 3".
	Attempt     int
	MaxAttempts int
	// Class and ClassReason surface the failure classification and the sentence
	// behind it.
	Class       model.FailureClass
	ClassReason string
	// Cancel is required whenever Conclusion is cancelled.
	Cancel *model.CancelReason
	// Timing renders the queued/setup/execute breakdown.
	Timing *model.Timing
	// Summary is appended verbatim after the generated summary lines.
	Summary string
}

// Terminal reports whether this update completes the check run. A terminal
// update is never coalesced away.
func (u Update) Terminal() bool { return u.Status == model.StatusCompleted }

// Validate rejects an update that cannot be written, naming the missing field.
func (u Update) Validate() error {
	switch {
	case u.JobID == 0:
		return fmt.Errorf("checks: update for %q has no JobID", u.Name)
	case !u.Repo.Valid():
		return fmt.Errorf("checks: update for job %d has no repo owner/name", u.JobID)
	case u.Name == "":
		return fmt.Errorf("checks: update for job %d has no Name", u.JobID)
	case u.HeadSHA == "":
		return fmt.Errorf("checks: update for job %d (%s) has no HeadSHA", u.JobID, u.Name)
	case !u.Status.Valid():
		return fmt.Errorf("checks: update for job %d has invalid status %q", u.JobID, u.Status)
	case u.Terminal() && u.Conclusion == "":
		return fmt.Errorf("checks: completed update for job %d has no conclusion", u.JobID)
	case u.Terminal() && !u.Conclusion.Valid():
		return fmt.Errorf("checks: update for job %d has invalid conclusion %q", u.JobID, u.Conclusion)
	case u.Conclusion == model.ConclusionCancelled && u.Cancel == nil:
		return fmt.Errorf("checks: job %d was cancelled with no CancelReason; a cancellation with no explanation is never reported", u.JobID)
	}
	if u.Cancel != nil {
		if err := u.Cancel.Validate(); err != nil {
			return fmt.Errorf("checks: job %d: %w", u.JobID, err)
		}
	}
	if len(u.Actions) > MaxActions {
		return fmt.Errorf("checks: job %d declares %d actions, limit %d", u.JobID, len(u.Actions), MaxActions)
	}
	for _, a := range u.Actions {
		if err := a.Validate(); err != nil {
			return err
		}
	}
	return nil
}
