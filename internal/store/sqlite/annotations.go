package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func validAnnotationLevel(l model.AnnotationLevel) bool {
	switch l {
	case model.AnnotationNotice, model.AnnotationWarning, model.AnnotationFailure:
		return true
	}
	return false
}

// AddAnnotations appends diagnostics for a job in one batch. Every level is
// checked before anything is written, so a bad one cannot leave half a batch
// behind.
func (s *Store) AddAnnotations(ctx context.Context, jobID int64, as []model.Annotation) error {
	if len(as) == 0 {
		return nil
	}
	for i := range as {
		if !validAnnotationLevel(as[i].Level) {
			return fmt.Errorf("sqlite: AddAnnotations: annotation %d has invalid level %q", i, as[i].Level)
		}
	}
	const ins = `
INSERT INTO annotations (job_id, path, start_line, end_line, start_column, end_column,
	level, message, title, raw_details)
VALUES (?,?,?,?,?,?,?,?,?,?)`
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, ins)
		if err != nil {
			return mapErr("sqlite: AddAnnotations", err)
		}
		defer stmt.Close()
		for _, a := range as {
			if _, err := stmt.ExecContext(ctx, jobID, a.Path, a.StartLine, a.EndLine,
				a.StartCol, a.EndCol, string(a.Level), a.Message, a.Title, a.RawDetail); err != nil {
				return mapErr("sqlite: AddAnnotations", err)
			}
		}
		return nil
	})
}

func (s *Store) ListAnnotations(ctx context.Context, jobID int64) ([]model.Annotation, error) {
	const q = `SELECT id, job_id, path, start_line, end_line, start_column, end_column,
	                  level, message, title, raw_details
	           FROM annotations WHERE job_id = ? ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, mapErr("sqlite: ListAnnotations", err)
	}
	defer rows.Close()
	var out []model.Annotation
	for rows.Next() {
		var a model.Annotation
		if err := rows.Scan(&a.ID, &a.JobID, &a.Path, &a.StartLine, &a.EndLine, &a.StartCol,
			&a.EndCol, &a.Level, &a.Message, &a.Title, &a.RawDetail); err != nil {
			return nil, mapErr("sqlite: ListAnnotations", err)
		}
		out = append(out, a)
	}
	return out, mapErr("sqlite: ListAnnotations", rows.Err())
}
