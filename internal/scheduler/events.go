package scheduler

import (
	"context"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Event kinds. The UI reads these back as a job's timeline, so every state
// change a user could ask "why did that happen?" about writes one.
const (
	EventRunStarted   = "run_started"
	EventRunCompleted = "run_completed"
	EventQueued       = "queued"
	EventWaiting      = "waiting"
	EventDispatched   = "dispatched"
	EventStarted      = "started"
	EventRetried      = "retried"
	EventRequeued     = "requeued"
	EventCancelled    = "cancelled"
	EventSkipped      = "skipped"
	EventCompleted    = "completed"
	EventClassified   = "classified"
	EventRerun        = "rerun"
	EventRestricted   = "restricted"
)

// emit records one audit row. A failure to record is returned, never swallowed:
// a timeline with holes in it is how "why was this cancelled?" becomes
// unanswerable.
func (s *Scheduler) emit(ctx context.Context, runID, jobID int64, kind, message string, detail map[string]any, at time.Time) error {
	return s.st.RecordEvent(ctx, store.Event{
		RunID:   runID,
		JobID:   jobID,
		Kind:    kind,
		Message: message,
		Detail:  detail,
		At:      at,
	})
}
