package cli

import (
	"io/fs"
	"os"
	"path/filepath"
)

// secureStateDir does not follow or chmod symlinks, so a link inside the
// state directory cannot point the mode change at a file outside it.
func secureStateDir(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	walkRoot := root
	if info.Mode()&os.ModeSymlink != 0 {
		walkRoot, err = filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
	}
	return filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
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
		return os.Chmod(path, mode)
	})
}
