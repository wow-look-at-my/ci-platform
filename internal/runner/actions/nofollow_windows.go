//go:build windows

package actions

import (
	"errors"
	"os"
)

// createNoFollow refuses rather than opening a path it cannot prove is not a
// symlink. Windows has no O_NOFOLLOW, and extracting an untrusted tarball
// without it is how a symlink entry turns a later file entry into an arbitrary
// write. Failing here costs nothing real: a job runs in a Linux DinD sandbox,
// so nothing on this platform ever reaches an extraction. Returning a
// silently weaker extractor would cost a great deal.
func createNoFollow(target string, mode os.FileMode) (*os.File, error) {
	return nil, errors.New("actions: extracting an action tarball needs O_NOFOLLOW, which Windows does not have; " +
		"run the runner on Linux")
}
