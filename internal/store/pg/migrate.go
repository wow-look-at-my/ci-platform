package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/migrations"
)

// migrateLockKey serializes Migrate across control-plane replicas. Two replicas
// starting together must not both try to create the same table.
const migrateLockKey int64 = 0x63695f706c6174 // "ci_plat"

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int
	name     string
	body     string
	checksum string
}

// loadMigrations reads the embedded files in version order and rejects a
// malformed or duplicated filename rather than silently skipping it.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: read dir: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations: %q does not match NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migrations: %q: %w", e.Name(), err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations: version %04d used by both %q and %q", v, prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  v,
			name:     m[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("migrations: none embedded")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Migrate applies pending migrations in version order, each in its own
// transaction, and refuses to run if an already-applied file's checksum
// changed. Editing an applied migration means the schema on disk and the schema
// in the database have silently diverged; stopping is the only honest answer.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrateFS(ctx, migrations.FS)
}

func (s *Store) migrateFS(ctx context.Context, fsys fs.FS) error {
	pending, err := loadMigrations(fsys)
	if err != nil {
		return err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	const createTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    int PRIMARY KEY,
    name       text NOT NULL,
    checksum   text NOT NULL,
    applied_at timestamptz NOT NULL
)`
	if _, err := conn.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied := map[int]struct {
		name     string
		checksum string
	}{}
	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var name, sum string
		if err := rows.Scan(&v, &name, &sum); err != nil {
			rows.Close()
			return fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[v] = struct {
			name     string
			checksum string
		}{name, sum}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate: read schema_migrations: %w", err)
	}

	for _, m := range pending {
		if a, ok := applied[m.version]; ok {
			if a.checksum != m.checksum {
				return fmt.Errorf(
					"migrate: %04d_%s.sql changed after it was applied (recorded %s, on disk %s): "+
						"the database and the migration files have diverged; add a new migration instead",
					m.version, m.name, a.checksum[:12], m.checksum[:12])
			}
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return err
		}
		slog.Info("applied migration", "version", m.version, "name", m.name)
	}
	return nil
}

// applyOne runs one migration and records it in the same transaction, so a
// half-applied file can never be recorded as applied.
func applyOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate %04d_%s: begin: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.body); err != nil {
		return fmt.Errorf("migrate %04d_%s: %w", m.version, m.name, err)
	}
	const ins = `INSERT INTO schema_migrations (version, name, checksum, applied_at)
	             VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, ins, m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		return fmt.Errorf("migrate %04d_%s: record: %w", m.version, m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate %04d_%s: commit: %w", m.version, m.name, err)
	}
	return nil
}
