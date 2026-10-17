package env

import (
	"io/fs"
	"syscall"
)

// fileID stands in for a file's identity on Windows. Its times alone are
// not enough: a directory recreated at the same path within one tick of
// the file system clock gets the old directory's creation and modification
// times. A directory therefore also carries its file index where one was
// read without holding a handle (see listDirectory) or from a short-lived
// handle on a watch target; zero is unknown.
type fileID struct {
	created, modified int64
	index             uint64
}

func identity(info fs.FileInfo) fileID {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}
	}
	id := fileID{created: d.CreationTime.Nanoseconds()}
	if info.IsDir() {
		id.modified = d.LastWriteTime.Nanoseconds()
	}
	return id
}

// ancestorIdentity omits an ancestor's modification time, which changes
// with every unrelated sibling. An ancestor gets no file index: one
// replaced within a tick replaces the target below it too, whose index
// tells.
func ancestorIdentity(info fs.FileInfo) fileID {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}
	}
	return fileID{created: d.CreationTime.Nanoseconds()}
}

func withIndex(id fileID, index uint64) fileID {
	id.index = index
	return id
}

// targetIndex is the file index of a watched directory, followed if it is
// a link, or zero. The handle shares everything and is closed at once: an
// open handle on a directory blocks renaming its parents, not the
// directory itself.
func targetIndex(path string) uint64 {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, err := syscall.CreateFile(name, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
