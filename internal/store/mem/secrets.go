package mem

import (
	"context"
	"fmt"
	"sort"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Scope names, matching the Postgres store's CHECK constraint.
const (
	ScopeOrg         = "org"
	ScopeRepo        = "repo"
	ScopeEnvironment = "environment"
)

// ScopeKey builds the storage key for a scope, identical to pg.ScopeKey.
// Callers must construct the same keys ResolveSecrets reads:
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

func (s *Store) PutSecret(_ context.Context, scope, scopeKey, name string, ciphertext []byte) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("mem: PutSecret: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("mem: PutSecret: scope %q needs a non-empty scope key and name", scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[scopedKey{scope, scopeKey, name}] = cloneBytes(ciphertext)
	return nil
}

// ResolveSecrets merges org, then repo, then environment. A name defined at a
// narrower scope replaces the wider one.
func (s *Store) ResolveSecrets(_ context.Context, owner, repo, environment string) (map[string][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string][]byte{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		for k, v := range s.secrets {
			if k.scope == sk[0] && k.scopeKey == sk[1] {
				out[k.name] = cloneBytes(v)
			}
		}
	}
	return out, nil
}

func (s *Store) DeleteSecret(_ context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("mem: DeleteSecret: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scopedKey{scope, scopeKey, name}
	if _, ok := s.secrets[k]; !ok {
		return store.ErrNotFound
	}
	delete(s.secrets, k)
	return nil
}

func (s *Store) ListSecretNames(_ context.Context, scope, scopeKey string) ([]string, error) {
	if err := validScope(scope); err != nil {
		return nil, fmt.Errorf("mem: ListSecretNames: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for k := range s.secrets {
		if k.scope == scope && k.scopeKey == scopeKey {
			out = append(out, k.name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) PutVar(_ context.Context, scope, scopeKey, name, value string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("mem: PutVar: %w", err)
	}
	if scopeKey == "" || name == "" {
		return fmt.Errorf("mem: PutVar: scope %q needs a non-empty scope key and name", scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vars[scopedKey{scope, scopeKey, name}] = value
	return nil
}

// ResolveVars merges org, then repo, then environment, exactly as secrets do.
func (s *Store) ResolveVars(_ context.Context, owner, repo, environment string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for _, sk := range resolutionOrder(owner, repo, environment) {
		for k, v := range s.vars {
			if k.scope == sk[0] && k.scopeKey == sk[1] {
				out[k.name] = v
			}
		}
	}
	return out, nil
}

func (s *Store) DeleteVar(_ context.Context, scope, scopeKey, name string) error {
	if err := validScope(scope); err != nil {
		return fmt.Errorf("mem: DeleteVar: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := scopedKey{scope, scopeKey, name}
	if _, ok := s.vars[k]; !ok {
		return store.ErrNotFound
	}
	delete(s.vars, k)
	return nil
}
