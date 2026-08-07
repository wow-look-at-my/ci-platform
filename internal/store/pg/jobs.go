package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const jobCols = `id, run_id, key, name, matrix_key, matrix, needs, labels, attempt, max_attempts,
	status, conclusion, class, cancel_actor, cancel_sentence, cancel_triggered_by,
	concurrency_group, cancel_in_progress, continue_on_error, timeout_minutes, check_run_id,
	runner_id, environment, awaiting_approval, outputs, failure_explanation, created_at,
	queued_at, started_at, setup_completed_at, completed_at, lease_expires_at, last_heartbeat_at,
	requeue_count, infra_retry_count, classification_log`

func scanJob(row pgx.Row) (*model.Job, error) {
	var j model.Job
	var conclusion, class, cancelActor, cancelSentence, cancelBy string
	if err := row.Scan(
		&j.ID, &j.RunID, &j.Key, &j.Name, &j.MatrixKey, &j.Matrix, &j.Needs, &j.Labels,
		&j.Attempt, &j.MaxAttempts, &j.Status, &conclusion, &class,
		&cancelActor, &cancelSentence, &cancelBy,
		&j.ConcurrencyGroup, &j.CancelInProgress, &j.ContinueOnError, &j.TimeoutMinutes,
		&j.CheckRunID, &j.RunnerID, &j.Environment, &j.AwaitingApproval, &j.Outputs,
		&j.FailureExplained, &j.CreatedAt, &j.QueuedAt, &j.StartedAt, &j.SetupCompletedAt,
		&j.CompletedAt, &j.LeaseExpiresAt, &j.LastHeartbeatAt,
		&j.RequeueCount, &j.InfraRetryCount, &j.ClassificationLog,
	); err != nil {
		return nil, err
	}
	j.Conclusion = model.Conclusion(conclusion)
	j.Class = model.FailureClass(class)
	j.Cancel = cancelFrom(cancelActor, cancelSentence, cancelBy)
	j.Matrix = emptyMapToNil(j.Matrix)
	j.Outputs = emptyMapToNil(j.Outputs)
	j.Needs = emptyToNil(j.Needs)
	j.Labels = emptyToNil(j.Labels)
	j.ClassificationLog = emptyToNil(j.ClassificationLog)
	j.CreatedAt = j.CreatedAt.UTC()
	j.QueuedAt = utcp(j.QueuedAt)
	j.StartedAt = utcp(j.StartedAt)
	j.SetupCompletedAt = utcp(j.SetupCompletedAt)
	j.CompletedAt = utcp(j.CompletedAt)
	j.LeaseExpiresAt = utcp(j.LeaseExpiresAt)
	j.LastHeartbeatAt = utcp(j.LastHeartbeatAt)
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

func (s *Store) CreateJob(ctx context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("pg: CreateJob: %w", err)
	}
	if j.ID != 0 {
		return fmt.Errorf("pg: CreateJob: id %d already set; the store allocates ids", j.ID)
	}
	actor, sentence, by := cancelColumns(j.Cancel)
	const q = `
INSERT INTO jobs (run_id, key, name, matrix_key, matrix, needs, labels, attempt, max_attempts,
	status, conclusion, class, cancel_actor, cancel_sentence, cancel_triggered_by,
	concurrency_group, cancel_in_progress, continue_on_error, timeout_minutes, check_run_id,
	runner_id, environment, awaiting_approval, outputs, failure_explanation, created_at,
	queued_at, started_at, setup_completed_at, completed_at, lease_expires_at, last_heartbeat_at,
	requeue_count, infra_retry_count, classification_log)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,
	$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)
RETURNING id`
	err := s.pool.QueryRow(ctx, q,
		j.RunID, j.Key, j.Name, j.MatrixKey, nonNilAnyMap(j.Matrix), nonNilStrings(j.Needs),
		nonNilStrings(j.Labels), j.Attempt, j.MaxAttempts,
		string(j.Status), string(j.Conclusion), string(j.Class), actor, sentence, by,
		j.ConcurrencyGroup, j.CancelInProgress, j.ContinueOnError, j.TimeoutMinutes, j.CheckRunID,
		j.RunnerID, j.Environment, j.AwaitingApproval, nonNilStringMap(j.Outputs),
		j.FailureExplained, utc(j.CreatedAt),
		utcp(j.QueuedAt), utcp(j.StartedAt), utcp(j.SetupCompletedAt), utcp(j.CompletedAt),
		utcp(j.LeaseExpiresAt), utcp(j.LastHeartbeatAt),
		j.RequeueCount, j.InfraRetryCount, nonNilStrings(j.ClassificationLog),
	).Scan(&j.ID)
	return mapErr("pg: CreateJob", err)
}

