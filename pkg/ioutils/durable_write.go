package ioutils

import (
	"io"
	"os"
	"path/filepath"
)

// DurableAtomicWriteFile writes, syncs and atomically replaces a file, then
// syncs its existing parent directory. renamed is true even if that last sync
// fails; in that case callers must preserve both possible recovery states.
func DurableAtomicWriteFile(filename string, data []byte, mode os.FileMode) (renamed bool, err error) {
	return durableAtomicWriteFile(filename, data, mode, SyncDir)
}

func durableAtomicWriteFile(filename string, data []byte, mode os.FileMode, syncDir func(string) error) (renamed bool, err error) {
	dir := filepath.Dir(filename)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(filename))
	if err != nil {
		return false, err
	}
	defer func() {
		if !renamed {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if n, err := f.Write(data); err != nil {
		return false, err
	} else if n < len(data) {
		return false, io.ErrShortWrite
	}
	if err := f.Chmod(mode); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), filename); err != nil {
		return false, err
	}
	return true, syncDir(dir)
}
