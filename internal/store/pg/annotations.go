package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func validAnnotationLevel(l model.AnnotationLevel) bool {
	switch l {
	case model.AnnotationNotice, model.AnnotationWarning, model.AnnotationFailure:
		return true
	}
	return false
}

// AddAnnotations appends diagnostics for a job in one batch.
func (s *Store) AddAnnotations(ctx context.Context, jobID int64, as []model.Annotation) error {
	if len(as) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(as))
	for i := range as {
		a := as[i]
		if !validAnnotationLevel(a.Level) {
			return fmt.Errorf("pg: AddAnnotations: annotation %d has invalid level %q", i, a.Level)
		}
		rows = append(rows, []any{jobID, a.Path, a.StartLine, a.EndLine, a.StartCol, a.EndCol,
			string(a.Level), a.Message, a.Title, a.RawDetail})
	}
	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"annotations"},
		[]string{"job_id", "path", "start_line", "end_line", "start_column", "end_column",
			"level", "message", "title", "raw_details"},
		pgx.CopyFromRows(rows))
	return mapErr("pg: AddAnnotations", err)
}

func (s *Store) ListAnnotations(ctx context.Context, jobID int64) ([]model.Annotation, error) {
	const q = `SELECT id, job_id, path, start_line, end_line, start_column, end_column,
	                  level, message, title, raw_details
	           FROM annotations WHERE job_id = $1 ORDER BY id`
	rows, err := s.pool.Query(ctx, q, jobID)
	if err != nil {
		return nil, mapErr("pg: ListAnnotations", err)
	}
	defer rows.Close()
	var out []model.Annotation
	for rows.Next() {
		var a model.Annotation
		if err := rows.Scan(&a.ID, &a.JobID, &a.Path, &a.StartLine, &a.EndLine, &a.StartCol,
			&a.EndCol, &a.Level, &a.Message, &a.Title, &a.RawDetail); err != nil {
			return nil, mapErr("pg: ListAnnotations", err)
		}
		out = append(out, a)
	}
	return out, mapErr("pg: ListAnnotations", rows.Err())
}
