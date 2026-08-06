package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

const stepCols = `id, job_id, number, name, step_id, status, conclusion, class, exit_code,
	continue_on_error, outputs, attempt, started_at, completed_at, log_start, log_end`

func scanStep(row pgx.Row) (*model.Step, error) {
	var s model.Step
	var conclusion, class string
	if err := row.Scan(&s.ID, &s.JobID, &s.Number, &s.Name, &s.StepID, &s.Status, &conclusion,
		&class, &s.ExitCode, &s.ContinueOnError, &s.Outputs, &s.Attempt,
		&s.StartedAt, &s.CompletedAt, &s.LogStart, &s.LogEnd); err != nil {
		return nil, err
	}
	s.Conclusion = model.Conclusion(conclusion)
	s.Class = model.FailureClass(class)
	s.Outputs = emptyMapToNil(s.Outputs)
	s.StartedAt = utcp(s.StartedAt)
	s.CompletedAt = utcp(s.CompletedAt)
	return &s, nil
}

// UpsertStep keys on (job, attempt, number): every attempt keeps its own steps.
func (s *Store) UpsertStep(ctx context.Context, st *model.Step) error {
	if st == nil {
		return fmt.Errorf("pg: UpsertStep: nil step")
	}
	if st.JobID == 0 {
		return fmt.Errorf("pg: UpsertStep: step %d has no job id", st.Number)
	}
	if !st.Status.Valid() {
		return fmt.Errorf("pg: UpsertStep: invalid status %q", st.Status)
	}
	if !st.Class.Valid() {
		return fmt.Errorf("pg: UpsertStep: invalid failure class %q", st.Class)
	}
	const q = `
INSERT INTO steps (job_id, attempt, number, name, step_id, status, conclusion, class, exit_code,
	continue_on_error, outputs, started_at, completed_at, log_start, log_end)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (job_id, attempt, number) DO UPDATE SET
	name = EXCLUDED.name, step_id = EXCLUDED.step_id, status = EXCLUDED.status,
	conclusion = EXCLUDED.conclusion, class = EXCLUDED.class, exit_code = EXCLUDED.exit_code,
	continue_on_error = EXCLUDED.continue_on_error, outputs = EXCLUDED.outputs,
	started_at = EXCLUDED.started_at, completed_at = EXCLUDED.completed_at,
	log_start = EXCLUDED.log_start, log_end = EXCLUDED.log_end
RETURNING id`
	err := s.pool.QueryRow(ctx, q, st.JobID, st.Attempt, st.Number, st.Name, st.StepID,
		string(st.Status), string(st.Conclusion), string(st.Class), st.ExitCode,
		st.ContinueOnError, nonNilStringMap(st.Outputs), utcp(st.StartedAt), utcp(st.CompletedAt),
		st.LogStart, st.LogEnd).Scan(&st.ID)
	return mapErr("pg: UpsertStep", err)
}

func (s *Store) ListSteps(ctx context.Context, jobID int64, attempt int) ([]*model.Step, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+stepCols+` FROM steps WHERE job_id = $1 AND attempt = $2 ORDER BY number`,
		jobID, attempt)
	if err != nil {
		return nil, mapErr("pg: ListSteps", err)
	}
	defer rows.Close()
	var out []*model.Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, mapErr("pg: ListSteps", err)
		}
		out = append(out, st)
	}
	return out, mapErr("pg: ListSteps", rows.Err())
}
