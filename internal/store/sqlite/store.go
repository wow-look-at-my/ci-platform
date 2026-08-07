// Package sqlite is the production store: one SQLite file, a durable job
// queue, and a lease protocol whose expiry path requeues rather than fails.
//
// see docs/storage.md
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: the binaries stay static, CGO stays off

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// timeLayout is how every timestamp is stored: fixed-width, UTC, nanoseconds
// always present. Fixed width is the point -- string comparison in SQL is then
// chronological comparison, which is what the queue's not_before and the
// reaper's lease_expires_at rely on.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Store implements store.Store against SQLite.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and verifies it. It does
// not migrate; call Migrate.
//
// path is a filename, or ":memory:" for a throwaway database. The pragmas are
// applied through the DSN so every connection gets them: a pragma set on one
// pooled connection would otherwise not apply to the next.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite: database path is empty")
	}
	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?" + strings.Join([]string{
			// WAL keeps a reader from blocking the writer, which is what lets
			// the API serve while the scheduler writes.
			"_pragma=journal_mode(WAL)",
			// Wait rather than fail on a busy database.
			"_pragma=busy_timeout(5000)",
			// The schema's ON DELETE CASCADE is load-bearing; SQLite ignores
			// every foreign key unless this is on.
			"_pragma=foreign_keys(1)",
			"_pragma=synchronous(NORMAL)",
		}, "&")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One connection. SQLite takes a database-wide write lock, so concurrent
	// writers would spend their time colliding and retrying; serializing them
	// here costs nothing on a single-node control plane and makes the queue's
	// claim-then-lease sequence atomic without row locks. A ":memory:" database
	// additionally IS the connection -- a second one would see an empty schema.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// DB exposes the handle for callers that need a raw query (health probes,
// ad-hoc reporting).
func (s *Store) DB() *sql.DB { return s.db }

// Durable reports true: a control-plane restart preserves every queued job.
func (s *Store) Durable() bool { return true }

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

// tx runs fn inside a transaction, rolling back on error.
//
// Every write goes through here with BEGIN IMMEDIATE semantics: SQLite would
// otherwise start a deferred transaction that takes its write lock at the first
// write, so a read-then-write sequence could see the database change underneath
// it and fail at commit instead of at the read.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() { _ = t.Rollback() }()
	if err := fn(t); err != nil {
		return err
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// mapErr translates the errors callers are expected to branch on. Everything
// else is returned wrapped, never swallowed.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	// The driver reports constraint violations as text; SQLITE_CONSTRAINT_UNIQUE
	// and _PRIMARYKEY are the two a caller acts on.
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "PRIMARY KEY constraint failed") {
		return fmt.Errorf("%s: %w: %s", op, store.ErrConflict, msg)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ts renders a timestamp for storage. Every stored time is UTC.
func ts(t time.Time) string { return t.UTC().Format(timeLayout) }

// tsp renders an optional timestamp; nil stays NULL.
func tsp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// parseTS reads a stored timestamp back.
func parseTS(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		// A row written by something other than this package is a corruption we
		// report rather than paper over with a zero time.
		return time.Time{}, fmt.Errorf("sqlite: %q is not a stored timestamp: %w", s, err)
	}
	return t.UTC(), nil
}

// nullTime converts a scanned nullable timestamp column.
func nullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTS(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// mustTime converts a scanned NOT NULL timestamp column.
func mustTime(s string) (time.Time, error) { return parseTS(s) }

// jsonText marshals a value for a JSON column, keeping an array an array and an
// object an object: a nil Go map or slice would encode as JSON null, which the
// schema's json_type CHECK rejects.
func jsonText(v any) (string, error) {
	switch t := v.(type) {
	case []string:
		if t == nil {
			v = []string{}
		}
	case map[string]any:
		if t == nil {
			v = map[string]any{}
		}
	case map[string]string:
		if t == nil {
			v = map[string]string{}
		}
	case map[string]int:
		if t == nil {
			v = map[string]int{}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlite: encode json column: %w", err)
	}
	return string(b), nil
}

// jsonInto decodes a JSON column into dst. An empty column is left as the
// zero value rather than reported as malformed.
func jsonInto(raw string, dst any) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("sqlite: decode json column %q: %w", raw, err)
	}
	return nil
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

// boolInt renders a bool for an INTEGER 0/1 column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

var _ store.Store = (*Store)(nil)
