package pg

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const artifactCols = `id, run_id, job_id, name, size_bytes, digest, storage_key, expires_at,
	created_at, finalized, finalized_at`

func scanArtifact(row pgx.Row) (*model.Artifact, error) {
	var a model.Artifact
	if err := row.Scan(&a.ID, &a.RunID, &a.JobID, &a.Name, &a.SizeBytes, &a.Digest,
		&a.StorageKey, &a.ExpiresAt, &a.CreatedAt, &a.Finalized, &a.FinalizedAt); err != nil {
		return nil, err
	}
	a.ExpiresAt = a.ExpiresAt.UTC()
	a.CreatedAt = a.CreatedAt.UTC()
	a.FinalizedAt = utcp(a.FinalizedAt)
	return &a, nil
}

func (s *Store) CreateArtifact(ctx context.Context, a *model.Artifact) error {
	if a == nil {
		return fmt.Errorf("pg: CreateArtifact: nil artifact")
	}
	if a.ID != 0 {
		return fmt.Errorf("pg: CreateArtifact: id %d already set; the store allocates ids", a.ID)
	}
	if a.Name == "" {
		return fmt.Errorf("pg: CreateArtifact: artifact for run %d has no name", a.RunID)
	}
	const q = `
INSERT INTO artifacts (run_id, job_id, name, size_bytes, digest, storage_key, expires_at,
	created_at, finalized, finalized_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`
	err := s.pool.QueryRow(ctx, q, a.RunID, a.JobID, a.Name, a.SizeBytes, a.Digest, a.StorageKey,
		utc(a.ExpiresAt), utc(a.CreatedAt), a.Finalized, utcp(a.FinalizedAt)).Scan(&a.ID)
	return mapErr("pg: CreateArtifact", err)
}

// FinalizeArtifact records the uploaded size and digest. Until it runs the
// artifact is not downloadable, because its bytes are not known to be complete.
func (s *Store) FinalizeArtifact(ctx context.Context, id int64, size int64, digest string) error {
	const q = `UPDATE artifacts SET size_bytes=$2, digest=$3, finalized=true, finalized_at=$4
	           WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, size, digest, time.Now().UTC())
	if err != nil {
		return mapErr("pg: FinalizeArtifact", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetArtifact(ctx context.Context, id int64) (*model.Artifact, error) {
	a, err := scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr("pg: GetArtifact", err)
	}
	return a, nil
}

func (s *Store) FindArtifact(ctx context.Context, runID int64, name string) (*model.Artifact, error) {
	a, err := scanArtifact(s.pool.QueryRow(ctx,
		`SELECT `+artifactCols+` FROM artifacts WHERE run_id = $1 AND name = $2`, runID, name))
	if err != nil {
		return nil, mapErr("pg: FindArtifact", err)
	}
	return a, nil
}

func (s *Store) ListArtifacts(ctx context.Context, runID int64) ([]*model.Artifact, error) {
	return s.queryArtifacts(ctx, "pg: ListArtifacts",
		`SELECT `+artifactCols+` FROM artifacts WHERE run_id = $1 ORDER BY id`, runID)
}

// DeleteExpiredArtifacts removes expired metadata and returns it so the caller
// can delete the corresponding blobs and log what went.
func (s *Store) DeleteExpiredArtifacts(ctx context.Context, now time.Time) ([]*model.Artifact, error) {
	out, err := s.queryArtifacts(ctx, "pg: DeleteExpiredArtifacts",
		`DELETE FROM artifacts WHERE expires_at <= $1 RETURNING `+artifactCols, utc(now))
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
	rows, err := s.pool.Query(ctx, q, args...)
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
