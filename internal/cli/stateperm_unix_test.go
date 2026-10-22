//go:build unix

package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// chmodEntry opens one directory entry from a directory fd. A symlink is not
// a path the kernel walks, so the target's mode stays as it was.
func TestChmodEntryDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dirfd)
	if err := chmodEntry(dirfd, "link"); err != nil {
		t.Fatal(err)
	}
	checkMode(t, outside, 0o644)

	nested := filepath.Join(t.TempDir(), "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(nested, "keep")
	if err := os.WriteFile(leaf, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := chmodEntry(dirfd, "linked-dir"); err != nil {
		t.Fatal(err)
	}
	checkMode(t, nested, 0o755)
	checkMode(t, leaf, 0o644)
}

func TestSecureStateDirChmodsUnixSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := secureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path, 0o600)
	checkMode(t, dir, 0o700)
}
