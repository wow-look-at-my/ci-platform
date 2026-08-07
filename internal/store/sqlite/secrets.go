package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Scope names. A secret is stored under exactly one of them, and resolution
// merges org then repo then environment, so the narrowest scope wins.
const (
	ScopeOrg         = "org"
	ScopeRepo        = "repo"
	ScopeEnvironment = "environment"
)

// ScopeKey builds the storage key for a scope. It is exported because callers
// must construct the same keys ResolveSecrets reads:
//
//	org         -> "owner"
//	repo        -> "owner/repo"
//	environment -> "owner/repo/environment"
func ScopeKey(scope, owner, repo, environment string) (string, error) {
	switch scope {
	case ScopeOrg:
		if owner == "" {
			return "", fmt.Errorf("scope %q needs an owner", scope)
		}
		return owner, nil
	case ScopeRepo:
		if owner == "" || repo == "" {
			return "", fmt.Errorf("scope %q needs an owner and a repo", scope)
		}
		return owner + "/" + repo, nil
	case ScopeEnvironment:
		if owner == "" || repo == "" || environment == "" {
			return "", fmt.Errorf("scope %q needs an owner, a repo and an environment", scope)
		}
		return owner + "/" + repo + "/" + environment, nil
	}
	return "", fmt.Errorf("unknown scope %q", scope)
}

func validScope(scope string) error {
	switch scope {
	case ScopeOrg, ScopeRepo, ScopeEnvironment:
		return nil
	}
	return fmt.Errorf("unknown scope %q, want one of %q, %q, %q",
		scope, ScopeOrg, ScopeRepo, ScopeEnvironment)
}

// resolutionOrder lists the (scope, key) pairs to merge, narrowest last.
// An empty environment contributes nothing rather than a bogus key.
func resolutionOrder(owner, repo, environment string) [][2]string {
	out := [][2]string{
		{ScopeOrg, owner},
		{ScopeRepo, owner + "/" + repo},
	}
	if environment != "" {
		out = append(out, [2]string{ScopeEnvironment, owner + "/" + repo + "/" + environment})
	}
	return out
}

func (s *Store) PutSecret(ctx context.Context, scope, scopeKey, name string, ciphertext []byte) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("sqlite: PutSecret: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("sqlite: PutSecret: scope %q needs a non-empty scope key and name", scope)
	}
	const q = `
INSERT INTO secrets (scope, scope_key, name, ciphertext, updated_at) VALUES (?,?,?,?,?)
ON CONFLICT (scope, scope_key, name) DO UPDATE SET
	ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q, scope, scopeKey, name, ciphertext, ts(time.Now()))
	return mapErr("sqlite: PutSecret", err)
}

// ResolveSecrets merges org, then repo, then environment. A name defined at a
// narrower scope replaces the wider one.
func (s *Store) ResolveSecrets(ctx context.Context, owner, repo, environment string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		rows, err := s.db.QueryContext(ctx,
			`SELECT name, ciphertext FROM secrets WHERE scope = ? AND scope_key = ?`, sk[0], sk[1])
		if err != nil {
			return nil, mapErr("sqlite: ResolveSecrets", err)
		}
		for rows.Next() {
			var name string
			var ct []byte
			if err := rows.Scan(&name, &ct); err != nil {
				rows.Close()
				return nil, mapErr("sqlite: ResolveSecrets", err)
			}
			out[name] = ct
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, mapErr("sqlite: ResolveSecrets", err)
		}
	}
	return out, nil
}

func (s *Store) DeleteSecret(ctx context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("sqlite: DeleteSecret: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM secrets WHERE scope = ? AND scope_key = ? AND name = ?`, scope, scopeKey, name)
	if err != nil {
		return mapErr("sqlite: DeleteSecret", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: DeleteSecret", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListSecretNames(ctx context.Context, scope, scopeKey string) ([]string, error) {
	if err := validScope(scope); err != nil {
		return nil, fmt.Errorf("sqlite: ListSecretNames: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM secrets WHERE scope = ? AND scope_key = ? ORDER BY name`, scope, scopeKey)
	if err != nil {
		return nil, mapErr("sqlite: ListSecretNames", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, mapErr("sqlite: ListSecretNames", err)
		}
		out = append(out, n)
	}
	return out, mapErr("sqlite: ListSecretNames", rows.Err())
}

func (s *Store) PutVar(ctx context.Context, scope, scopeKey, name, value string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("sqlite: PutVar: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("sqlite: PutVar: scope %q needs a non-empty scope key and name", scope)
	}
	const q = `
INSERT INTO vars (scope, scope_key, name, value, updated_at) VALUES (?,?,?,?,?)
ON CONFLICT (scope, scope_key, name) DO UPDATE SET
	value = excluded.value, updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q, scope, scopeKey, name, value, ts(time.Now()))
	return mapErr("sqlite: PutVar", err)
}

// ResolveVars merges org, then repo, then environment, exactly as secrets do.
func (s *Store) ResolveVars(ctx context.Context, owner, repo, environment string) (map[string]string, error) {
	out := map[string]string{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		rows, err := s.db.QueryContext(ctx,
			`SELECT name, value FROM vars WHERE scope = ? AND scope_key = ?`, sk[0], sk[1])
		if err != nil {
			return nil, mapErr("sqlite: ResolveVars", err)
		}
		for rows.Next() {
			var name, value string
			if err := rows.Scan(&name, &value); err != nil {
				rows.Close()
				return nil, mapErr("sqlite: ResolveVars", err)
			}
			out[name] = value
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, mapErr("sqlite: ResolveVars", err)
		}
	}
	return out, nil
}

func (s *Store) DeleteVar(ctx context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("sqlite: DeleteVar: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM vars WHERE scope = ? AND scope_key = ? AND name = ?`, scope, scopeKey, name)
	if err != nil {
		return mapErr("sqlite: DeleteVar", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: DeleteVar", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
