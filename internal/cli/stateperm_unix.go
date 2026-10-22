//go:build unix

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// chmodTree chmods root and everything under it through directory file
// descriptors. Each step is openat of one name relative to the parent fd
// with O_NOFOLLOW, then fchmod of that fd, so a directory symlink cannot
// redirect the walk and a final-component symlink is not followed.
func chmodTree(root string) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return fmt.Errorf("state dir %s is a symlink", root)
		}
		if errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("state dir %s is not a directory", root)
		}
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return err
	}
	return chmodChildren(fd)
}

func chmodChildren(dirfd int) error {
	dupfd, err := unix.Dup(dirfd)
	if err != nil {
		return err
	}
	unix.CloseOnExec(dupfd)
	f := os.NewFile(uintptr(dupfd), "")
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := chmodEntry(dirfd, e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// chmodEntry opens name under dirfd without following a symlink and chmods
// that descriptor. A symlink is left unchanged.
func chmodEntry(dirfd int, name string) error {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil
		}
		// A listening socket cannot be opened (ENXIO). chmod that directory
		// entry from the parent fd; AT_SYMLINK_NOFOLLOW will not follow a
		// symlink swapped in under this name.
		if errors.Is(err, unix.ENXIO) {
			return unix.Fchmodat(dirfd, name, 0o600, unix.AT_SYMLINK_NOFOLLOW)
		}
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return err
		}
		return chmodChildren(fd)
	}
	return unix.Fchmod(fd, 0o600)
}
