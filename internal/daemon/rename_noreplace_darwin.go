//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func renameNoReplace(sourceFD int, source string, destinationFD int, destination string) error {
	return unix.RenameatxNp(sourceFD, source, destinationFD, destination, unix.RENAME_EXCL)
}
