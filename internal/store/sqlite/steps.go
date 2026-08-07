package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

const stepCols = `id, job_id, number, name, step_id, status, conclusion, class, exit_code,
	continue_on_error, outputs, attempt, started_at, completed_at, log_start, log_end`

func scanStep(row scanner) (*model.Step, error) {
	var s model.Step
	var conclusion, class, outputs string
	var startedAt, completedAt sql.NullString
	if err := row.Scan(&s.ID, &s.JobID, &s.Number, &s.Name, &s.StepID, &s.Status, &conclusion,
		&class, &s.ExitCode, &s.ContinueOnError, &outputs, &s.Attempt,
		&startedAt, &completedAt, &s.LogStart, &s.LogEnd); err != nil {
		return nil, err
	}
	s.Conclusion = model.Conclusion(conclusion)
	s.Class = model.FailureClass(class)
	if err := jsonInto(outputs, &s.Outputs); err != nil {
		return nil, err
	}
	s.Outputs = emptyMapToNil(s.Outputs)

	var err error
	if s.StartedAt, err = nullTime(startedAt); err != nil {
		return nil, err
	}
	if s.CompletedAt, err = nullTime(completedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// UpsertStep keys on (job, attempt, number): every attempt keeps its own steps.
func (s *Store) UpsertStep(ctx context.Context, st *model.Step) error {
	if st == nil {
		return fmt.Errorf("sqlite: UpsertStep: nil step")
	}
	if st.JobID == 0 {
		return fmt.Errorf("sqlite: UpsertStep: step %d has no job id", st.Number)
	}
	if !st.Status.Valid() {
		return fmt.Errorf("sqlite: UpsertStep: invalid status %q", st.Status)
	}
	if !st.Class.Valid() {
		return fmt.Errorf("sqlite: UpsertStep: invalid failure class %q", st.Class)
	}
	outputs, err := jsonText(st.Outputs)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO steps (job_id, attempt, number, name, step_id, status, conclusion, class, exit_code,
	continue_on_error, outputs, started_at, completed_at, log_start, log_end)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (job_id, attempt, number) DO UPDATE SET
	name = excluded.name, step_id = excluded.step_id, status = excluded.status,
	conclusion = excluded.conclusion, class = excluded.class, exit_code = excluded.exit_code,
	continue_on_error = excluded.continue_on_error, outputs = excluded.outputs,
	started_at = excluded.started_at, completed_at = excluded.completed_at,
	log_start = excluded.log_start, log_end = excluded.log_end
RETURNING id`
	err = s.db.QueryRowContext(ctx, q, st.JobID, st.Attempt, st.Number, st.Name, st.StepID,
		string(st.Status), string(st.Conclusion), string(st.Class), st.ExitCode,
		boolInt(st.ContinueOnError), outputs, tsp(st.StartedAt), tsp(st.CompletedAt),
		st.LogStart, st.LogEnd).Scan(&st.ID)
	return mapErr("sqlite: UpsertStep", err)
}

func (s *Store) ListSteps(ctx context.Context, jobID int64, attempt int) ([]*model.Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepCols+` FROM steps WHERE job_id = ? AND attempt = ? ORDER BY number`,
		jobID, attempt)
	if err != nil {
		return nil, mapErr("sqlite: ListSteps", err)
	}
	defer rows.Close()
	var out []*model.Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, mapErr("sqlite: ListSteps", err)
		}
		out = append(out, st)
	}
	return out, mapErr("sqlite: ListSteps", rows.Err())
}
