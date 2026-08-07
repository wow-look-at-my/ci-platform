package storetest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/store/mem"
	"github.com/wow-look-at-my/ci-platform/internal/store/storetest"
)

func TestMemStore(t *testing.T) {
	storetest.RunSuite(t, func(t *testing.T) store.Store {
		s := mem.New()
		require.NoError(t, s.Migrate(context.Background()))
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		return s
	})
}

func TestMemStoreIsNotDurable(t *testing.T) {
	s := mem.New()
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.False(t, s.Durable(),
		"the in-memory store must never claim durability: a restart loses every queued job")
}

func TestMemOpen(t *testing.T) {
	s, err := mem.Open(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.False(t, s.Durable())
}
