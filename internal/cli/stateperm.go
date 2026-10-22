package cli

import (
	"fmt"
	"os"
)

// secureStateDir tightens the state directory and its contents. The directory
// itself must not be a symlink. Modes are changed on file descriptors opened
// without following links: openat from the parent directory on unix, and
// O_NOFOLLOW (or a refusal to start) elsewhere. A link inside the tree cannot
// point the change at a file outside it.
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
	return chmodTree(root)
}
