package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// scanner is what QueryRow and Rows have in common.
type scanner interface{ Scan(dest ...any) error }

const runCols = `id, repo_id, repo_full_name, workflow_name, workflow_path, run_number, attempt,
	event, head_sha, head_branch, base_branch, actor, is_fork_pr, approved, approved_by,
	check_suite_id, status, conclusion, cancel_actor, cancel_sentence, cancel_triggered_by,
	event_payload, inputs, created_at, started_at, completed_at`

func scanRun(row scanner) (*model.Run, error) {
	var r model.Run
	var conclusion, cancelActor, cancelSentence, cancelBy, inputs string
	var payload, startedAt, completedAt sql.NullString
	var createdAt string
	if err := row.Scan(
		&r.ID, &r.RepoID, &r.RepoFull, &r.WorkflowName, &r.WorkflowPath, &r.RunNumber, &r.Attempt,
		&r.Event, &r.HeadSHA, &r.HeadBranch, &r.BaseBranch, &r.Actor, &r.IsForkPR, &r.Approved, &r.ApprovedBy,
		&r.CheckSuiteID, &r.Status, &conclusion, &cancelActor, &cancelSentence, &cancelBy,
		&payload, &inputs, &createdAt, &startedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	r.Conclusion = model.Conclusion(conclusion)
	r.Cancel = cancelFrom(cancelActor, cancelSentence, cancelBy)
	if payload.Valid && payload.String != "" {
		r.EventPayload = json.RawMessage(payload.String)
	}
	if err := jsonInto(inputs, &r.Inputs); err != nil {
		return nil, err
	}
	r.Inputs = emptyMapToNil(r.Inputs)

	var err error
	if r.CreatedAt, err = mustTime(createdAt); err != nil {
		return nil, err
	}
	if r.StartedAt, err = nullTime(startedAt); err != nil {
		return nil, err
	}
	if r.CompletedAt, err = nullTime(completedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRun inserts a run and fills in the allocated id.
func (s *Store) CreateRun(ctx context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("sqlite: CreateRun: nil run")
	}
	if r.ID != 0 {
		return fmt.Errorf("sqlite: CreateRun: id %d already set; the store allocates ids", r.ID)
	}
	if !r.Status.Valid() {
		return fmt.Errorf("sqlite: CreateRun: invalid status %q", r.Status)
	}
	if err := validCancel(r.Cancel); err != nil {
		return fmt.Errorf("sqlite: CreateRun: %w", err)
	}
	actor, sentence, by := cancelColumns(r.Cancel)
	var payload any
	if len(r.EventPayload) > 0 {
		payload = string(r.EventPayload)
	}
	inputs, err := jsonText(r.Inputs)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO runs (repo_id, repo_full_name, workflow_name, workflow_path, run_number, attempt,
	event, head_sha, head_branch, base_branch, actor, is_fork_pr, approved, approved_by,
	check_suite_id, status, conclusion, cancel_actor, cancel_sentence, cancel_triggered_by,
	event_payload, inputs, created_at, started_at, completed_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
RETURNING id`
	err = s.db.QueryRowContext(ctx, q,
		r.RepoID, r.RepoFull, r.WorkflowName, r.WorkflowPath, r.RunNumber, r.Attempt,
		r.Event, r.HeadSHA, r.HeadBranch, r.BaseBranch, r.Actor, boolInt(r.IsForkPR), boolInt(r.Approved), r.ApprovedBy,
		r.CheckSuiteID, string(r.Status), string(r.Conclusion), actor, sentence, by,
		payload, inputs, ts(r.CreatedAt), tsp(r.StartedAt), tsp(r.CompletedAt),
	).Scan(&r.ID)
	return mapErr("sqlite: CreateRun", err)
}

func (s *Store) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr("sqlite: GetRun", err)
	}
	return r, nil
}

func (s *Store) UpdateRun(ctx context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("sqlite: UpdateRun: nil run")
	}
	if r.ID == 0 {
		return fmt.Errorf("sqlite: UpdateRun: run has no id")
	}
	if !r.Status.Valid() {
		return fmt.Errorf("sqlite: UpdateRun: invalid status %q", r.Status)
	}
	if err := validCancel(r.Cancel); err != nil {
		return fmt.Errorf("sqlite: UpdateRun: %w", err)
	}
	actor, sentence, by := cancelColumns(r.Cancel)
	var payload any
	if len(r.EventPayload) > 0 {
		payload = string(r.EventPayload)
	}
	inputs, err := jsonText(r.Inputs)
	if err != nil {
		return err
	}
	const q = `
UPDATE runs SET repo_id=?, repo_full_name=?, workflow_name=?, workflow_path=?, run_number=?,
	attempt=?, event=?, head_sha=?, head_branch=?, base_branch=?, actor=?, is_fork_pr=?,
	approved=?, approved_by=?, check_suite_id=?, status=?, conclusion=?, cancel_actor=?,
	cancel_sentence=?, cancel_triggered_by=?, event_payload=?, inputs=?, created_at=?,
	started_at=?, completed_at=?
WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q,
		r.RepoID, r.RepoFull, r.WorkflowName, r.WorkflowPath, r.RunNumber, r.Attempt,
		r.Event, r.HeadSHA, r.HeadBranch, r.BaseBranch, r.Actor, boolInt(r.IsForkPR), boolInt(r.Approved), r.ApprovedBy,
		r.CheckSuiteID, string(r.Status), string(r.Conclusion), actor, sentence, by,
		payload, inputs, ts(r.CreatedAt), tsp(r.StartedAt), tsp(r.CompletedAt), r.ID)
	if err != nil {
		return mapErr("sqlite: UpdateRun", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: UpdateRun", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// runWhere builds the shared filter for ListRuns and CountRuns.
func runWhere(f store.RunFilter) (string, []any) {
	var clauses []string
	var args []any
	add := func(expr string, vals ...any) {
		args = append(args, vals...)
		clauses = append(clauses, expr)
	}
	if f.RepoID != 0 {
		add("repo_id = ?", f.RepoID)
	}
	if f.Branch != "" {
		add("head_branch = ?", f.Branch)
	}
	if f.Actor != "" {
		add("actor = ?", f.Actor)
	}
	if f.Event != "" {
		add("event = ?", f.Event)
	}
	if f.Status != "" {
		add("status = ?", string(f.Status))
	}
	if f.Conclusion != "" {
		add("conclusion = ?", string(f.Conclusion))
	}
	if f.Workflow != "" {
		add("(workflow_path = ? OR workflow_name = ?)", f.Workflow, f.Workflow)
	}
	if f.Search != "" {
		// SQLite's LIKE is already case-insensitive for ASCII, which is what
		// Postgres needed ILIKE for.
		like := "%" + f.Search + "%"
		add("(workflow_name LIKE ? OR head_branch LIKE ? OR head_sha LIKE ? OR actor LIKE ?)",
			like, like, like, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) ListRuns(ctx context.Context, f store.RunFilter) ([]*model.Run, error) {
	where, args := runWhere(f)
	q := `SELECT ` + runCols + ` FROM runs` + where + ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += " LIMIT ?"
	}
	if f.Offset > 0 {
		// SQLite has no bare OFFSET: it is only legal after a LIMIT, so an
		// offset with no limit needs the "everything" sentinel.
		if f.Limit <= 0 {
			q += " LIMIT -1"
		}
		args = append(args, f.Offset)
		q += " OFFSET ?"
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr("sqlite: ListRuns", err)
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, mapErr("sqlite: ListRuns", err)
		}
		out = append(out, r)
	}
	return out, mapErr("sqlite: ListRuns", rows.Err())
}

func (s *Store) CountRuns(ctx context.Context, f store.RunFilter) (int, error) {
	where, args := runWhere(f)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs`+where, args...).Scan(&n)
	return n, mapErr("sqlite: CountRuns", err)
}

func (s *Store) ListRunsForSHA(ctx context.Context, repoID int64, sha string) ([]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE repo_id = ? AND head_sha = ? ORDER BY created_at DESC, id DESC`,
		repoID, sha)
	if err != nil {
		return nil, mapErr("sqlite: ListRunsForSHA", err)
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, mapErr("sqlite: ListRunsForSHA", err)
		}
		out = append(out, r)
	}
	return out, mapErr("sqlite: ListRunsForSHA", rows.Err())
}

// NextRunNumber allocates the next per-(repo, workflow) number. The upsert
// serializes concurrent allocations on the row, so no two runs share a number.
func (s *Store) NextRunNumber(ctx context.Context, repoID int64, workflowPath string) (int64, error) {
	const q = `
INSERT INTO run_numbers (repo_id, workflow_path, current) VALUES (?, ?, 1)
ON CONFLICT (repo_id, workflow_path) DO UPDATE SET current = run_numbers.current + 1
RETURNING current`
	var n int64
	err := s.db.QueryRowContext(ctx, q, repoID, workflowPath).Scan(&n)
	return n, mapErr("sqlite: NextRunNumber", err)
}

// validCancel rejects a cancellation with no explanation before it reaches the
// database's own CHECK, so the caller gets the model's error message.
func validCancel(c *model.CancelReason) error {
	if c == nil {
		return nil
	}
	return c.Validate()
}
