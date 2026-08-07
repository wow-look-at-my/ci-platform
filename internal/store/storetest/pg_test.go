package storetest_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/pg"
	"github.com/wow-look-at-my/ci-platform/internal/store/storetest"
)

// dsnEnv names the variable that points the Postgres tests at a live database.
// The tests skip when it is unset rather than inventing a default: a hard-coded
// localhost DSN would turn "nobody ran these" into a green build.
const dsnEnv = "CIPLATFORM_TEST_DATABASE_URL"

const skipMsg = "set " + dsnEnv + " to run the Postgres store tests, e.g.\n" +
	"  CIPLATFORM_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/ciplatform?sslmode=disable' \\\n" +
	"    go test ./internal/store/..."

var schemaSeq atomic.Int64

// schemaDSN points a connection at a private schema, so every test gets a
// clean database without dropping anyone else's tables.
func schemaDSN(t *testing.T, base, schema string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err, "%s must be a URL-form DSN", dsnEnv)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// newPGStore creates a fresh schema, migrates into it, and drops it afterwards.
func newPGStore(t *testing.T, base string) store.Store {
	t.Helper()
	ctx := context.Background()

	schema := fmt.Sprintf("storetest_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
	admin, err := pg.Open(ctx, base)
	require.NoError(t, err)
	_, err = admin.Pool().Exec(ctx, `CREATE SCHEMA `+pgIdent(schema))
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	s, err := pg.Open(ctx, schemaDSN(t, base, schema))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))

	t.Cleanup(func() {
		require.NoError(t, s.Close())
		cleanup, err := pg.Open(context.Background(), base)
		require.NoError(t, err)
		defer func() { require.NoError(t, cleanup.Close()) }()
		_, err = cleanup.Pool().Exec(context.Background(), `DROP SCHEMA `+pgIdent(schema)+` CASCADE`)
		require.NoError(t, err)
	})
	return s
}

// pgIdent quotes a generated identifier. The schema names here are built from a
// timestamp and a counter, but quoting keeps the DDL correct by construction.
func pgIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func TestPGStore(t *testing.T) {
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skip(skipMsg)
	}
	storetest.RunSuite(t, func(t *testing.T) store.Store { return newPGStore(t, base) })
}

func TestPGStoreIsDurable(t *testing.T) {
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skip(skipMsg)
	}
	s := newPGStore(t, base)
	require.True(t, s.Durable())
}

// TestPGMigrationChecksumDivergence proves the runner stops rather than letting
// the files on disk and the schema in the database drift apart silently.
func TestPGMigrationChecksumDivergence(t *testing.T) {
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skip(skipMsg)
	}
	ctx := context.Background()
	s := newPGStore(t, base).(*pg.Store)

	// Rewrite a recorded checksum to stand in for an edited migration file.
	_, err := s.Pool().Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'edited-after-it-was-applied' WHERE version = 1`)
	require.NoError(t, err)

	err = s.Migrate(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed after it was applied")
	require.Contains(t, err.Error(), "diverged")
}
