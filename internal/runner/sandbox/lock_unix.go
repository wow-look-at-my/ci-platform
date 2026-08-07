//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// fileLock is an advisory exclusive lock over the shared image-cache volume.
// Two dockerd processes sharing one /var/lib/docker corrupt it, so concurrent
// jobs serialize on the cache rather than quietly racing.
type fileLock struct {
	f *os.File
}

func acquireLock(ctx context.Context, path string, poll time.Duration) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("locking image cache %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("another job still holds the image cache lock %s: %w", path, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
