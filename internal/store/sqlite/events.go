package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// execer is what *sql.DB and *sql.Tx have in common here, so an event written
// inside a queue transaction and one written on its own share a code path.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordEvent appends one audit record. Kind is required: an unattributed
// event is not an audit trail.
func (s *Store) RecordEvent(ctx context.Context, e store.Event) error {
	return s.recordEvent(ctx, s.db, e)
}

func (s *Store) recordEvent(ctx context.Context, q execer, e store.Event) error {
	if e.Kind == "" {
		return fmt.Errorf("sqlite: RecordEvent: event for run %d job %d has no kind", e.RunID, e.JobID)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	detail, err := jsonText(e.Detail)
	if err != nil {
		return err
	}
	const ins = `INSERT INTO events (run_id, job_id, kind, message, detail, at)
	             VALUES (?,?,?,?,?,?)`
	_, err = q.ExecContext(ctx, ins, e.RunID, e.JobID, e.Kind, e.Message, detail, ts(e.At))
	return mapErr("sqlite: RecordEvent", err)
}

// ListEvents returns the timeline, oldest first. A non-zero runID or jobID
// narrows the result; both zero returns every event.
func (s *Store) ListEvents(ctx context.Context, runID, jobID int64) ([]store.Event, error) {
	const q = `SELECT id, run_id, job_id, kind, message, detail, at FROM events
	           WHERE (? = 0 OR run_id = ?) AND (? = 0 OR job_id = ?)
	           ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, runID, runID, jobID, jobID)
	if err != nil {
		return nil, mapErr("sqlite: ListEvents", err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		var detail, at string
		if err := rows.Scan(&e.ID, &e.RunID, &e.JobID, &e.Kind, &e.Message, &detail, &at); err != nil {
			return nil, mapErr("sqlite: ListEvents", err)
		}
		if err := jsonInto(detail, &e.Detail); err != nil {
			return nil, err
		}
		e.Detail = emptyMapToNil(e.Detail)
		if e.At, err = mustTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapErr("sqlite: ListEvents", rows.Err())
}
