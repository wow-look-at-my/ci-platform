package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const cacheCols = `id, repo_id, key, version, ref, size_bytes, storage_key, created_at,
	last_accessed, finalized`

func scanCache(row pgx.Row) (*model.CacheEntry, error) {
	var e model.CacheEntry
	if err := row.Scan(&e.ID, &e.RepoID, &e.Key, &e.Version, &e.Ref, &e.SizeBytes,
		&e.StorageKey, &e.CreatedAt, &e.LastAccessed, &e.Finalized); err != nil {
		return nil, err
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.LastAccessed = e.LastAccessed.UTC()
	return &e, nil
}

// ReserveCache records an entry before its bytes are uploaded. It is not
// eligible for a restore until FinalizeCache runs.
func (s *Store) ReserveCache(ctx context.Context, e *model.CacheEntry) error {
	if e == nil {
		return fmt.Errorf("pg: ReserveCache: nil entry")
	}
	if e.ID != 0 {
		return fmt.Errorf("pg: ReserveCache: id %d already set; the store allocates ids", e.ID)
	}
	if e.Key == "" {
		return fmt.Errorf("pg: ReserveCache: entry for repo %d has no key", e.RepoID)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.LastAccessed.IsZero() {
		e.LastAccessed = e.CreatedAt
	}
	const q = `
INSERT INTO cache_entries (repo_id, key, version, ref, size_bytes, storage_key, created_at,
	last_accessed, finalized)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`
	err := s.pool.QueryRow(ctx, q, e.RepoID, e.Key, e.Version, e.Ref, e.SizeBytes, e.StorageKey,
		utc(e.CreatedAt), utc(e.LastAccessed), e.Finalized).Scan(&e.ID)
	return mapErr("pg: ReserveCache", err)
}

func (s *Store) FinalizeCache(ctx context.Context, id int64, size int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cache_entries SET size_bytes = $2, finalized = true WHERE id = $1`, id, size)
	if err != nil {
		return mapErr("pg: FinalizeCache", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetCache(ctx context.Context, id int64) (*model.CacheEntry, error) {
	e, err := scanCache(s.pool.QueryRow(ctx, `SELECT `+cacheCols+` FROM cache_entries WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr("pg: GetCache", err)
	}
	return e, nil
}

func (s *Store) TouchCache(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cache_entries SET last_accessed = $2 WHERE id = $1`, id, utc(at))
	if err != nil {
		return mapErr("pg: TouchCache", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// likePrefix escapes the LIKE metacharacters so a restore key containing % or _
// matches literally rather than as a wildcard.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}

// LookupCache implements restore-keys semantics: the exact key first, then each
// restore key as a prefix in declaration order, newest created_at winning
// within a key. matchedOn names the key that hit.
//
// ref is recorded on the entry and returned, not filtered on: which refs may
// restore from which is the caller's policy, and silently narrowing here would
// turn a hit into an unexplained miss.
func (s *Store) LookupCache(ctx context.Context, repoID int64, key string, restoreKeys []string, version, ref string) (*model.CacheEntry, string, error) {
	const exact = `
SELECT ` + cacheCols + ` FROM cache_entries
WHERE repo_id = $1 AND version = $2 AND finalized AND key = $3
ORDER BY created_at DESC, id DESC LIMIT 1`
	e, err := scanCache(s.pool.QueryRow(ctx, exact, repoID, version, key))
	if err == nil {
		return e, key, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", mapErr("pg: LookupCache", err)
	}

	const prefix = `
SELECT ` + cacheCols + ` FROM cache_entries
WHERE repo_id = $1 AND version = $2 AND finalized AND key LIKE $3 ESCAPE '\'
ORDER BY created_at DESC, id DESC LIMIT 1`
	for _, rk := range restoreKeys {
		if rk == "" {
			continue
		}
		e, err := scanCache(s.pool.QueryRow(ctx, prefix, repoID, version, likePrefix(rk)))
		if err == nil {
			return e, rk, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", mapErr("pg: LookupCache", err)
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *Store) RecordCacheEvent(ctx context.Context, e model.CacheEvent) error {
	switch e.Kind {
	case "hit", "miss", "store", "evict":
	default:
		return fmt.Errorf("pg: RecordCacheEvent: unknown kind %q", e.Kind)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	const q = `INSERT INTO cache_events (repo_id, key, kind, matched_on, reason, size_bytes, at)
	           VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`
	var id int64
	err := s.pool.QueryRow(ctx, q, e.RepoID, e.Key, e.Kind, e.MatchedOn, e.Reason,
		e.SizeBytes, utc(e.At)).Scan(&id)
	return mapErr("pg: RecordCacheEvent", err)
}

func (s *Store) ListCacheEvents(ctx context.Context, repoID int64, limit int) ([]model.CacheEvent, error) {
	q := `SELECT id, repo_id, key, kind, matched_on, reason, size_bytes, at FROM cache_events
	      WHERE repo_id = $1 ORDER BY at DESC, id DESC`
	args := []any{repoID}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr("pg: ListCacheEvents", err)
	}
	defer rows.Close()
	var out []model.CacheEvent
	for rows.Next() {
		var e model.CacheEvent
		if err := rows.Scan(&e.ID, &e.RepoID, &e.Key, &e.Kind, &e.MatchedOn, &e.Reason,
			&e.SizeBytes, &e.At); err != nil {
			return nil, mapErr("pg: ListCacheEvents", err)
		}
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, mapErr("pg: ListCacheEvents", rows.Err())
}

// CacheUsage sums the finalized entries. A reserved-but-unfinalized entry has
// no known size, so counting it would be a guess.
func (s *Store) CacheUsage(ctx context.Context, repoID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(size_bytes), 0) FROM cache_entries WHERE repo_id = $1 AND finalized`,
		repoID).Scan(&n)
	return n, mapErr("pg: CacheUsage", err)
}

