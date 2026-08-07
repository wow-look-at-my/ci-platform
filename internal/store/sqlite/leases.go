package sqlite

// The lease half of the queue: extending a lease, giving one up, and reaping
// the ones whose runner stopped reporting.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Heartbeat extends the lease, but only for the runner that still holds it. A
// runner whose lease was reaped gets ErrLeaseLost and must stop work.
func (s *Store) Heartbeat(ctx context.Context, runnerID string, jobID int64, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("sqlite: Heartbeat: lease ttl must be positive, got %s", ttl)
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	return s.tx(ctx, func(tx *sql.Tx) error {
		const q = `
UPDATE job_queue SET lease_expires_at = ?
WHERE job_id = ? AND runner_id = ? AND state = 'leased'`
		res, err := tx.ExecContext(ctx, q, ts(expires), jobID, runnerID)
		if err != nil {
			return mapErr("sqlite: Heartbeat", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr("sqlite: Heartbeat", err)
		}
		if n == 0 {
			return store.ErrLeaseLost
		}
		const upd = `UPDATE jobs SET lease_expires_at = ?, last_heartbeat_at = ? WHERE id = ?`
		if _, err := tx.ExecContext(ctx, upd, ts(expires), ts(now), jobID); err != nil {
			return mapErr("sqlite: Heartbeat", err)
		}
		return nil
	})
}

// ReleaseLease drops a lease without completing the job and puts it back on the
// queue. The reason is validated first: a requeue nobody can explain is the bug
// this signature exists to prevent.
//
// not_before is left alone, here and in the reaper: a requeue is immediately
// dispatchable to another runner. Backoff belongs to a retry, which completes
// the job and enqueues the next attempt with its own NotBefore.
func (s *Store) ReleaseLease(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	if err := reason.Validate(); err != nil {
		return fmt.Errorf("sqlite: ReleaseLease: %w", err)
	}
	now := time.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		const q = `
UPDATE job_queue
SET state = 'queued', runner_id = '', lease_expires_at = NULL,
    requeue_count = requeue_count + 1
WHERE job_id = ? AND runner_id = ? AND state = 'leased'
RETURNING run_id, requeue_count`
		var runID int64
		var requeues int
		err := tx.QueryRowContext(ctx, q, jobID, runnerID).Scan(&runID, &requeues)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrLeaseLost
		}
		if err != nil {
			return mapErr("sqlite: ReleaseLease", err)
		}
		const upd = `
UPDATE jobs SET status = 'queued', runner_id = '', lease_expires_at = NULL,
	started_at = NULL, setup_completed_at = NULL, requeue_count = ?
WHERE id = ?`
		if _, err := tx.ExecContext(ctx, upd, requeues, jobID); err != nil {
			return mapErr("sqlite: ReleaseLease", err)
		}
		return s.recordEvent(ctx, tx, store.Event{
			RunID: runID, JobID: jobID, Kind: EventRequeued, Message: reason.Sentence,
			Detail: map[string]any{
				"actor":         string(reason.Actor),
				"triggered_by":  reason.TriggeredBy,
				"runner_id":     runnerID,
				"requeue_count": requeues,
			},
			At: now,
		})
	})
}

// ReapExpiredLeases requeues every job whose lease expired, in one transaction,
// and returns them. This is what turns "the runner disappeared" into a requeue
// rather than a lost or failed job: the status goes back to queued, never to
// failed, and the requeue is explained in the event log.
func (s *Store) ReapExpiredLeases(ctx context.Context, now time.Time) ([]*model.Job, error) {
	now = now.UTC()
	var jobs []*model.Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// The expired rows are read before they are updated: the runner_id is
		// about to be cleared, and the event has to name the runner that
		// vanished rather than say "the runner".
		const find = `
SELECT job_id, run_id, runner_id, requeue_count FROM job_queue
WHERE state = 'leased' AND lease_expires_at <= ?
ORDER BY job_id`
		rows, err := tx.QueryContext(ctx, find, ts(now))
		if err != nil {
			return mapErr("sqlite: ReapExpiredLeases", err)
		}
		type reaped struct {
			jobID, runID int64
			requeues     int
			runnerID     string
		}
		var found []reaped
		for rows.Next() {
			var r reaped
			if err := rows.Scan(&r.jobID, &r.runID, &r.runnerID, &r.requeues); err != nil {
				rows.Close()
				return mapErr("sqlite: ReapExpiredLeases", err)
			}
			r.requeues++
			found = append(found, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr("sqlite: ReapExpiredLeases", err)
		}

		for _, r := range found {
			const release = `
UPDATE job_queue SET state = 'queued', runner_id = '', lease_expires_at = NULL,
	requeue_count = ?
WHERE job_id = ? AND state = 'leased'`
			if _, err := tx.ExecContext(ctx, release, r.requeues, r.jobID); err != nil {
				return mapErr("sqlite: ReapExpiredLeases", err)
			}
			const upd = `
UPDATE jobs SET status = 'queued', runner_id = '', lease_expires_at = NULL,
	started_at = NULL, setup_completed_at = NULL, requeue_count = ?
WHERE id = ?
RETURNING ` + jobCols
			j, err := scanJob(tx.QueryRowContext(ctx, upd, r.requeues, r.jobID))
			if err != nil {
				return mapErr("sqlite: ReapExpiredLeases", err)
			}
			// The stored row is unleased, but the returned copy keeps the
			// runner it lost so the caller can name it. Without this the
			// requeue can only say "the runner", which is the report the
			// operator cannot act on.
			j.RunnerID = r.runnerID
			jobs = append(jobs, j)

			sentence := fmt.Sprintf(
				"Runner %s stopped reporting, so its lease on this job expired and the job was put "+
					"back on the queue (requeue %d). Nothing your workflow did caused this.",
				r.runnerID, r.requeues)
			if err := s.recordEvent(ctx, tx, store.Event{
				RunID: r.runID, JobID: r.jobID, Kind: EventRunnerLost, Message: sentence,
				Detail: map[string]any{
					"actor":         string(model.CancelActorRunnerLost),
					"runner_id":     r.runnerID,
					"requeue_count": r.requeues,
				},
				At: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		slog.Warn("requeued job after lease expiry",
			"job_id", j.ID, "run_id", j.RunID, "requeue_count", j.RequeueCount)
	}
	return jobs, nil
}

// QueueStats is the queue page's data and the starvation alarm.
//
// "Online" is idle or busy. A drained runner is deliberately not counted: it
// finishes what it has and takes nothing new, so counting it would hide a
