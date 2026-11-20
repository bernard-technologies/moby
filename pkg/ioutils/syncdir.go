package ioutils

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SyncDir persists the entries of an existing directory on filesystems that
// support directory fsync.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", path, err)
	}
	return nil
}

// SyncDirChain syncs dir and each of its parents through the existing root.
// Callers use it after MkdirAll to persist all the new directory entries.
func SyncDirChain(dir, root string) error {
	dir, root = filepath.Clean(dir), filepath.Clean(root)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("cannot sync directory chain from %s to %s", dir, root)
	}
	for {
		if err := SyncDir(dir); err != nil {
			return err
		}
		if dir == root {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}