// EvictCaches drops least-recently-accessed entries until the repo is under
// quota and returns exactly what it removed. Every eviction is also written to
// cache_events and logged: a cache that silently drops entries is
// indistinguishable from one that is merely slow.
func (s *Store) EvictCaches(ctx context.Context, repoID int64, quotaBytes int64, now time.Time) ([]*model.CacheEntry, error) {
	if quotaBytes < 0 {
		return nil, fmt.Errorf("pg: EvictCaches: negative quota %d for repo %d", quotaBytes, repoID)
	}
	var evicted []*model.CacheEntry
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// Order is the eviction order itself: least recently accessed first.
		const sel = `
SELECT ` + cacheCols + ` FROM cache_entries
WHERE repo_id = $1 AND finalized
ORDER BY last_accessed ASC, id ASC
FOR UPDATE`
		rows, err := tx.Query(ctx, sel, repoID)
		if err != nil {
			return mapErr("pg: EvictCaches", err)
		}
		var entries []*model.CacheEntry
		var total int64
		for rows.Next() {
			e, err := scanCache(rows)
			if err != nil {
				rows.Close()
				return mapErr("pg: EvictCaches", err)
			}
			entries = append(entries, e)
			total += e.SizeBytes
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr("pg: EvictCaches", err)
		}

		for _, e := range entries {
			if total <= quotaBytes {
				break
			}
			if _, err := tx.Exec(ctx, `DELETE FROM cache_entries WHERE id = $1`, e.ID); err != nil {
				return mapErr("pg: EvictCaches", err)
			}
			total -= e.SizeBytes
			evicted = append(evicted, e)
			const ev = `INSERT INTO cache_events (repo_id, key, kind, matched_on, reason, size_bytes, at)
			            VALUES ($1,$2,'evict','',$3,$4,$5)`
			reason := fmt.Sprintf("evicted to stay under the %d byte cache quota; "+
				"it was the least recently used entry, last read %s",
				quotaBytes, e.LastAccessed.Format(time.RFC3339))
			if _, err := tx.Exec(ctx, ev, repoID, e.Key, reason, e.SizeBytes, utc(now)); err != nil {
				return mapErr("pg: EvictCaches", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, e := range evicted {
		slog.Warn("evicted cache entry",
			"repo_id", repoID, "key", e.Key, "version", e.Version,
			"size_bytes", e.SizeBytes, "last_accessed", e.LastAccessed, "quota_bytes", quotaBytes)
	}
	return evicted, nil
}
