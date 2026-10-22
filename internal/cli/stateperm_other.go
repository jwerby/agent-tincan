//go:build !unix && !windows

package cli

import (
	"errors"
	"fmt"
)

// chmodTree refuses to start. These platforms have no O_NOFOLLOW open, and a
// path chmod would follow a symlink swapped in between the check and the change.
func chmodTree(root string) error {
	return fmt.Errorf("secure state dir %s: %w", root, errors.ErrUnsupported)
}
