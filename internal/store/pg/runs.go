package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const runCols = `id, repo_id, repo_full_name, workflow_name, workflow_path, run_number, attempt,
	event, head_sha, head_branch, base_branch, actor, is_fork_pr, approved, approved_by,
	check_suite_id, status, conclusion, cancel_actor, cancel_sentence, cancel_triggered_by,
	event_payload, inputs, created_at, started_at, completed_at`

func scanRun(row pgx.Row) (*model.Run, error) {
	var r model.Run
	var conclusion, cancelActor, cancelSentence, cancelBy string
	var payload []byte
	if err := row.Scan(
		&r.ID, &r.RepoID, &r.RepoFull, &r.WorkflowName, &r.WorkflowPath, &r.RunNumber, &r.Attempt,
		&r.Event, &r.HeadSHA, &r.HeadBranch, &r.BaseBranch, &r.Actor, &r.IsForkPR, &r.Approved, &r.ApprovedBy,
		&r.CheckSuiteID, &r.Status, &conclusion, &cancelActor, &cancelSentence, &cancelBy,
		&payload, &r.Inputs, &r.CreatedAt, &r.StartedAt, &r.CompletedAt,
	); err != nil {
		return nil, err
	}
	r.Conclusion = model.Conclusion(conclusion)
	r.Cancel = cancelFrom(cancelActor, cancelSentence, cancelBy)
	if len(payload) > 0 {
		r.EventPayload = json.RawMessage(payload)
	}
	r.Inputs = emptyMapToNil(r.Inputs)
	r.CreatedAt = r.CreatedAt.UTC()
	r.StartedAt = utcp(r.StartedAt)
	r.CompletedAt = utcp(r.CompletedAt)
	return &r, nil
}

// CreateRun inserts a run and fills in the allocated id.
func (s *Store) CreateRun(ctx context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("pg: CreateRun: nil run")
	}
	if r.ID != 0 {
		return fmt.Errorf("pg: CreateRun: id %d already set; the store allocates ids", r.ID)
	}
	if !r.Status.Valid() {
		return fmt.Errorf("pg: CreateRun: invalid status %q", r.Status)
	}
	if err := validCancel(r.Cancel); err != nil {
		return fmt.Errorf("pg: CreateRun: %w", err)
	}
	actor, sentence, by := cancelColumns(r.Cancel)
	var payload any
	if len(r.EventPayload) > 0 {
		payload = []byte(r.EventPayload)
	}
	const q = `
INSERT INTO runs (repo_id, repo_full_name, workflow_name, workflow_path, run_number, attempt,
	event, head_sha, head_branch, base_branch, actor, is_fork_pr, approved, approved_by,
	check_suite_id, status, conclusion, cancel_actor, cancel_sentence, cancel_triggered_by,
	event_payload, inputs, created_at, started_at, completed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
RETURNING id`
	err := s.pool.QueryRow(ctx, q,
		r.RepoID, r.RepoFull, r.WorkflowName, r.WorkflowPath, r.RunNumber, r.Attempt,
		r.Event, r.HeadSHA, r.HeadBranch, r.BaseBranch, r.Actor, r.IsForkPR, r.Approved, r.ApprovedBy,
		r.CheckSuiteID, string(r.Status), string(r.Conclusion), actor, sentence, by,
		payload, nonNilAnyMap(r.Inputs), utc(r.CreatedAt), utcp(r.StartedAt), utcp(r.CompletedAt),
	).Scan(&r.ID)
	return mapErr("pg: CreateRun", err)
}

func (s *Store) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr("pg: GetRun", err)
	}
	return r, nil
}

