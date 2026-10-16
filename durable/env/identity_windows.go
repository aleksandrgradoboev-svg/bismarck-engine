package env

import (
	"io/fs"
	"syscall"
)

// fileID stands in for a file's identity on Windows, where the file index
// needs an open handle, and an open handle on a directory blocks renaming
// its parents, which is why watching polls here. The creation time changes
// when a path is replaced by another file; a directory also carries its
// modification time, which changes when it is recreated even where NTFS
// tunneling keeps a creation time.
type fileID struct{ created, modified int64 }

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
// with every unrelated sibling.
func ancestorIdentity(info fs.FileInfo) fileID {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fileID{}
	}
	return fileID{created: d.CreationTime.Nanoseconds()}
}
