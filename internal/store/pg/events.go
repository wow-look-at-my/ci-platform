package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// RecordEvent appends one audit record. Kind is required: an unattributed
// event is not an audit trail.
func (s *Store) RecordEvent(ctx context.Context, e store.Event) error {
	return s.recordEvent(ctx, s.pool, e)
}

type execQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) recordEvent(ctx context.Context, q execQuerier, e store.Event) error {
	if e.Kind == "" {
		return fmt.Errorf("pg: RecordEvent: event for run %d job %d has no kind", e.RunID, e.JobID)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	const ins = `INSERT INTO events (run_id, job_id, kind, message, detail, at)
	             VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`
	var id int64
	err := q.QueryRow(ctx, ins, e.RunID, e.JobID, e.Kind, e.Message,
		nonNilAnyMap(e.Detail), utc(e.At)).Scan(&id)
	return mapErr("pg: RecordEvent", err)
}

// ListEvents returns the timeline, oldest first. A non-zero runID or jobID
// narrows the result; both zero returns every event.
func (s *Store) ListEvents(ctx context.Context, runID, jobID int64) ([]store.Event, error) {
	const q = `SELECT id, run_id, job_id, kind, message, detail, at FROM events
	           WHERE ($1 = 0 OR run_id = $1) AND ($2 = 0 OR job_id = $2)
	           ORDER BY id`
	rows, err := s.pool.Query(ctx, q, runID, jobID)
	if err != nil {
		return nil, mapErr("pg: ListEvents", err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.JobID, &e.Kind, &e.Message, &e.Detail, &e.At); err != nil {
			return nil, mapErr("pg: ListEvents", err)
		}
		e.Detail = emptyMapToNil(e.Detail)
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, mapErr("pg: ListEvents", rows.Err())
}
