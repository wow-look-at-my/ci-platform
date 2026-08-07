package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const jobCols = `id, run_id, key, name, matrix_key, matrix, needs, labels, attempt, max_attempts,
	status, conclusion, class, cancel_actor, cancel_sentence, cancel_triggered_by,
	concurrency_group, cancel_in_progress, continue_on_error, timeout_minutes, check_run_id,
	runner_id, environment, awaiting_approval, outputs, failure_explanation, created_at,
	queued_at, started_at, setup_completed_at, completed_at, lease_expires_at, last_heartbeat_at,
	requeue_count, infra_retry_count, classification_log`

func scanJob(row scanner) (*model.Job, error) {
	var j model.Job
	var conclusion, class, cancelActor, cancelSentence, cancelBy string
	var matrix, needs, labels, outputs, classLog, createdAt string
	var queuedAt, startedAt, setupAt, completedAt, leaseAt, heartbeatAt sql.NullString
	if err := row.Scan(
		&j.ID, &j.RunID, &j.Key, &j.Name, &j.MatrixKey, &matrix, &needs, &labels,
		&j.Attempt, &j.MaxAttempts, &j.Status, &conclusion, &class,
		&cancelActor, &cancelSentence, &cancelBy,
		&j.ConcurrencyGroup, &j.CancelInProgress, &j.ContinueOnError, &j.TimeoutMinutes,
		&j.CheckRunID, &j.RunnerID, &j.Environment, &j.AwaitingApproval, &outputs,
		&j.FailureExplained, &createdAt, &queuedAt, &startedAt, &setupAt,
		&completedAt, &leaseAt, &heartbeatAt,
		&j.RequeueCount, &j.InfraRetryCount, &classLog,
	); err != nil {
		return nil, err
	}
	j.Conclusion = model.Conclusion(conclusion)
	j.Class = model.FailureClass(class)
	j.Cancel = cancelFrom(cancelActor, cancelSentence, cancelBy)

	for _, d := range []struct {
		raw string
		dst any
	}{
		{matrix, &j.Matrix}, {needs, &j.Needs}, {labels, &j.Labels},
		{outputs, &j.Outputs}, {classLog, &j.ClassificationLog},
	} {
		if err := jsonInto(d.raw, d.dst); err != nil {
			return nil, err
		}
	}
	j.Matrix = emptyMapToNil(j.Matrix)
	j.Outputs = emptyMapToNil(j.Outputs)
	j.Needs = emptyToNil(j.Needs)
	j.Labels = emptyToNil(j.Labels)
	j.ClassificationLog = emptyToNil(j.ClassificationLog)

	var err error
	if j.CreatedAt, err = mustTime(createdAt); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		raw sql.NullString
		dst **time.Time
	}{
		{queuedAt, &j.QueuedAt}, {startedAt, &j.StartedAt}, {setupAt, &j.SetupCompletedAt},
		{completedAt, &j.CompletedAt}, {leaseAt, &j.LeaseExpiresAt}, {heartbeatAt, &j.LastHeartbeatAt},
	} {
		if *d.dst, err = nullTime(d.raw); err != nil {
			return nil, err
		}
	}
	return &j, nil
}

func checkJob(j *model.Job) error {
	if j == nil {
		return fmt.Errorf("nil job")
	}
	if !j.Status.Valid() {
		return fmt.Errorf("invalid status %q", j.Status)
	}
	if j.Conclusion != "" && !j.Conclusion.Valid() {
		return fmt.Errorf("invalid conclusion %q", j.Conclusion)
	}
	if !j.Class.Valid() {
		return fmt.Errorf("invalid failure class %q", j.Class)
	}
	return validCancel(j.Cancel)
}

// jobArgs is the column order shared by CreateJob and UpdateJob, so the two
// cannot drift into writing different things.
func jobArgs(j *model.Job) ([]any, error) {
	actor, sentence, by := cancelColumns(j.Cancel)
	matrix, err := jsonText(j.Matrix)
	if err != nil {
		return nil, err
	}
	needs, err := jsonText(j.Needs)
	if err != nil {
		return nil, err
	}
	labels, err := jsonText(j.Labels)
	if err != nil {
		return nil, err
	}
	outputs, err := jsonText(j.Outputs)
	if err != nil {
		return nil, err
	}
	classLog, err := jsonText(j.ClassificationLog)
	if err != nil {
		return nil, err
	}
	return []any{
		j.RunID, j.Key, j.Name, j.MatrixKey, matrix, needs, labels, j.Attempt, j.MaxAttempts,
		string(j.Status), string(j.Conclusion), string(j.Class), actor, sentence, by,
		j.ConcurrencyGroup, boolInt(j.CancelInProgress), boolInt(j.ContinueOnError),
		j.TimeoutMinutes, j.CheckRunID, j.RunnerID, j.Environment, boolInt(j.AwaitingApproval),
		outputs, j.FailureExplained, ts(j.CreatedAt),
		tsp(j.QueuedAt), tsp(j.StartedAt), tsp(j.SetupCompletedAt), tsp(j.CompletedAt),
		tsp(j.LeaseExpiresAt), tsp(j.LastHeartbeatAt),
		j.RequeueCount, j.InfraRetryCount, classLog,
	}, nil
}

