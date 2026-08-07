//go:build !unix

package sandbox

import (
	"context"
	"fmt"
	"time"
)

type fileLock struct{}

// acquireLock fails rather than pretending the cache is safe to share: without
// a file lock, two dockerds would corrupt one graph directory.
func acquireLock(context.Context, string, time.Duration) (*fileLock, error) {
	return nil, fmt.Errorf("a shared image cache volume needs file locking, which this platform does not provide; run without an image cache volume")
}

func (l *fileLock) release() error { return nil }
