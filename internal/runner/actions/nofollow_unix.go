//go:build !windows

package actions

import (
	"os"
	"syscall"
)

// createNoFollow creates or truncates target for writing and refuses to open
// through a symlink sitting at that path.
func createNoFollow(target string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
}
