package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecureStateDirTightensFilesAndSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "relay.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "attachments")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "blob"), []byte("blob"), 0o666); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(outsideDir, "other")
	if err := os.Mkdir(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "keep"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := secureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	checkMode(t, dir, 0o700)
	checkMode(t, filepath.Join(dir, "relay.db"), 0o600)
	checkMode(t, sub, 0o700)
	checkMode(t, filepath.Join(sub, "blob"), 0o600)
	checkMode(t, outside, 0o644)
	checkMode(t, linked, 0o755)
	checkMode(t, filepath.Join(linked, "keep"), 0o644)
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode %o, want %o", path, got, want)
	}
}
