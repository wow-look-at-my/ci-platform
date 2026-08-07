package storetest_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/sqlite"
	"github.com/wow-look-at-my/ci-platform/internal/store/storetest"
)

// newSQLiteStore gives each test its own database file. There is nothing to
// skip on and nothing to provision: the production store now runs anywhere the
// tests do, so "nobody ran these" can no longer read as a green build.
func newSQLiteStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()

	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ciplatform.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestSQLiteStore(t *testing.T) {
	storetest.RunSuite(t, func(t *testing.T) store.Store { return newSQLiteStore(t) })
}

func TestSQLiteStoreIsDurable(t *testing.T) {
	require.True(t, newSQLiteStore(t).Durable())
}

// A database survives the process that made it: reopening finds the schema
// already applied and the rows still there.
func TestSQLiteStoreSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ciplatform.db")

	first, err := sqlite.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, first.Migrate(ctx))
	require.NoError(t, first.UpsertRepo(ctx, &model.Repo{ID: 7, Owner: "acme", Name: "widget"}))
	require.NoError(t, first.Close())

	second, err := sqlite.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, second.Migrate(ctx), "reopening an applied schema is a no-op, not a re-apply")

	got, err := second.GetRepo(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, "widget", got.Name)
}

// TestSQLiteSchemaDivergence proves the store stops rather than letting the
// schema file and the database in front of it drift apart silently.
func TestSQLiteSchemaDivergence(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t).(*sqlite.Store)

	// Rewrite the recorded fingerprint to stand in for an edited schema file.
	_, err := s.DB().ExecContext(ctx,
		`UPDATE schema_meta SET fingerprint = 'edited-after-it-was-applied' WHERE id = 1`)
	require.NoError(t, err)

	err = s.Migrate(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "diverged")
}
