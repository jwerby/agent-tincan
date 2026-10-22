//go:build unix

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// chmodNoFollow changes path's mode without following a symlink. On Linux a
// symlink returns an error instead of changing the target.
func chmodNoFollow(path string, mode os.FileMode) error {
	return unix.Fchmodat(unix.AT_FDCWD, path, uint32(mode.Perm()), unix.AT_SYMLINK_NOFOLLOW)
}
