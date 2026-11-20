//go:build !linux
// +build !linux

package ioutils

import (
	"errors"
	"os"
)

func SyncFilesystem(*os.File) error {
	return errors.New("filesystem sync is only supported on Linux")
}
