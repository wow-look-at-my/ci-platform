package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// schemaSQL is the whole schema, current truth in one file.
//
// There is deliberately no numbered migration chain. Nothing is deployed yet,
// so a forward-only sequence with a version table would be machinery for a
// situation that does not exist -- and it would make the schema something you
// reconstruct by replaying files instead of something you read. The first
// deployed instance is what earns a migration chain; until then, changing this
// file and recreating the database is the whole procedure.
//
//go:embed schema.sql
var schemaSQL string

// schemaFingerprint identifies the schema this build expects.
func schemaFingerprint() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}

// Migrate creates the schema on an empty database, and on a populated one
// checks that it is the schema this build expects.
//
// A mismatch is a hard stop, not a repair attempt: the database in front of us
// was built by a different version of this file, and guessing at the difference
// is how a schema and its code drift apart silently.
func (s *Store) Migrate(ctx context.Context) error {
	const createMeta = `
CREATE TABLE IF NOT EXISTS schema_meta (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    fingerprint TEXT NOT NULL,
    applied_at  TEXT NOT NULL
)`
	if _, err := s.db.ExecContext(ctx, createMeta); err != nil {
		return fmt.Errorf("sqlite: create schema_meta: %w", err)
	}

	want := schemaFingerprint()
	var have string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint FROM schema_meta WHERE id = 1`).Scan(&have)
	switch {
	case err == nil && have == want:
		return nil
	case err == nil:
		return fmt.Errorf(
			"sqlite: this database was created from a different schema (recorded %s, this build expects %s): "+
				"the schema file and the database have diverged; point CIPLATFORM_DATABASE_URL at a new file, "+
				"or migrate the existing one by hand",
			have[:12], want[:12])
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("sqlite: read schema_meta: %w", err)
	}

	// Empty database: apply the schema and record what was applied, in one
	// transaction, so a half-created schema can never be recorded as complete.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	const ins = `INSERT INTO schema_meta (id, fingerprint, applied_at) VALUES (1, ?, ?)`
	if _, err := tx.ExecContext(ctx, ins, want, ts(time.Now())); err != nil {
		return fmt.Errorf("sqlite: record schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit schema: %w", err)
	}
	slog.Info("applied schema", "fingerprint", want[:12])
	return nil
}
