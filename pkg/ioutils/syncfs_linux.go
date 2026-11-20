package ioutils

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFilesystem flushes the filesystem containing f. Open f before the
// writes if it is to observe writeback errors recorded since those writes.
func SyncFilesystem(f *os.File) error {
	for {
		err := unix.Syncfs(int(f.Fd()))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("sync filesystem containing %s: %w", f.Name(), err)
		}
		return nil
	}
}