func (s *Store) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr("pg: GetJob", err)
	}
	return j, nil
}

// UpdateJob writes the whole job. A job that has reached a terminal status
// leaves the queue in the same transaction: a completed job must never remain
// dispatchable.
func (s *Store) UpdateJob(ctx context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("pg: UpdateJob: %w", err)
	}
	if j.ID == 0 {
		return fmt.Errorf("pg: UpdateJob: job has no id")
	}
	actor, sentence, by := cancelColumns(j.Cancel)
	const q = `
UPDATE jobs SET run_id=$2, key=$3, name=$4, matrix_key=$5, matrix=$6, needs=$7, labels=$8,
	attempt=$9, max_attempts=$10, status=$11, conclusion=$12, class=$13, cancel_actor=$14,
	cancel_sentence=$15, cancel_triggered_by=$16, concurrency_group=$17, cancel_in_progress=$18,
	continue_on_error=$19, timeout_minutes=$20, check_run_id=$21, runner_id=$22, environment=$23,
	awaiting_approval=$24, outputs=$25, failure_explanation=$26, created_at=$27, queued_at=$28,
	started_at=$29, setup_completed_at=$30, completed_at=$31, lease_expires_at=$32,
	last_heartbeat_at=$33, requeue_count=$34, infra_retry_count=$35, classification_log=$36
WHERE id = $1`
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, j.ID,
			j.RunID, j.Key, j.Name, j.MatrixKey, nonNilAnyMap(j.Matrix), nonNilStrings(j.Needs),
			nonNilStrings(j.Labels), j.Attempt, j.MaxAttempts,
			string(j.Status), string(j.Conclusion), string(j.Class), actor, sentence, by,
			j.ConcurrencyGroup, j.CancelInProgress, j.ContinueOnError, j.TimeoutMinutes, j.CheckRunID,
			j.RunnerID, j.Environment, j.AwaitingApproval, nonNilStringMap(j.Outputs),
			j.FailureExplained, utc(j.CreatedAt),
			utcp(j.QueuedAt), utcp(j.StartedAt), utcp(j.SetupCompletedAt), utcp(j.CompletedAt),
			utcp(j.LeaseExpiresAt), utcp(j.LastHeartbeatAt),
			j.RequeueCount, j.InfraRetryCount, nonNilStrings(j.ClassificationLog))
		if err != nil {
			return mapErr("pg: UpdateJob", err)
		}
		if tag.RowsAffected() == 0 {
			return store.ErrNotFound
		}
		if j.Status.Terminal() {
			if _, err := tx.Exec(ctx, `DELETE FROM job_queue WHERE job_id = $1`, j.ID); err != nil {
				return mapErr("pg: UpdateJob: dequeue completed job", err)
			}
		}
		return nil
	})
}

func (s *Store) ListJobsForRun(ctx context.Context, runID int64) ([]*model.Job, error) {
	return s.queryJobs(ctx, "pg: ListJobsForRun",
		`SELECT `+jobCols+` FROM jobs WHERE run_id = $1 ORDER BY id`, runID)
}

// ListJobsInConcurrencyGroup returns the live members of a group, oldest first,
// so the scheduler can admit one and hold or cancel the rest.
func (s *Store) ListJobsInConcurrencyGroup(ctx context.Context, group string) ([]*model.Job, error) {
	if group == "" {
		return nil, fmt.Errorf("pg: ListJobsInConcurrencyGroup: empty group")
	}
	return s.queryJobs(ctx, "pg: ListJobsInConcurrencyGroup",
		`SELECT `+jobCols+` FROM jobs
		 WHERE concurrency_group = $1 AND status <> 'completed'
		 ORDER BY created_at, id`, group)
}

func (s *Store) queryJobs(ctx context.Context, op, q string, args ...any) ([]*model.Job, error) {
	rows, err := s.pool.Query(ctx, q, args...)
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
