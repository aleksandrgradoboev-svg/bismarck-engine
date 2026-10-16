// Package env is durable's execution environment (pi
// packages/durable/src/env): where files live and how commands run, as a
// capability the harness and its tools are given rather than the local OS
// they assume. Local is the environment of the machine the engine runs on.
//
// pi's Result<T, FileError> is Go's (T, error) with a *FileError, and pi's
// Context is a leading context.Context: a cancelled ctx fails an operation
// with the "aborted" code, before it starts or once it ends.
package env

import (
	"context"
	"time"
)

// FileKind is what a path names. Other kinds of files (devices, sockets,
// FIFOs) are not reported.
type FileKind string

const (
	KindFile      FileKind = "file"
	KindDirectory FileKind = "directory"
	KindSymlink   FileKind = "symlink"
)

// FileInfo describes a path without following a final symbolic link.
type FileInfo struct {
	Name    string
	Path    string
	Kind    FileKind
	Size    int64
	ModTime time.Time
}

// TextLine is one line of a text file.
type TextLine struct {
	Text string
	// Terminated reports whether a newline ended the line; a reader that
	// rebuilds records uses it to drop a torn last one.
	Terminated bool
}

// TextLineReader reads a UTF-8 text file line by line, splitting on "\n"
// only. ReadLine returns io.EOF after the last line. A cancelled read
// consumes nothing, so it can be retried.
type TextLineReader interface {
	ReadLine(ctx context.Context) (TextLine, error)
	// Close is best-effort and idempotent.
	Close() error
}

// LineRange selects lines [Start, End) of a file, 0-based: line k starts
// after the k-th newline byte. End 0 is to the end; otherwise End must
// exceed Start.
type LineRange struct {
	Start int64
	End   int64
}

// LineScan is where lines of a file are, as BinaryReader.ScanLines found
// them.
type LineScan struct {
	// Newlines is the number of newline bytes in the whole file; it has
	// Newlines + 1 lines.
	Newlines int64
	// Start and End are the byte range of the selected lines: from the start
	// of the first to the end of the last, without the newline that ends it.
	// A selection past the last line is empty at the end of the file.
	Start, End int64
	// FirstLineEnd is where the first selected line ends: its newline, or
	// the end of the file.
	FirstLineEnd int64
	// LastLineStart is where the last selected line starts.
	LastLineStart int64
	// SelectedBytes and FirstLineBytes are the UTF-8 lengths of the decoded
	// selection and of its first line: of the text decoding the whole file
	// would produce for that range, so a leading byte-order mark is not
	// counted.
	SelectedBytes, FirstLineBytes int64
}

// BinaryReader reads positions of one opened regular file; every call sees
// that file even if its path is renamed.
type BinaryReader interface {
	// Info is the opened file's metadata, not that of whatever its path
	// names now.
	Info(ctx context.Context) (FileInfo, error)
	// Read returns up to length bytes at offset; fewer only at the end of
	// the file.
	Read(ctx context.Context, offset, length int64) ([]byte, error)
	// ScanLines locates lines in one pass over the file.
	ScanLines(ctx context.Context, lines LineRange) (LineScan, error)
	// Close is best-effort and idempotent.
	Close() error
}

// DirReader pages one directory's entries.
type DirReader interface {
	// Next returns up to maxEntries entries in the order the file system
	// returns them, continuing where the previous call stopped; done marks
	// the end, with the last entries or with an empty page. An entry that
	// disappears before its metadata is read is skipped, as are entries of
	// unsupported kinds. After a failed or cancelled call, close the reader.
	Next(ctx context.Context, maxEntries int) (entries []FileInfo, done bool, err error)
	// Close is best-effort and idempotent.
	Close() error
}

// BinaryReaderOptions open a BinaryReader.
type BinaryReaderOptions struct {
	// NoFollow refuses a symbolic link as the final path component with the
	// "invalid" code instead of following it; earlier components still
	// resolve.
	NoFollow bool
}

// RemoveOptions remove a path.
type RemoveOptions struct {
	// Recursive removes a directory with everything below it; without it a
	// directory is refused.
	Recursive bool
	// Force ignores a missing path.
	Force bool
}

// TempFileOptions name a temporary file.
type TempFileOptions struct {
	Prefix, Suffix string
}

// AllLines is ReadTextLines' maxLines for every line.
const AllLines = -1

// FileSystem is the file half of an environment. Paths that are not
// absolute resolve against Cwd; "~" and "~/..." against the home directory,
// and a file:// URL is its path.
type FileSystem interface {
	// ID names the file namespace: equal IDs see the same files at the same
	// paths, whatever their Cwd. Every Local shares one ID.
	ID() string
	Cwd() string
	// AbsolutePath resolves a path without requiring it to exist or
	// resolving symbolic links.
	AbsolutePath(ctx context.Context, path string) (string, error)
	JoinPath(ctx context.Context, parts []string) (string, error)
	// ReadTextFile reads a whole file as UTF-8, invalid sequences replaced.
	ReadTextFile(ctx context.Context, path string) (string, error)
	OpenTextLineReader(ctx context.Context, path string) (TextLineReader, error)
	// ReadTextLines reads at most maxLines lines; AllLines reads them all,
	// and 0 reads none.
	ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error)
	ReadBinaryFile(ctx context.Context, path string) ([]byte, error)
	// OpenBinaryReader opens a regular file for bounded positional reads. A
	// directory fails with "is_directory", another kind of file with
	// "invalid".
	OpenBinaryReader(ctx context.Context, path string, options BinaryReaderOptions) (BinaryReader, error)
	// WriteFile replaces a file's content, creating it and its parent
	// directories.
	WriteFile(ctx context.Context, path string, content []byte) error
	// AppendFile appends to a file, creating it and its parent directories.
	AppendFile(ctx context.Context, path string, content []byte) error
	// TruncateFile cuts or extends an existing file to exactly size bytes.
	TruncateFile(ctx context.Context, path string, size int64) error
	// FlushFile makes a file's content and the metadata needed to retrieve
	// it durable.
	FlushFile(ctx context.Context, path string) error
	// RenameFile replaces destination with source.
	RenameFile(ctx context.Context, source, destination string) error
	// Stat is pi's fileInfo; a Go method cannot share its result type's
	// name.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// ListDir lists a directory's entries of supported kinds.
	ListDir(ctx context.Context, path string) ([]FileInfo, error)
	OpenDirReader(ctx context.Context, path string) (DirReader, error)
	// CanonicalPath resolves symbolic links of an existing path.
	CanonicalPath(ctx context.Context, path string) (string, error)
	// Exists reports whether a path exists; a missing path is not an error.
	Exists(ctx context.Context, path string) (bool, error)
	CreateDir(ctx context.Context, path string, recursive bool) error
	Remove(ctx context.Context, path string, options RemoveOptions) error
	// CreateTempDir creates a fresh directory in the system's temporary
	// directory; "" is the prefix "tmp-".
	CreateTempDir(ctx context.Context, prefix string) (string, error)
	// CreateTempFile creates an empty file in a fresh temporary directory.
	CreateTempFile(ctx context.Context, options TempFileOptions) (string, error)
	// Cleanup releases what the environment still holds, for its owner's
	// shutdown. Best-effort.
	Cleanup(ctx context.Context)
}
