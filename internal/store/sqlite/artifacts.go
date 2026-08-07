package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const artifactCols = `id, run_id, job_id, name, size_bytes, digest, storage_key, expires_at,
	created_at, finalized, finalized_at`

func scanArtifact(row scanner) (*model.Artifact, error) {
	var a model.Artifact
	var expiresAt, createdAt string
	var finalizedAt sql.NullString
	if err := row.Scan(&a.ID, &a.RunID, &a.JobID, &a.Name, &a.SizeBytes, &a.Digest,
		&a.StorageKey, &expiresAt, &createdAt, &a.Finalized, &finalizedAt); err != nil {
		return nil, err
	}
	var err error
	if a.ExpiresAt, err = mustTime(expiresAt); err != nil {
		return nil, err
	}
	if a.CreatedAt, err = mustTime(createdAt); err != nil {
		return nil, err
	}
	if a.FinalizedAt, err = nullTime(finalizedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) CreateArtifact(ctx context.Context, a *model.Artifact) error {
	if a == nil {
		return fmt.Errorf("sqlite: CreateArtifact: nil artifact")
	}
	if a.ID != 0 {
		return fmt.Errorf("sqlite: CreateArtifact: id %d already set; the store allocates ids", a.ID)
	}
	if a.Name == "" {
		return fmt.Errorf("sqlite: CreateArtifact: artifact for run %d has no name", a.RunID)
	}
	const q = `
INSERT INTO artifacts (run_id, job_id, name, size_bytes, digest, storage_key, expires_at,
	created_at, finalized, finalized_at)
VALUES (?,?,?,?,?,?,?,?,?,?) RETURNING id`
	err := s.db.QueryRowContext(ctx, q, a.RunID, a.JobID, a.Name, a.SizeBytes, a.Digest, a.StorageKey,
		ts(a.ExpiresAt), ts(a.CreatedAt), boolInt(a.Finalized), tsp(a.FinalizedAt)).Scan(&a.ID)
	return mapErr("sqlite: CreateArtifact", err)
}

// FinalizeArtifact records the uploaded size and digest. Until it runs the
// artifact is not downloadable, because its bytes are not known to be complete.
func (s *Store) FinalizeArtifact(ctx context.Context, id int64, size int64, digest string) error {
	const q = `UPDATE artifacts SET size_bytes=?, digest=?, finalized=1, finalized_at=?
	           WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, size, digest, ts(time.Now()), id)
	if err != nil {
		return mapErr("sqlite: FinalizeArtifact", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: FinalizeArtifact", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetArtifact(ctx context.Context, id int64) (*model.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr("sqlite: GetArtifact", err)
	}
	return a, nil
}

func (s *Store) FindArtifact(ctx context.Context, runID int64, name string) (*model.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx,
		`SELECT `+artifactCols+` FROM artifacts WHERE run_id = ? AND name = ?`, runID, name))
	if err != nil {
		return nil, mapErr("sqlite: FindArtifact", err)
	}
	return a, nil
}

func (s *Store) ListArtifacts(ctx context.Context, runID int64) ([]*model.Artifact, error) {
	return s.queryArtifacts(ctx, "sqlite: ListArtifacts",
		`SELECT `+artifactCols+` FROM artifacts WHERE run_id = ? ORDER BY id`, runID)
}

// DeleteExpiredArtifacts removes expired metadata and returns it so the caller
// can delete the corresponding blobs and log what went.
func (s *Store) DeleteExpiredArtifacts(ctx context.Context, now time.Time) ([]*model.Artifact, error) {
	out, err := s.queryArtifacts(ctx, "sqlite: DeleteExpiredArtifacts",
		`DELETE FROM artifacts WHERE expires_at <= ? RETURNING `+artifactCols, ts(now))
	if err != nil {
		return nil, err
	}
	if len(out) > 0 {
		var bytes int64
		for _, a := range out {
			bytes += a.SizeBytes
		}
		slog.Info("deleted expired artifacts", "count", len(out), "bytes", bytes)
	}
	return out, nil
}

func (s *Store) queryArtifacts(ctx context.Context, op, q string, args ...any) ([]*model.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(op, err)
	}
	defer rows.Close()
	var out []*model.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, mapErr(op, err)
		}
		out = append(out, a)
	}
	return out, mapErr(op, rows.Err())
}

// ArtifactUsage totals a repository's finalized artifact bytes.
func (s *Store) ArtifactUsage(ctx context.Context, repoID int64) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.size_bytes), 0) FROM artifacts a
JOIN runs r ON r.id = a.run_id
WHERE r.repo_id = ? AND a.finalized = 1`, repoID).Scan(&total)
	if err != nil {
		return 0, mapErr("sqlite: ArtifactUsage", err)
	}
	return total, nil
}
