//go:build windows

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// chmodTree chmods root and its contents. Each chmod opens the path with
// FILE_FLAG_OPEN_REPARSE_POINT, the Windows equivalent of O_NOFOLLOW, and
// applies the mode to that handle so a symlink swapped in after the listing
// is not followed.
func chmodTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		return chmodNoFollow(path, mode)
	})
}

func chmodNoFollow(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	return f.Chmod(mode)
}
