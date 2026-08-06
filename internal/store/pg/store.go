// Package pg is the production store: Postgres, a durable job queue, and a
// lease protocol whose expiry path requeues rather than fails.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Store implements store.Store against Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and verifies the connection. It does not migrate; call Migrate.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool for callers that need a raw connection
// (health probes, ad-hoc reporting queries).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Durable reports true: a control-plane restart preserves every queued job.
func (s *Store) Durable() bool { return true }

// Close releases the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// tx runs fn inside a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	t, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: begin: %w", err)
	}
	defer func() { _ = t.Rollback(ctx) }()
	if err := fn(t); err != nil {
		return err
	}
	if err := t.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit: %w", err)
	}
	return nil
}

// mapErr translates the errors callers are expected to branch on. Everything
// else is returned wrapped, never swallowed.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%s: %w: %s", op, store.ErrConflict, pgErr.ConstraintName)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// utc normalizes a timestamp for storage. Every stored time is UTC.
func utc(t time.Time) time.Time { return t.UTC() }

// utcp normalizes an optional timestamp.
func utcp(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// nonNilStrings keeps a jsonb array column an array. A nil Go slice would
// encode as JSON null, which the schema's jsonb_typeof CHECK rejects.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nonNilAnyMap keeps a jsonb object column an object.
func nonNilAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// nonNilStringMap keeps a jsonb object column an object.
func nonNilStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// emptyToNil normalizes a decoded empty collection back to nil, so a value
// round-trips through either store identically.
func emptyToNil[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func emptyMapToNil[K comparable, V any](m map[K]V) map[K]V {
	if len(m) == 0 {
		return nil
	}
	return m
}

// cancelColumns flattens a CancelReason into its three columns. The schema
// enforces that an actor without a sentence cannot be stored.
func cancelColumns(c *model.CancelReason) (actor, sentence, triggeredBy string) {
	if c == nil {
		return "", "", ""
	}
	return string(c.Actor), c.Sentence, c.TriggeredBy
}

func cancelFrom(actor, sentence, triggeredBy string) *model.CancelReason {
	if actor == "" && sentence == "" {
		return nil
	}
	return &model.CancelReason{
		Actor:       model.CancelActor(actor),
		Sentence:    sentence,
		TriggeredBy: triggeredBy,
	}
}
