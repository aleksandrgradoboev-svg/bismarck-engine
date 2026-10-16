package env

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// errSymlink marks a refused final symbolic link.
var errSymlink = errors.New("final path component is a symbolic link")

// openRead opens a file for reading with every sharing mode, so the file can
// be renamed or deleted while it is open, as on POSIX; directories open too,
// for their metadata. Windows has no O_NOFOLLOW: noFollow checks the final
// component first, which is not race-free.
func openRead(path string, noFollow bool) (*os.File, error) {
	if noFollow {
		st, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return nil, errSymlink
		}
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func isSymlinkRefusal(err error) bool { return errors.Is(err, errSymlink) }
