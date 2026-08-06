package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Event kinds written by the queue. They are the job timeline the UI reads
// back, and the only place a requeue's explanation is recorded.
const (
	EventDispatched  = "dispatched"
	EventRedispatched = "redispatched"
	EventRequeued    = "requeued"
	EventRunnerLost  = "runner_lost"
)

// Enqueue makes a job eligible for dispatch. It is idempotent on job id: a job
// that is already queued or already leased keeps its row, its queue position,
// and its lease. Two schedulers racing to enqueue the same job produce one row.
func (s *Store) Enqueue(ctx context.Context, q store.QueuedJob) error {
	if q.JobID == 0 {
		return fmt.Errorf("pg: Enqueue: queued job has no job id")
	}
	queuedAt := q.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = time.Now()
	}
	notBefore := q.NotBefore
	if notBefore.IsZero() {
		notBefore = queuedAt
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		// Read without FOR UPDATE: every queue transaction locks job_queue
		// before jobs, and taking the jobs lock first here would invert that
		// order and deadlock against Dequeue.
		var attempt int
		var runID int64
		err := tx.QueryRow(ctx, `SELECT run_id, attempt FROM jobs WHERE id = $1`, q.JobID).
			Scan(&runID, &attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pg: Enqueue: job %d: %w", q.JobID, store.ErrNotFound)
		}
		if err != nil {
			return mapErr("pg: Enqueue", err)
		}
		if q.RunID != 0 && q.RunID != runID {
			return fmt.Errorf("pg: Enqueue: job %d belongs to run %d, not %d", q.JobID, runID, q.RunID)
		}

		const ins = `
INSERT INTO job_queue (job_id, run_id, attempt, labels, concurrency_group, priority,
	queued_at, not_before, state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
ON CONFLICT (job_id) DO NOTHING`
		tag, err := tx.Exec(ctx, ins, q.JobID, runID, attempt, nonNilStrings(q.Labels), q.Group,
			q.Priority, utc(queuedAt), utc(notBefore))
		if err != nil {
			return mapErr("pg: Enqueue", err)
		}
		if tag.RowsAffected() == 0 {
			// Already queued or leased. Not an error, and not a second row.
			return nil
		}
		const upd = `
UPDATE jobs SET status = 'queued', labels = $2, concurrency_group = $3,
	queued_at = COALESCE(queued_at, $4)
WHERE id = $1`
		if _, err := tx.Exec(ctx, upd, q.JobID, nonNilStrings(q.Labels), q.Group, utc(queuedAt)); err != nil {
			return mapErr("pg: Enqueue", err)
		}
		return nil
	})
}

