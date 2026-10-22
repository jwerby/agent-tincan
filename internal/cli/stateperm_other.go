//go:build !unix

package cli

import (
	"fmt"
	"os"
)

func chmodNoFollow(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	return os.Chmod(path, mode)
}