func (s *Store) CreateJob(ctx context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("sqlite: CreateJob: %w", err)
	}
	if j.ID != 0 {
		return fmt.Errorf("sqlite: CreateJob: id %d already set; the store allocates ids", j.ID)
	}
	args, err := jobArgs(j)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO jobs (run_id, key, name, matrix_key, matrix, needs, labels, attempt, max_attempts,
	status, conclusion, class, cancel_actor, cancel_sentence, cancel_triggered_by,
	concurrency_group, cancel_in_progress, continue_on_error, timeout_minutes, check_run_id,
	runner_id, environment, awaiting_approval, outputs, failure_explanation, created_at,
	queued_at, started_at, setup_completed_at, completed_at, lease_expires_at, last_heartbeat_at,
	requeue_count, infra_retry_count, classification_log)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
RETURNING id`
	return mapErr("sqlite: CreateJob", s.db.QueryRowContext(ctx, q, args...).Scan(&j.ID))
}

func (s *Store) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr("sqlite: GetJob", err)
	}
	return j, nil
}

// UpdateJob writes the whole job. A job that has reached a terminal status
// leaves the queue in the same transaction: a completed job must never remain
// dispatchable.
func (s *Store) UpdateJob(ctx context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("sqlite: UpdateJob: %w", err)
	}
	if j.ID == 0 {
		return fmt.Errorf("sqlite: UpdateJob: job has no id")
	}
	args, err := jobArgs(j)
	if err != nil {
		return err
	}
	args = append(args, j.ID)
	const q = `
UPDATE jobs SET run_id=?, key=?, name=?, matrix_key=?, matrix=?, needs=?, labels=?,
	attempt=?, max_attempts=?, status=?, conclusion=?, class=?, cancel_actor=?,
	cancel_sentence=?, cancel_triggered_by=?, concurrency_group=?, cancel_in_progress=?,
	continue_on_error=?, timeout_minutes=?, check_run_id=?, runner_id=?, environment=?,
	awaiting_approval=?, outputs=?, failure_explanation=?, created_at=?, queued_at=?,
	started_at=?, setup_completed_at=?, completed_at=?, lease_expires_at=?,
	last_heartbeat_at=?, requeue_count=?, infra_retry_count=?, classification_log=?
WHERE id = ?`
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return mapErr("sqlite: UpdateJob", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr("sqlite: UpdateJob", err)
		}
		if n == 0 {
			return store.ErrNotFound
		}
		if j.Status.Terminal() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM job_queue WHERE job_id = ?`, j.ID); err != nil {
				return mapErr("sqlite: UpdateJob: dequeue completed job", err)
			}
		}
		return nil
	})
}

func (s *Store) ListJobsForRun(ctx context.Context, runID int64) ([]*model.Job, error) {
	return s.queryJobs(ctx, "sqlite: ListJobsForRun",
		`SELECT `+jobCols+` FROM jobs WHERE run_id = ? ORDER BY id`, runID)
}

// ListJobsInConcurrencyGroup returns the live members of a group, oldest first,
// so the scheduler can admit one and hold or cancel the rest.
func (s *Store) ListJobsInConcurrencyGroup(ctx context.Context, group string) ([]*model.Job, error) {
	if group == "" {
		return nil, fmt.Errorf("sqlite: ListJobsInConcurrencyGroup: empty group")
	}
	return s.queryJobs(ctx, "sqlite: ListJobsInConcurrencyGroup",
		`SELECT `+jobCols+` FROM jobs
		 WHERE concurrency_group = ? AND status <> 'completed'
		 ORDER BY created_at, id`, group)
}

func (s *Store) queryJobs(ctx context.Context, op, q string, args ...any) ([]*model.Job, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(op, err)
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, mapErr(op, err)
		}
		out = append(out, j)
	}
	return out, mapErr(op, rows.Err())
}
