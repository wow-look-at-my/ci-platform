//go:build windows

package actions

import (
	"errors"
	"os"
)

// createNoFollow refuses rather than opening a path it cannot prove is not a
// symlink.
func createNoFollow(target string, mode os.FileMode) (*os.File, error) {
	return nil, errors.New("actions: extracting an action tarball needs O_NOFOLLOW, which Windows does not have; " +
		"run the runner on Linux")
}
