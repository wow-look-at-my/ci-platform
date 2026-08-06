package pg

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
		return fmt.Errorf("pg: PutSecret: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("pg: PutSecret: scope %q needs a non-empty scope key and name", scope)
	}
	const q = `
INSERT INTO secrets (scope, scope_key, name, ciphertext, updated_at) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (scope, scope_key, name) DO UPDATE SET
	ciphertext = EXCLUDED.ciphertext, updated_at = EXCLUDED.updated_at`
	_, err := s.pool.Exec(ctx, q, scope, scopeKey, name, ciphertext, time.Now().UTC())
	return mapErr("pg: PutSecret", err)
}

// ResolveSecrets merges org, then repo, then environment. A name defined at a
// narrower scope replaces the wider one.
func (s *Store) ResolveSecrets(ctx context.Context, owner, repo, environment string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		rows, err := s.pool.Query(ctx,
			`SELECT name, ciphertext FROM secrets WHERE scope = $1 AND scope_key = $2`, sk[0], sk[1])
		if err != nil {
			return nil, mapErr("pg: ResolveSecrets", err)
		}
		for rows.Next() {
			var name string
			var ct []byte
			if err := rows.Scan(&name, &ct); err != nil {
				rows.Close()
				return nil, mapErr("pg: ResolveSecrets", err)
			}
			out[name] = ct
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, mapErr("pg: ResolveSecrets", err)
		}
	}
	return out, nil
}

func (s *Store) DeleteSecret(ctx context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("pg: DeleteSecret: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM secrets WHERE scope = $1 AND scope_key = $2 AND name = $3`, scope, scopeKey, name)
	if err != nil {
		return mapErr("pg: DeleteSecret", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListSecretNames(ctx context.Context, scope, scopeKey string) ([]string, error) {
	if err := validScope(scope); err != nil {
		return nil, fmt.Errorf("pg: ListSecretNames: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT name FROM secrets WHERE scope = $1 AND scope_key = $2 ORDER BY name`, scope, scopeKey)
	if err != nil {
		return nil, mapErr("pg: ListSecretNames", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, mapErr("pg: ListSecretNames", err)
		}
		out = append(out, n)
	}
	return out, mapErr("pg: ListSecretNames", rows.Err())
}

func (s *Store) PutVar(ctx context.Context, scope, scopeKey, name, value string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("pg: PutVar: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("pg: PutVar: scope %q needs a non-empty scope key and name", scope)
	}
	const q = `
INSERT INTO vars (scope, scope_key, name, value, updated_at) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (scope, scope_key, name) DO UPDATE SET
	value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`
	_, err := s.pool.Exec(ctx, q, scope, scopeKey, name, value, time.Now().UTC())
	return mapErr("pg: PutVar", err)
}

// ResolveVars merges org, then repo, then environment, exactly as secrets do.
func (s *Store) ResolveVars(ctx context.Context, owner, repo, environment string) (map[string]string, error) {
	out := map[string]string{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		rows, err := s.pool.Query(ctx,
			`SELECT name, value FROM vars WHERE scope = $1 AND scope_key = $2`, sk[0], sk[1])
		if err != nil {
			return nil, mapErr("pg: ResolveVars", err)
		}
		for rows.Next() {
			var name, value string
			if err := rows.Scan(&name, &value); err != nil {
				rows.Close()
				return nil, mapErr("pg: ResolveVars", err)
			}
			out[name] = value
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, mapErr("pg: ResolveVars", err)
		}
	}
	return out, nil
}

func (s *Store) DeleteVar(ctx context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("pg: DeleteVar: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM vars WHERE scope = $1 AND scope_key = $2 AND name = $3`, scope, scopeKey, name)
	if err != nil {
		return mapErr("pg: DeleteVar", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
