package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wow-look-at-my/go-containers/set"
)

// KeyStore remembers which idempotency keys this runner has started, across
// restarts.
type KeyStore struct {
	mu   sync.Mutex
	path string
	keys set.Set[string]
	f    *os.File
}

// OpenKeyStore loads the persisted set and opens it for appending.
func OpenKeyStore(path string) (*KeyStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("idempotency store: %w", err)
	}
	keys := set.New[string]()
	existing, err := os.Open(path)
	if err == nil {
		sc := bufio.NewScanner(existing)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				keys.Add(line)
			}
		}
		cerr := existing.Close()
		if serr := sc.Err(); serr != nil {
			return nil, fmt.Errorf("idempotency store %s: %w", path, serr)
		}
		if cerr != nil {
			return nil, cerr
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("idempotency store %s: %w", path, err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("idempotency store %s: %w", path, err)
	}
	return &KeyStore{path: path, keys: keys, f: f}, nil
}

// Started reports whether the key has already been started by this runner.
func (s *KeyStore) Started(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.Contains(key)
}

// MarkStarted records the key durably. It is called BEFORE the job runs and
// fsyncs, because a key recorded after execution is a key lost in the
// crash. That crash made it matter.
func (s *KeyStore) MarkStarted(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("idempotency store: refusing to record an empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys.Contains(key) {
		return nil
	}
	if _, err := s.f.WriteString(key + "\n"); err != nil {
		return fmt.Errorf("idempotency store %s: %w", s.path, err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("idempotency store %s: %w", s.path, err)
	}
	s.keys.Add(key)
	return nil
}

// Len is the number of recorded keys.
func (s *KeyStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.Len()
}

// Close releases the append handle.
func (s *KeyStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
