package env

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// localID is the namespace every Local shares: they all see the machine's
// files.
const localID = "local"

const (
	// binaryReadChunk is the largest single read of a BinaryReader, so a
	// huge length allocates only as much as the file yields.
	binaryReadChunk = 1024 * 1024
	// textChunk is the read size of text line readers and line scans.
	textChunk = 64 * 1024
)

// Local is the environment of the machine the engine runs on (pi
// NodeExecutionEnv). It is safe for concurrent use.
type Local struct {
	cwd       string
	shellPath string
	shellEnv  map[string]string
	watch     WatchOptions

	mu     sync.Mutex
	active map[*exec.Cmd]bool

	// spillFile replaces the spill file a command's output goes to, for
	// tests.
	spillFile func(ctx context.Context) (string, error)
}

var _ FileSystem = (*Local)(nil)

// LocalOptions configure a Local.
type LocalOptions struct {
	// ShellPath is the bash a script runs through; "" resolves one as pi
	// does (Git Bash on Windows, /bin/bash, bash on PATH, sh).
	ShellPath string
	// ShellEnv adds variables to the inherited environment of every command.
	ShellEnv map[string]string
	Watch    WatchOptions
}

// NewLocal returns the local environment working in cwd; "" is the
// process's working directory.
func NewLocal(cwd string) *Local { return NewLocalWith(cwd, LocalOptions{}) }

// NewLocalWith returns a configured local environment working in cwd.
func NewLocalWith(cwd string, options LocalOptions) *Local {
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return &Local{cwd: cwd, shellPath: options.ShellPath, shellEnv: options.ShellEnv, watch: options.Watch}
}

func (l *Local) ID() string  { return localID }
func (l *Local) Cwd() string { return l.cwd }

