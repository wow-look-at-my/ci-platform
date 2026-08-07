package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const cacheCols = `id, repo_id, key, version, ref, size_bytes, storage_key, created_at,
	last_accessed, finalized`

func scanCache(row scanner) (*model.CacheEntry, error) {
	var e model.CacheEntry
	var createdAt, lastAccessed string
	if err := row.Scan(&e.ID, &e.RepoID, &e.Key, &e.Version, &e.Ref, &e.SizeBytes,
		&e.StorageKey, &createdAt, &lastAccessed, &e.Finalized); err != nil {
		return nil, err
	}
	var err error
	if e.CreatedAt, err = mustTime(createdAt); err != nil {
		return nil, err
	}
	if e.LastAccessed, err = mustTime(lastAccessed); err != nil {
		return nil, err
	}
	return &e, nil
}

// ReserveCache records an entry before its bytes are uploaded. It is not
// eligible for a restore until FinalizeCache runs.
func (s *Store) ReserveCache(ctx context.Context, e *model.CacheEntry) error {
	if e == nil {
		return fmt.Errorf("sqlite: ReserveCache: nil entry")
	}
	if e.ID != 0 {
		return fmt.Errorf("sqlite: ReserveCache: id %d already set; the store allocates ids", e.ID)
	}
	if e.Key == "" {
		return fmt.Errorf("sqlite: ReserveCache: entry for repo %d has no key", e.RepoID)
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
VALUES (?,?,?,?,?,?,?,?,?) RETURNING id`
	err := s.db.QueryRowContext(ctx, q, e.RepoID, e.Key, e.Version, e.Ref, e.SizeBytes, e.StorageKey,
		ts(e.CreatedAt), ts(e.LastAccessed), boolInt(e.Finalized)).Scan(&e.ID)
	return mapErr("sqlite: ReserveCache", err)
}

func (s *Store) FinalizeCache(ctx context.Context, id int64, size int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE cache_entries SET size_bytes = ?, finalized = 1 WHERE id = ?`, size, id)
	if err != nil {
		return mapErr("sqlite: FinalizeCache", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: FinalizeCache", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetCache(ctx context.Context, id int64) (*model.CacheEntry, error) {
	e, err := scanCache(s.db.QueryRowContext(ctx, `SELECT `+cacheCols+` FROM cache_entries WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr("sqlite: GetCache", err)
	}
	return e, nil
}

// ListCacheEntries returns a repository's finalized entries, newest first.
func (s *Store) ListCacheEntries(ctx context.Context, repoID int64) ([]*model.CacheEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cacheCols+` FROM cache_entries WHERE repo_id = ? AND finalized = 1
ORDER BY created_at DESC, id DESC`, repoID)
	if err != nil {
		return nil, mapErr("sqlite: ListCacheEntries", err)
	}
	defer rows.Close()
	var out []*model.CacheEntry
	for rows.Next() {
		e, err := scanCache(rows)
		if err != nil {
			return nil, mapErr("sqlite: ListCacheEntries", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr("sqlite: ListCacheEntries", err)
	}
	return out, nil
}

func (s *Store) TouchCache(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE cache_entries SET last_accessed = ? WHERE id = ?`, ts(at), id)
	if err != nil {
		return mapErr("sqlite: TouchCache", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: TouchCache", err)
	}
	if n == 0 {
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
WHERE repo_id = ? AND version = ? AND finalized = 1 AND key = ?
ORDER BY created_at DESC, id DESC LIMIT 1`
	e, err := scanCache(s.db.QueryRowContext(ctx, exact, repoID, version, key))
	if err == nil {
		return e, key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", mapErr("sqlite: LookupCache", err)
	}

	const prefix = `
SELECT ` + cacheCols + ` FROM cache_entries
WHERE repo_id = ? AND version = ? AND finalized = 1 AND key LIKE ? ESCAPE '\'
ORDER BY created_at DESC, id DESC LIMIT 1`
	for _, rk := range restoreKeys {
		if rk == "" {
			continue
		}
		e, err := scanCache(s.db.QueryRowContext(ctx, prefix, repoID, version, likePrefix(rk)))
		if err == nil {
			return e, rk, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, "", mapErr("sqlite: LookupCache", err)
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *Store) RecordCacheEvent(ctx context.Context, e model.CacheEvent) error {
	switch e.Kind {
	case "hit", "miss", "store", "evict":
	default:
		return fmt.Errorf("sqlite: RecordCacheEvent: unknown kind %q", e.Kind)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	const q = `INSERT INTO cache_events (repo_id, key, kind, matched_on, reason, size_bytes, at)
	           VALUES (?,?,?,?,?,?,?)`
	_, err := s.db.ExecContext(ctx, q, e.RepoID, e.Key, e.Kind, e.MatchedOn, e.Reason,
		e.SizeBytes, ts(e.At))
	return mapErr("sqlite: RecordCacheEvent", err)
}

func (s *Store) ListCacheEvents(ctx context.Context, repoID int64, limit int) ([]model.CacheEvent, error) {
	q := `SELECT id, repo_id, key, kind, matched_on, reason, size_bytes, at FROM cache_events
	      WHERE repo_id = ? ORDER BY at DESC, id DESC`
	args := []any{repoID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr("sqlite: ListCacheEvents", err)
	}
	defer rows.Close()
	var out []model.CacheEvent
	for rows.Next() {
		var e model.CacheEvent
		var at string
		if err := rows.Scan(&e.ID, &e.RepoID, &e.Key, &e.Kind, &e.MatchedOn, &e.Reason,
			&e.SizeBytes, &at); err != nil {
			return nil, mapErr("sqlite: ListCacheEvents", err)
		}
		if e.At, err = mustTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapErr("sqlite: ListCacheEvents", rows.Err())
}

// CacheUsage sums the finalized entries. A reserved-but-unfinalized entry has
// no known size, so counting it would be a guess.
func (s *Store) CacheUsage(ctx context.Context, repoID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(size_bytes), 0) FROM cache_entries WHERE repo_id = ? AND finalized = 1`,
		repoID).Scan(&n)
	return n, mapErr("sqlite: CacheUsage", err)
}

// EvictCaches drops least-recently-accessed entries until the repo is under
// quota and returns exactly what it removed. Every eviction is also written to
// cache_events and logged: a cache that silently drops entries is
// indistinguishable from one that is merely slow.
func (s *Store) EvictCaches(ctx context.Context, repoID int64, quotaBytes int64, now time.Time) ([]*model.CacheEntry, error) {
	if quotaBytes < 0 {
		return nil, fmt.Errorf("sqlite: EvictCaches: negative quota %d for repo %d", quotaBytes, repoID)
	}
	var evicted []*model.CacheEntry
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Order is the eviction order itself: least recently accessed first.
		const sel = `
SELECT ` + cacheCols + ` FROM cache_entries
WHERE repo_id = ? AND finalized = 1
ORDER BY last_accessed ASC, id ASC`
		rows, err := tx.QueryContext(ctx, sel, repoID)
		if err != nil {
			return mapErr("sqlite: EvictCaches", err)
		}
		var entries []*model.CacheEntry
		var total int64
		for rows.Next() {
			e, err := scanCache(rows)
			if err != nil {
				rows.Close()
				return mapErr("sqlite: EvictCaches", err)
			}
			entries = append(entries, e)
			total += e.SizeBytes
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr("sqlite: EvictCaches", err)
		}

		for _, e := range entries {
			if total <= quotaBytes {
				break
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries WHERE id = ?`, e.ID); err != nil {
				return mapErr("sqlite: EvictCaches", err)
			}
			total -= e.SizeBytes
			evicted = append(evicted, e)
			const ev = `INSERT INTO cache_events (repo_id, key, kind, matched_on, reason, size_bytes, at)
			            VALUES (?,?,'evict','',?,?,?)`
			reason := fmt.Sprintf("evicted to stay under the %d byte cache quota; "+
				"it was the least recently used entry, last read %s",
				quotaBytes, e.LastAccessed.Format(time.RFC3339))
			if _, err := tx.ExecContext(ctx, ev, repoID, e.Key, reason, e.SizeBytes, ts(now)); err != nil {
				return mapErr("sqlite: EvictCaches", err)
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
