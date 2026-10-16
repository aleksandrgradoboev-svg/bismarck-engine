//go:build !windows

package env

import (
	"errors"
	"os"
	"syscall"
)

// openRead opens a file for reading. It does not block on a FIFO without a
// writer, and with noFollow it refuses a final symbolic link (ELOOP).
func openRead(path string, noFollow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, 0)
}

// isSymlinkRefusal is O_NOFOLLOW's failure on a final symbolic link: ELOOP,
// or EMLINK on some BSDs.
func isSymlinkRefusal(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK)
}
