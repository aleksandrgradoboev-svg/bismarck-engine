//go:build !windows

package env

import (
	"io/fs"
	"syscall"
)

// fileID is a file's identity: its device and inode.
type fileID struct{ dev, ino uint64 }

func identity(info fs.FileInfo) fileID {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	}
	return fileID{}
}

// ancestorIdentity is the identity of an ancestor: the same as any file's.
func ancestorIdentity(info fs.FileInfo) fileID { return identity(info) }

// withIndex keeps id: the inode already identifies the file.
func withIndex(id fileID, index uint64) fileID { return id }

func targetIndex(path string) uint64 { return 0 }