// resolve is pi's resolvePath: "~" and "~/..." (and "~\..." on Windows)
// under the home directory, a file:// URL as its path, and a relative path
// against the working directory. A malformed URL stays an ordinary path.
func (l *Local) resolve(path string) string {
	normalized := path
	home := func() string { h, _ := os.UserHomeDir(); return h }
	switch {
	case normalized == "~":
		normalized = home()
	case strings.HasPrefix(normalized, "~/") || (runtime.GOOS == "windows" && strings.HasPrefix(normalized, `~\`)):
		normalized = filepath.Join(home(), normalized[2:])
	case strings.HasPrefix(normalized, "file://"):
		if p, ok := fileURLPath(normalized); ok {
			normalized = p
		}
	}
	if filepath.IsAbs(normalized) {
		return filepath.Clean(normalized)
	}
	return filepath.Join(l.cwd, normalized)
}

// fileURLPath is Node's fileURLToPath for the URLs it accepts.
func fileURLPath(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if runtime.GOOS != "windows" {
		if u.Host != "" && u.Host != "localhost" {
			return "", false
		}
		return p, true
	}
	if u.Host != "" && u.Host != "localhost" {
		return `\\` + u.Host + filepath.FromSlash(p), true // a UNC path
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p), true
}

func (l *Local) AbsolutePath(ctx context.Context, path string) (string, error) {
	return l.resolve(path), nil
}

func (l *Local) JoinPath(ctx context.Context, parts []string) (string, error) {
	return filepath.Join(parts...), nil
}

func fileInfoOf(path string, st fs.FileInfo) (FileInfo, error) {
	var kind FileKind
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		kind = KindSymlink
	case st.IsDir():
		kind = KindDirectory
	case st.Mode().IsRegular():
		kind = KindFile
	default:
		return FileInfo{}, newFileError(CodeInvalid, "Unsupported file type", path)
	}
	return FileInfo{Name: filepath.Base(path), Path: path, Kind: kind, Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *Local) ReadTextFile(ctx context.Context, path string) (string, error) {
	data, err := l.ReadBinaryFile(ctx, path)
	if err != nil {
		return "", err
	}
	// Node's utf8 Buffer decoding: invalid sequences replaced, a
	// byte-order mark kept.
	return jstext.DecodeUTF8(data), nil
}

func (l *Local) OpenTextLineReader(ctx context.Context, path string) (TextLineReader, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	f, err := openRead(resolved, false)
	if err != nil {
		return nil, toFileError(err, resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		f.Close()
		return nil, err
	}
	return &textLineReader{file: f, path: resolved}, nil
}

func (l *Local) ReadTextLines(ctx context.Context, path string, maxLines int) ([]string, error) {
	if maxLines == 0 || (maxLines < 0 && maxLines != AllLines) {
		return []string{}, nil
	}
	reader, err := l.OpenTextLineReader(ctx, path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	lines := []string{}
	for maxLines == AllLines || len(lines) < maxLines {
		line, err := reader.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, line.Text)
	}
	return lines, nil
}

func (l *Local) ReadBinaryFile(ctx context.Context, path string) ([]byte, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, toFileError(err, resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	return data, nil
}

func symlinkRefused(path string, cause error) error {
	return &FileError{Code: CodeInvalid, Message: "Refusing to follow a symbolic link", Path: path, Cause: cause}
}

func (l *Local) OpenBinaryReader(ctx context.Context, path string, options BinaryReaderOptions) (BinaryReader, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	f, err := openRead(resolved, options.NoFollow)
	if err != nil {
		if options.NoFollow && isSymlinkRefusal(err) {
			return nil, symlinkRefused(resolved, err)
		}
		return nil, toFileError(err, resolved)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, toFileError(err, resolved)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		if st.IsDir() {
			return nil, newFileError(CodeIsDirectory, "EISDIR: illegal operation on a directory, read", resolved)
		}
		return nil, newFileError(CodeInvalid, "Not a regular file", resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		f.Close()
		return nil, err
	}
	return &binaryReader{file: f, path: resolved}, nil
}

func (l *Local) WriteFile(ctx context.Context, path string, content []byte) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return toFileError(err, resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	if err := os.WriteFile(resolved, content, 0o644); err != nil {
		return toFileError(err, resolved)
	}
	return nil
}

func (l *Local) AppendFile(ctx context.Context, path string, content []byte) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return toFileError(err, resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	f, err := os.OpenFile(resolved, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return toFileError(err, resolved)
	}
	_, err = f.Write(content)
	if err = errors.Join(err, f.Close()); err != nil {
		return toFileError(err, resolved)
	}
	return aborted(ctx, resolved)
}

func (l *Local) TruncateFile(ctx context.Context, path string, size int64) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	if size < 0 || size > maxSafeInteger {
		return newFileError(CodeInvalid, "File size must be a non-negative safe integer", resolved)
	}
	f, err := os.OpenFile(resolved, os.O_RDWR, 0)
	if err != nil {
		return toFileError(err, resolved)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return toFileError(err, resolved)
	}
	return aborted(ctx, resolved)
}

func (l *Local) FlushFile(ctx context.Context, path string) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	// POSIX refuses to open a directory for writing and Windows opens it, so
	// check first.
	if st, err := os.Stat(resolved); err == nil && st.IsDir() {
		return newFileError(CodeIsDirectory, "Is a directory", resolved)
	}
	f, err := os.OpenFile(resolved, os.O_RDWR, 0)
	if err != nil {
		return toFileError(err, resolved)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return toFileError(err, resolved)
	}
	return aborted(ctx, resolved)
}

func (l *Local) RenameFile(ctx context.Context, source, destination string) error {
	from, to := l.resolve(source), l.resolve(destination)
	if err := aborted(ctx, to); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return toFileError(err, from)
	}
	return nil
}

func (l *Local) Stat(ctx context.Context, path string) (FileInfo, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return FileInfo{}, err
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return FileInfo{}, toFileError(err, resolved)
	}
	return fileInfoOf(resolved, st)
}

// notDirectory is the failure of using a file as a directory.
func notDirectory(path string) error {
	return newFileError(CodeNotDirectory, "ENOTDIR: not a directory, scandir", path)
}

func (l *Local) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		if st, statErr := os.Stat(resolved); statErr == nil && !st.IsDir() {
			return nil, notDirectory(resolved)
		}
		return nil, toFileError(err, resolved)
	}
	infos := []FileInfo{}
	for _, entry := range entries {
		if err := aborted(ctx, resolved); err != nil {
			return nil, err
		}
		entryPath := filepath.Join(resolved, entry.Name())
		st, err := os.Lstat(entryPath)
		if err != nil {
			return nil, toFileError(err, entryPath)
		}
		if info, err := fileInfoOf(entryPath, st); err == nil {
			infos = append(infos, info)
		}
	}
	return infos, nil
}

func (l *Local) OpenDirReader(ctx context.Context, path string) (DirReader, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return nil, err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return nil, toFileError(err, resolved)
	}
	if !st.IsDir() {
		return nil, notDirectory(resolved)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, toFileError(err, resolved)
	}
	if err := aborted(ctx, resolved); err != nil {
		f.Close()
		return nil, err
	}
	return &dirReader{dir: f, path: resolved}, nil
}

func (l *Local) CanonicalPath(ctx context.Context, path string) (string, error) {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", toFileError(err, resolved)
	}
	return canonical, nil
}

func (l *Local) Exists(ctx context.Context, path string) (bool, error) {
	_, err := l.Stat(ctx, path)
	switch {
	case err == nil:
		return true, nil
	case Code(err) == CodeNotFound:
		return false, nil
	}
	return false, err
}

func (l *Local) CreateDir(ctx context.Context, path string, recursive bool) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	var err error
	if recursive {
		err = os.MkdirAll(resolved, 0o755)
	} else {
		err = os.Mkdir(resolved, 0o755)
	}
	if err != nil {
		return toFileError(err, resolved)
	}
	return nil
}

func (l *Local) Remove(ctx context.Context, path string, options RemoveOptions) error {
	resolved := l.resolve(path)
	if err := aborted(ctx, resolved); err != nil {
		return err
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && options.Force {
			return nil
		}
		return toFileError(err, resolved)
	}
	if st.IsDir() && !options.Recursive {
		return newFileError(CodeIsDirectory, "Path is a directory: rm returned EISDIR (is a directory) "+resolved, resolved)
	}
	if st.IsDir() {
		err = os.RemoveAll(resolved)
	} else {
		err = os.Remove(resolved)
	}
	if err != nil && !(options.Force && errors.Is(err, fs.ErrNotExist)) {
		return toFileError(err, resolved)
	}
	return nil
}

func (l *Local) CreateTempDir(ctx context.Context, prefix string) (string, error) {
	if err := aborted(ctx, ""); err != nil {
		return "", err
	}
	if prefix == "" {
		prefix = "tmp-"
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", toFileError(err, "")
	}
	return dir, nil
}

func (l *Local) CreateTempFile(ctx context.Context, options TempFileOptions) (string, error) {
	dir, err := l.CreateTempDir(ctx, "tmp-")
	if err != nil {
		return "", err
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	path := filepath.Join(dir, options.Prefix+hex.EncodeToString(id[:])+options.Suffix)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", toFileError(err, path)
	}
	return path, nil
}

// textLineReader is pi's strict-LF reader: Node's readline cannot tell
// whether the last line ended with a newline. It reads at explicit offsets,
// so a cancelled read consumes nothing.
type textLineReader struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	decoder  jstext.TextDecoder
	chunk    []byte
	offset   int64
	buffered string
	ended    bool
	closed   bool
}

func (r *textLineReader) ReadLine(ctx context.Context) (TextLine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := aborted(ctx, r.path); err != nil {
		return TextLine{}, err
	}
	if r.closed {
		return TextLine{}, newFileError(CodeInvalid, "Text line reader is closed", r.path)
	}
	if r.chunk == nil {
		r.chunk = make([]byte, textChunk)
	}
	for {
		if i := strings.IndexByte(r.buffered, '\n'); i >= 0 {
			text := r.buffered[:i]
			r.buffered = r.buffered[i+1:]
			return TextLine{Text: text, Terminated: true}, nil
		}
		if r.ended {
			if r.buffered == "" {
				return TextLine{}, io.EOF
			}
			text := r.buffered
			r.buffered = ""
			return TextLine{Text: text}, nil
		}
		n, err := r.file.ReadAt(r.chunk, r.offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return TextLine{}, toFileError(err, r.path)
		}
		if err := aborted(ctx, r.path); err != nil {
			return TextLine{}, err
		}
		r.offset += int64(n)
		if n == 0 {
			r.buffered += r.decoder.Flush()
			r.ended = true
		} else {
			r.buffered += r.decoder.Decode(r.chunk[:n])
		}
	}
}

func (r *textLineReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.buffered = ""
	_ = r.file.Close()
	return nil
}

type binaryReader struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	closed bool
}

func (r *binaryReader) closedError() error {
	return newFileError(CodeInvalid, "Binary reader is closed", r.path)
}

func (r *binaryReader) Info(ctx context.Context) (FileInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := aborted(ctx, r.path); err != nil {
		return FileInfo{}, err
	}
	if r.closed {
		return FileInfo{}, r.closedError()
	}
	st, err := r.file.Stat()
	if err != nil {
		return FileInfo{}, toFileError(err, r.path)
	}
	return fileInfoOf(r.path, st)
}

func (r *binaryReader) Read(ctx context.Context, offset, length int64) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := aborted(ctx, r.path); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, r.closedError()
	}
	if offset < 0 || offset > maxSafeInteger || length < 0 || length > maxSafeInteger {
		return nil, newFileError(CodeInvalid, "Offset and length must be non-negative safe integers", r.path)
	}
	out := []byte{}
	for int64(len(out)) < length {
		chunk := make([]byte, min(length-int64(len(out)), binaryReadChunk))
		n, err := r.file.ReadAt(chunk, offset+int64(len(out)))
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, toFileError(err, r.path)
		}
		if err := aborted(ctx, r.path); err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		if len(out) == 0 && n == len(chunk) && int64(n) == length {
			return chunk, nil
		}
		out = append(out, chunk[:n]...)
	}
	return out, nil
}

func (r *binaryReader) ScanLines(ctx context.Context, lines LineRange) (LineScan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := aborted(ctx, r.path); err != nil {
		return LineScan{}, err
	}
	if r.closed {
		return LineScan{}, r.closedError()
	}
	scanner, err := NewLineScanner(lines)
	if err != nil {
		return LineScan{}, newFileError(CodeInvalid, "Invalid line range", r.path)
	}
	chunk := make([]byte, textChunk)
	for position := int64(0); ; {
		n, err := r.file.ReadAt(chunk, position)
		if err != nil && !errors.Is(err, io.EOF) {
			return LineScan{}, toFileError(err, r.path)
		}
		if err := aborted(ctx, r.path); err != nil {
			return LineScan{}, err
		}
		if n == 0 {
			return scanner.Finish(), nil
		}
		scanner.Push(chunk[:n])
		position += int64(n)
	}
}

func (r *binaryReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = r.file.Close()
	}
	return nil
}

type dirReader struct {
	mu     sync.Mutex
	dir    *os.File
	path   string
	done   bool
	closed bool
}

func (r *dirReader) Next(ctx context.Context, maxEntries int) ([]FileInfo, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := aborted(ctx, r.path); err != nil {
		return nil, false, err
	}
	if r.closed {
		return nil, false, newFileError(CodeInvalid, "Directory reader is closed", r.path)
	}
	if maxEntries <= 0 {
		return nil, false, newFileError(CodeInvalid, "maxEntries must be a positive safe integer", r.path)
	}
	entries := []FileInfo{}
	for !r.done && len(entries) < maxEntries {
		batch, err := r.dir.ReadDir(maxEntries - len(entries))
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			r.done = true
			break
		}
		if err != nil {
			return nil, false, toFileError(err, r.path)
		}
		for _, entry := range batch {
			if err := aborted(ctx, r.path); err != nil {
				return nil, false, err
			}
			entryPath := filepath.Join(r.path, entry.Name())
			st, err := os.Lstat(entryPath)
			if err != nil {
				// Removed between enumeration and lstat: not part of the
				// listing any more.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, false, toFileError(err, entryPath)
			}
			if info, err := fileInfoOf(entryPath, st); err == nil {
				entries = append(entries, info)
			}
		}
	}
	return entries, r.done, nil
}

func (r *dirReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = r.dir.Close()
	}
	return nil
}
