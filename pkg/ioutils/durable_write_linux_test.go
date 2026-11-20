package ioutils

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDurableAtomicWriteReportsPostRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	renamed, err := durableAtomicWriteFile(path, []byte("new"), 0600, func(gotDir string) error {
		if gotDir != dir {
			t.Fatalf("synced %s instead of %s", gotDir, dir)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
			t.Fatalf("parent sync did not follow rename: %q, %v", data, err)
		}
		return syscall.EIO
	})
	if !renamed || !errors.Is(err, syscall.EIO) {
		t.Fatalf("renamed=%v, err=%v", renamed, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0600 {
		t.Fatalf("unexpected file mode: %v", info.Mode())
	}
}

func TestDurableAtomicWriteRenameFailureDoesNotSyncParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	renamed, err := durableAtomicWriteFile(path, []byte("new"), 0600, func(string) error {
		t.Fatal("sync called after unsuccessful rename")
		return nil
	})
	if renamed || err == nil {
		t.Fatalf("renamed=%v, err=%v", renamed, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("temporary file leaked or destination changed: %v, %v", entries, err)
	}
}