func (s *Store) UpdateRun(ctx context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("pg: UpdateRun: nil run")
	}
	if r.ID == 0 {
		return fmt.Errorf("pg: UpdateRun: run has no id")
	}
	if !r.Status.Valid() {
		return fmt.Errorf("pg: UpdateRun: invalid status %q", r.Status)
	}
	if err := validCancel(r.Cancel); err != nil {
		return fmt.Errorf("pg: UpdateRun: %w", err)
	}
	actor, sentence, by := cancelColumns(r.Cancel)
	var payload any
	if len(r.EventPayload) > 0 {
		payload = []byte(r.EventPayload)
	}
	const q = `
UPDATE runs SET repo_id=$2, repo_full_name=$3, workflow_name=$4, workflow_path=$5, run_number=$6,
	attempt=$7, event=$8, head_sha=$9, head_branch=$10, base_branch=$11, actor=$12, is_fork_pr=$13,
	approved=$14, approved_by=$15, check_suite_id=$16, status=$17, conclusion=$18, cancel_actor=$19,
	cancel_sentence=$20, cancel_triggered_by=$21, event_payload=$22, inputs=$23, created_at=$24,
	started_at=$25, completed_at=$26
WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, r.ID,
		r.RepoID, r.RepoFull, r.WorkflowName, r.WorkflowPath, r.RunNumber, r.Attempt,
		r.Event, r.HeadSHA, r.HeadBranch, r.BaseBranch, r.Actor, r.IsForkPR, r.Approved, r.ApprovedBy,
		r.CheckSuiteID, string(r.Status), string(r.Conclusion), actor, sentence, by,
		payload, nonNilAnyMap(r.Inputs), utc(r.CreatedAt), utcp(r.StartedAt), utcp(r.CompletedAt))
	if err != nil {
		return mapErr("pg: UpdateRun", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// runWhere builds the shared filter for ListRuns and CountRuns.
func runWhere(f store.RunFilter) (string, []any) {
	var clauses []string
	var args []any
	add := func(expr string, v any) {
		args = append(args, v)
		clauses = append(clauses, strings.ReplaceAll(expr, "?", "$"+strconv.Itoa(len(args))))
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
		add("(workflow_path = ? OR workflow_name = ?)", f.Workflow)
	}
	if f.Search != "" {
		add("(workflow_name ILIKE ? OR head_branch ILIKE ? OR head_sha ILIKE ? OR actor ILIKE ?)",
			"%"+f.Search+"%")
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
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if f.Offset > 0 {
		args = append(args, f.Offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr("pg: ListRuns", err)
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, mapErr("pg: ListRuns", err)
		}
		out = append(out, r)
	}
	return out, mapErr("pg: ListRuns", rows.Err())
}

func (s *Store) CountRuns(ctx context.Context, f store.RunFilter) (int, error) {
	where, args := runWhere(f)
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs`+where, args...).Scan(&n)
	return n, mapErr("pg: CountRuns", err)
}

func (s *Store) ListRunsForSHA(ctx context.Context, repoID int64, sha string) ([]*model.Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+runCols+` FROM runs WHERE repo_id = $1 AND head_sha = $2 ORDER BY created_at DESC, id DESC`,
		repoID, sha)
	if err != nil {
		return nil, mapErr("pg: ListRunsForSHA", err)
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, mapErr("pg: ListRunsForSHA", err)
		}
		out = append(out, r)
	}
	return out, mapErr("pg: ListRunsForSHA", rows.Err())
}

// NextRunNumber allocates the next per-(repo, workflow) number. The upsert
// serializes concurrent allocations on the row, so no two runs share a number.
func (s *Store) NextRunNumber(ctx context.Context, repoID int64, workflowPath string) (int64, error) {
	const q = `
INSERT INTO run_numbers (repo_id, workflow_path, current) VALUES ($1, $2, 1)
ON CONFLICT (repo_id, workflow_path) DO UPDATE SET current = run_numbers.current + 1
RETURNING current`
	var n int64
	err := s.pool.QueryRow(ctx, q, repoID, workflowPath).Scan(&n)
	return n, mapErr("pg: NextRunNumber", err)
}

// validCancel rejects a cancellation with no explanation before it reaches the
// database's own CHECK, so the caller gets the model's error message.
func validCancel(c *model.CancelReason) error {
	if c == nil {
		return nil
	}
	return c.Validate()
}
