package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// secureStateDir tightens the state directory and its contents. The directory
// itself must not be a symlink. Modes are changed without following links, so
// a link inside the tree cannot point the change at a file outside it.
func secureStateDir(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state dir %s is a symlink", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("state dir %s is not a directory", root)
	}
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
