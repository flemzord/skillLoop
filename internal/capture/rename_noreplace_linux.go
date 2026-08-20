//go:build linux

package capture

import "golang.org/x/sys/unix"

func renameNoReplace(sourceFD int, source string, destinationFD int, destination string) error {
	return unix.Renameat2(sourceFD, source, destinationFD, destination, unix.RENAME_NOREPLACE)
}
