//go:build !windows

package actions

import (
	"os"
	"syscall"
)

// createNoFollow creates or truncates target for writing and refuses to open
// through a symlink sitting at that path.
//
// An earlier tar entry can create a symlink where a later entry writes a file;
// without O_NOFOLLOW that write lands wherever the link points. checkLinkTarget
// already refuses a link escaping the extraction directory, so this is the
// second lock on the same door -- one that also holds if something outside this
// process plants the link between the two entries.
func createNoFollow(target string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
}
