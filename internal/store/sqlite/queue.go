package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Event kinds written by the queue. They are the job timeline the UI reads
// back, and the only place a requeue's explanation is recorded.
const (
	EventDispatched   = "dispatched"
	EventRedispatched = "redispatched"
	EventRequeued     = "requeued"
	EventRunnerLost   = "runner_lost"
)

// Enqueue makes a job eligible for dispatch. It is idempotent on job id: a job
// that is already queued or already leased keeps its row, its queue position,
// and its lease. Two schedulers racing to enqueue the same job produce one row.
func (s *Store) Enqueue(ctx context.Context, q store.QueuedJob) error {
	if q.JobID == 0 {
		return fmt.Errorf("sqlite: Enqueue: queued job has no job id")
	}
	queuedAt := q.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = time.Now()
	}
	notBefore := q.NotBefore
	if notBefore.IsZero() {
		notBefore = queuedAt
	}
	labels, err := jsonText(q.Labels)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var attempt int
		var runID int64
		err := tx.QueryRowContext(ctx, `SELECT run_id, attempt FROM jobs WHERE id = ?`, q.JobID).
			Scan(&runID, &attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: Enqueue: job %d: %w", q.JobID, store.ErrNotFound)
		}
		if err != nil {
			return mapErr("sqlite: Enqueue", err)
		}
		if q.RunID != 0 && q.RunID != runID {
			return fmt.Errorf("sqlite: Enqueue: job %d belongs to run %d, not %d", q.JobID, runID, q.RunID)
		}
		// The caller states the attempt because it is half of the dispatch
		// idempotency key. Disagreeing with the job row means the caller and
		// the store have different ideas about which attempt this is, which
		// would let the same attempt dispatch twice; refuse rather than pick.
		if q.Attempt != 0 {
			if q.Attempt != attempt {
				return fmt.Errorf("sqlite: Enqueue: job %d is on attempt %d, caller enqueued attempt %d",
					q.JobID, attempt, q.Attempt)
			}
			attempt = q.Attempt
		}

		const ins = `
INSERT INTO job_queue (job_id, run_id, attempt, labels, concurrency_group, priority,
	queued_at, not_before, state)
VALUES (?,?,?,?,?,?,?,?,'queued')
ON CONFLICT (job_id) DO NOTHING`
		res, err := tx.ExecContext(ctx, ins, q.JobID, runID, attempt, labels, q.Group,
			q.Priority, ts(queuedAt), ts(notBefore))
		if err != nil {
			return mapErr("sqlite: Enqueue", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr("sqlite: Enqueue", err)
		}
		if n == 0 {
			// Already queued or leased. Not an error, and not a second row.
			return nil
		}
		const upd = `
UPDATE jobs SET status = 'queued', labels = ?, concurrency_group = ?,
	queued_at = COALESCE(queued_at, ?)
WHERE id = ?`
		if _, err := tx.ExecContext(ctx, upd, labels, q.Group, ts(queuedAt), q.JobID); err != nil {
			return mapErr("sqlite: Enqueue", err)
		}
		return nil
	})
}

// DropFromQueue removes a job's queue entry, lease and all. Missing is not an
// error: the caller wants the row gone, and it is.
func (s *Store) DropFromQueue(ctx context.Context, jobID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM job_queue WHERE job_id = ?`, jobID); err != nil {
		return mapErr("sqlite: DropFromQueue", err)
	}
	return nil
}

// labelsSatisfied is the eligibility clause: every label the job requires must
// be present in the runner's set. json_each walks the job's labels, and the
// NOT EXISTS fails the row on the first one the runner lacks.
const labelsSatisfied = `
NOT EXISTS (
    SELECT 1 FROM json_each(job_queue.labels) AS need
    WHERE need.value NOT IN (SELECT value FROM json_each(?))
)`

// Dequeue atomically claims one eligible job for a runner and returns it with a
// lease held until now+ttl.
//
// Eligibility: state 'queued', not_before reached, and every label the job
// requires present in the runner's label set. Ordering is priority descending
// then queued_at ascending. Selecting the candidate and taking its lease happen
// in one transaction on the store's single connection, so two runners polling
// at once cannot be handed the same job.
func (s *Store) Dequeue(ctx context.Context, runnerID string, labels []string, ttl time.Duration) (*model.Job, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("sqlite: Dequeue: empty runner id")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("sqlite: Dequeue: lease ttl must be positive, got %s", ttl)
	}
	runnerLabels, err := jsonText(labels)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)

	var job *model.Job
	err = s.tx(ctx, func(tx *sql.Tx) error {
		const pick = `
SELECT job_id, run_id, attempt FROM job_queue
WHERE state = 'queued'
  AND not_before <= ?
  AND ` + labelsSatisfied + `
ORDER BY priority DESC, queued_at ASC, job_id ASC
LIMIT 1`
		var jobID, runID int64
		var attempt int
		err := tx.QueryRowContext(ctx, pick, ts(now), runnerLabels).Scan(&jobID, &runID, &attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}

		const claim = `
UPDATE job_queue SET state = 'leased', runner_id = ?, lease_expires_at = ?
WHERE job_id = ? AND state = 'queued'`
		res, err := tx.ExecContext(ctx, claim, runnerID, ts(expires), jobID)
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}
		claimed, err := res.RowsAffected()
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}
		if claimed == 0 {
			// The row stopped being queued between the pick and the claim.
			// Reporting "nothing to do" is honest: this poll got no job.
			return store.ErrNotFound
		}

		// The dispatch record is written in the same transaction as the lease.
		// A control-plane restart in between is therefore impossible: either
		// both landed or neither did.
		const dispatch = `
INSERT INTO job_dispatches (run_id, job_id, attempt, runner_id, first_dispatched_at, last_dispatched_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT (run_id, job_id, attempt) DO NOTHING`
		res, err = tx.ExecContext(ctx, dispatch, runID, jobID, attempt, runnerID, ts(now), ts(now))
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}
		first := inserted == 1
		if !first {
			// Redelivery after a lease loss: the same dispatch, handed out
			// again. It never becomes a second dispatch record.
			const bump = `
UPDATE job_dispatches SET runner_id = ?, last_dispatched_at = ?, dispatch_count = dispatch_count + 1
WHERE run_id = ? AND job_id = ? AND attempt = ?`
			if _, err := tx.ExecContext(ctx, bump, runnerID, ts(now), runID, jobID, attempt); err != nil {
				return mapErr("sqlite: Dequeue", err)
			}
		}

		const upd = `
UPDATE jobs SET status = 'in_progress', runner_id = ?, lease_expires_at = ?,
	last_heartbeat_at = ?, started_at = ?, setup_completed_at = NULL
WHERE id = ?
RETURNING ` + jobCols
		job, err = scanJob(tx.QueryRowContext(ctx, upd, runnerID, ts(expires), ts(now), ts(now), jobID))
		if err != nil {
			return mapErr("sqlite: Dequeue", err)
		}

		kind, msg := EventDispatched, fmt.Sprintf("Dispatched to runner %s.", runnerID)
		if !first {
			kind = EventRedispatched
			msg = fmt.Sprintf("Redispatched to runner %s after the previous lease was lost.", runnerID)
		}
		return s.recordEvent(ctx, tx, store.Event{
			RunID: runID, JobID: jobID, Kind: kind, Message: msg,
			Detail: map[string]any{"runner_id": runnerID, "attempt": attempt},
			At:     now,
		})
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}
