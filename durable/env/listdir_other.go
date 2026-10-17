//go:build !windows

package env

import "os"

// listedEntry is a directory entry; the inode in its identity makes a file
// index unnecessary.
type listedEntry struct {
	name  string
	index uint64
}

func listDirectory(directory string) ([]listedEntry, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	out := make([]listedEntry, len(entries))
	for i, entry := range entries {
		out[i] = listedEntry{name: entry.Name()}
	}
	return out, nil
}
