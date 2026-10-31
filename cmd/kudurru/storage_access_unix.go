//go:build unix

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func storageAccess(path string, info os.FileInfo) error {
	mode := uint32(unix.R_OK | unix.W_OK)
	if info.IsDir() {
		mode |= unix.X_OK
	} else {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err != nil {
			return err
		}
		if stat.Nlink != 1 {
			return fmt.Errorf("hard-linked database or sidecar is not supported")
		}
	}
	return unix.Access(path, mode)
}
