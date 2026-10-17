package env

import (
	"os"
	"runtime"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// listedEntry is a directory entry and its file index, zero where the
// listing carried none.
type listedEntry struct {
	name  string
	index uint64
}

// fileIDBothDirInfo is FILE_ID_BOTH_DIR_INFO with its short name left
// opaque; FileID sits at offset 96 and FileName at 104.
type fileIDBothDirInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    syscall.Filetime
	LastAccessTime  syscall.Filetime
	LastWriteTime   syscall.Filetime
	ChangeTime      syscall.Filetime
	EndOfFile       uint64
	AllocationSize  uint64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	_               [28]byte // ShortNameLength, ShortName
	FileID          uint64
	FileName        [1]uint16
}

const listBufferSize = 64 * 1024

// listDirectory lists a directory as os.ReadDir does, from the same open,
// and reads each entry's file index from the listing itself, as os.ReadDir
// does internally without exposing it: opening every entry would hold a
// handle that blocks renaming its parents. Where the file system lists no
// indexes, the entries come without them.
func listDirectory(directory string) ([]listedEntry, error) {
	f, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if entries, ok, err := listWithIndexes(f); ok {
		return entries, err
	}
	// No listing with indexes, or not a directory: os.ReadDir's own path
	// and errors.
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	entries := make([]listedEntry, len(names))
	for i, name := range names {
		entries[i] = listedEntry{name: name}
	}
	return entries, nil
}

// listWithIndexes reports ok false when the first request fails for a
// reason other than an empty directory, leaving the directory unread.
func listWithIndexes(f *os.File) (entries []listedEntry, ok bool, err error) {
	h := windows.Handle(f.Fd())
	defer runtime.KeepAlive(f)
	var flags uint32
	if windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, nil, 0) != nil ||
		flags&windows.FILE_SUPPORTS_OPEN_BY_FILE_ID == 0 {
		return nil, false, nil
	}
	// The entries hold 64-bit fields: the buffer must be 8-byte aligned,
	// which a byte slice is not.
	words := make([]uint64, listBufferSize/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), listBufferSize)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	for first := true; ; first = false {
		err := windows.GetFileInformationByHandleEx(h, class, &buffer[0], uint32(len(buffer)))
		if err == syscall.ERROR_NO_MORE_FILES || (first && err == syscall.ERROR_FILE_NOT_FOUND) {
			// ERROR_FILE_NOT_FOUND: an empty root directory (go.dev/issue/61159).
			break
		}
		if err != nil {
			if first {
				return nil, false, nil
			}
			return nil, true, &os.PathError{Op: "GetFileInformationByHandleEx", Path: f.Name(), Err: err}
		}
		class = windows.FileIdBothDirectoryInfo
		for offset := 0; ; {
			info := (*fileIDBothDirInfo)(unsafe.Pointer(&buffer[offset]))
			name := unsafe.Slice(&info.FileName[0], info.FileNameLength/2)
			if !(len(name) == 1 && name[0] == '.') && !(len(name) == 2 && name[0] == '.' && name[1] == '.') {
				entries = append(entries, listedEntry{name: syscall.UTF16ToString(name), index: info.FileID})
			}
			if info.NextEntryOffset == 0 {
				break
			}
			offset += int(info.NextEntryOffset)
		}
	}
	slices.SortFunc(entries, func(a, b listedEntry) int {
		switch {
		case a.name < b.name:
			return -1
		case a.name > b.name:
			return 1
		}
		return 0
	})
	return entries, true, nil
}
