package env

import (
	"errors"
	"syscall"
)

// errorDirectory is Windows' ERROR_DIRECTORY, "the directory name is
// invalid": a file used as a directory.
const errorDirectory syscall.Errno = 267

func isNotDirectory(err error) bool {
	var e syscall.Errno
	return errors.As(err, &e) && e == errorDirectory
}