// Dequeue atomically claims one eligible job for a runner and returns it with a
// lease held until now+ttl.
//
// Eligibility: state 'queued', not_before reached, and every label the job
// requires present in the runner's label set (jsonb containment, runner @> job).
// Ordering is priority descending then queued_at ascending. The claim is a
// single UPDATE ... FROM (SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1), so
// concurrent runners never see the same row and never block on each other.
func (s *Store) Dequeue(ctx context.Context, runnerID string, labels []string, ttl time.Duration) (*model.Job, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("pg: Dequeue: empty runner id")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("pg: Dequeue: lease ttl must be positive, got %s", ttl)
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)

	var job *model.Job
	err := s.tx(ctx, func(tx pgx.Tx) error {
		const claim = `
UPDATE job_queue q
SET state = 'leased', runner_id = $1, lease_expires_at = $2
FROM (
    SELECT job_id FROM job_queue
    WHERE state = 'queued'
      AND not_before <= $3
      AND $4::jsonb @> labels
    ORDER BY priority DESC, queued_at ASC, job_id ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
) sel
WHERE q.job_id = sel.job_id
RETURNING q.job_id, q.run_id, q.attempt`
		var jobID, runID int64
		var attempt int
		err := tx.QueryRow(ctx, claim, runnerID, expires, now, nonNilStrings(labels)).
			Scan(&jobID, &runID, &attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return mapErr("pg: Dequeue", err)
		}

		// The dispatch record is written in the same transaction as the lease.
		// A control-plane restart in between is therefore impossible: either
		// both landed or neither did.
		const dispatch = `
INSERT INTO job_dispatches (run_id, job_id, attempt, runner_id, first_dispatched_at, last_dispatched_at)
VALUES ($1,$2,$3,$4,$5,$5)
ON CONFLICT (run_id, job_id, attempt) DO NOTHING`
		tag, err := tx.Exec(ctx, dispatch, runID, jobID, attempt, runnerID, now)
		if err != nil {
			return mapErr("pg: Dequeue", err)
		}
		first := tag.RowsAffected() == 1
		if !first {
			// Redelivery after a lease loss: the same dispatch, handed out
			// again. It never becomes a second dispatch record.
			const bump = `
UPDATE job_dispatches SET runner_id = $4, last_dispatched_at = $5, dispatch_count = dispatch_count + 1
WHERE run_id = $1 AND job_id = $2 AND attempt = $3`
			if _, err := tx.Exec(ctx, bump, runID, jobID, attempt, runnerID, now); err != nil {
				return mapErr("pg: Dequeue", err)
			}
		}

		const upd = `
UPDATE jobs SET status = 'in_progress', runner_id = $2, lease_expires_at = $3,
	last_heartbeat_at = $4, started_at = $4, setup_completed_at = NULL
WHERE id = $1
RETURNING ` + jobCols
		job, err = scanJob(tx.QueryRow(ctx, upd, jobID, runnerID, expires, now))
		if err != nil {
			return mapErr("pg: Dequeue", err)
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

// Heartbeat extends the lease, but only for the runner that still holds it. A
// runner whose lease was reaped gets ErrLeaseLost and must stop work.
func (s *Store) Heartbeat(ctx context.Context, runnerID string, jobID int64, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("pg: Heartbeat: lease ttl must be positive, got %s", ttl)
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	return s.tx(ctx, func(tx pgx.Tx) error {
		const q = `
UPDATE job_queue SET lease_expires_at = $3
WHERE job_id = $2 AND runner_id = $1 AND state = 'leased'`
		tag, err := tx.Exec(ctx, q, runnerID, jobID, expires)
		if err != nil {
			return mapErr("pg: Heartbeat", err)
		}
		if tag.RowsAffected() == 0 {
			return store.ErrLeaseLost
		}
		const upd = `UPDATE jobs SET lease_expires_at = $2, last_heartbeat_at = $3 WHERE id = $1`
		if _, err := tx.Exec(ctx, upd, jobID, expires, now); err != nil {
			return mapErr("pg: Heartbeat", err)
		}
		return nil
	})
}

// ReleaseLease drops a lease without completing the job and puts it back on the
// queue. The reason is validated first: a requeue nobody can explain is the bug
// this signature exists to prevent.
func (s *Store) ReleaseLease(ctx context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	if err := reason.Validate(); err != nil {
		return fmt.Errorf("pg: ReleaseLease: %w", err)
	}
	now := time.Now().UTC()
	return s.tx(ctx, func(tx pgx.Tx) error {
		const q = `
UPDATE job_queue
SET state = 'queued', runner_id = '', lease_expires_at = NULL,
    requeue_count = requeue_count + 1, not_before = $3
WHERE job_id = $2 AND runner_id = $1 AND state = 'leased'
RETURNING run_id, requeue_count`
		var runID int64
		var requeues int
		err := tx.QueryRow(ctx, q, runnerID, jobID, now).Scan(&runID, &requeues)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrLeaseLost
		}
		if err != nil {
			return mapErr("pg: ReleaseLease", err)
		}
		const upd = `
UPDATE jobs SET status = 'queued', runner_id = '', lease_expires_at = NULL,
	started_at = NULL, setup_completed_at = NULL, requeue_count = $2
WHERE id = $1`
		if _, err := tx.Exec(ctx, upd, jobID, requeues); err != nil {
			return mapErr("pg: ReleaseLease", err)
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
//
// Safe to run from several control-plane replicas: the UPDATE re-checks
// state = 'leased' under a row lock, so a lease is reaped exactly once and
// requeue_count is incremented exactly once.
func (s *Store) ReapExpiredLeases(ctx context.Context, now time.Time) ([]*model.Job, error) {
	now = now.UTC()
	var jobs []*model.Job
	err := s.tx(ctx, func(tx pgx.Tx) error {
		const claim = `
UPDATE job_queue
SET state = 'queued', runner_id = '', lease_expires_at = NULL,
    requeue_count = requeue_count + 1, not_before = $1
WHERE state = 'leased' AND lease_expires_at <= $1
RETURNING job_id, run_id, requeue_count, runner_id`
		rows, err := tx.Query(ctx, claim, now)
		if err != nil {
			return mapErr("pg: ReapExpiredLeases", err)
		}
		type reaped struct {
			jobID, runID int64
			requeues     int
			runnerID     string
		}
		var found []reaped
		for rows.Next() {
			var r reaped
			if err := rows.Scan(&r.jobID, &r.runID, &r.requeues, &r.runnerID); err != nil {
				rows.Close()
				return mapErr("pg: ReapExpiredLeases", err)
			}
			found = append(found, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr("pg: ReapExpiredLeases", err)
		}

		for _, r := range found {
			const upd = `
UPDATE jobs SET status = 'queued', runner_id = '', lease_expires_at = NULL,
	started_at = NULL, setup_completed_at = NULL, requeue_count = $2
WHERE id = $1
RETURNING ` + jobCols
			j, err := scanJob(tx.QueryRow(ctx, upd, r.jobID, r.requeues))
			if err != nil {
				return mapErr("pg: ReapExpiredLeases", err)
			}
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
// starving label behind a runner that will never pick the work up.
func (s *Store) QueueStats(ctx context.Context, now time.Time) (*store.QueueStats, error) {
	now = now.UTC()
	out := &store.QueueStats{
		DepthByLabel:   map[string]int{},
		RunnersByLabel: map[string]int{},
		IdleByLabel:    map[string]int{},
		At:             now,
	}

	const depthQ = `SELECT job_id, labels, queued_at FROM job_queue WHERE state = 'queued'`
	rows, err := s.pool.Query(ctx, depthQ)
	if err != nil {
		return nil, mapErr("pg: QueueStats", err)
	}
	var oldest time.Time
	for rows.Next() {
		var jobID int64
		var labels []string
		var queuedAt time.Time
		if err := rows.Scan(&jobID, &labels, &queuedAt); err != nil {
			rows.Close()
			return nil, mapErr("pg: QueueStats", err)
		}
		out.Depth++
		for _, l := range labels {
			out.DepthByLabel[l]++
		}
		if oldest.IsZero() || queuedAt.Before(oldest) {
			oldest = queuedAt
			out.OldestJobID = jobID
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapErr("pg: QueueStats", err)
	}
	if !oldest.IsZero() {
		if d := now.Sub(oldest); d > 0 {
			out.OldestWaiting = d
		}
	}

	const runnerQ = `SELECT labels, state FROM runners WHERE state IN ('idle', 'busy')`
	rrows, err := s.pool.Query(ctx, runnerQ)
	if err != nil {
		return nil, mapErr("pg: QueueStats", err)
	}
	for rrows.Next() {
		var labels []string
		var state string
		if err := rrows.Scan(&labels, &state); err != nil {
			rrows.Close()
			return nil, mapErr("pg: QueueStats", err)
		}
		for _, l := range labels {
			out.RunnersByLabel[l]++
			if state == string(model.RunnerIdle) {
				out.IdleByLabel[l]++
			}
		}
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, mapErr("pg: QueueStats", err)
	}

	for label, depth := range out.DepthByLabel {
		if depth > 0 && out.RunnersByLabel[label] == 0 {
			out.StarvedLabels = append(out.StarvedLabels, label)
		}
	}
	sort.Strings(out.StarvedLabels)
	return out, nil
}

func (s *Store) RecordQueueSample(ctx context.Context, sample store.QueueSample) error {
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	const q = `INSERT INTO queue_samples (at, depth, depth_by_label, busy, idle)
	           VALUES ($1,$2,$3,$4,$5) RETURNING id`
	byLabel := sample.DepthByLabel
	if byLabel == nil {
		byLabel = map[string]int{}
	}
	var id int64
	err := s.pool.QueryRow(ctx, q, utc(sample.At), sample.Depth, byLabel, sample.Busy, sample.Idle).Scan(&id)
	return mapErr("pg: RecordQueueSample", err)
}

func (s *Store) QueueDepthHistory(ctx context.Context, since time.Time) ([]store.QueueSample, error) {
	const q = `SELECT at, depth, depth_by_label, busy, idle FROM queue_samples
	           WHERE at >= $1 ORDER BY at, id`
	rows, err := s.pool.Query(ctx, q, utc(since))
	if err != nil {
		return nil, mapErr("pg: QueueDepthHistory", err)
	}
	defer rows.Close()
	var out []store.QueueSample
	for rows.Next() {
		var sample store.QueueSample
		if err := rows.Scan(&sample.At, &sample.Depth, &sample.DepthByLabel, &sample.Busy, &sample.Idle); err != nil {
			return nil, mapErr("pg: QueueDepthHistory", err)
		}
		sample.At = sample.At.UTC()
		sample.DepthByLabel = emptyMapToNil(sample.DepthByLabel)
		out = append(out, sample)
	}
	return out, mapErr("pg: QueueDepthHistory", rows.Err())
}
