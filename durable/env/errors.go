package env

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// FileErrorCode classifies a FileError.
type FileErrorCode string

const (
	CodeAborted          FileErrorCode = "aborted"
	CodeNotFound         FileErrorCode = "not_found"
	CodePermissionDenied FileErrorCode = "permission_denied"
	CodeNotDirectory     FileErrorCode = "not_directory"
	CodeIsDirectory      FileErrorCode = "is_directory"
	CodeInvalid          FileErrorCode = "invalid"
	CodeNotSupported     FileErrorCode = "not_supported"
	CodeUnknown          FileErrorCode = "unknown"
)

// FileError is an expected failure of a file operation.
type FileError struct {
	Code    FileErrorCode
	Message string
	// Path is the path the failure concerns, when there is one.
	Path  string
	Cause error
}

func (e *FileError) Error() string { return e.Message }
func (e *FileError) Unwrap() error { return e.Cause }

// Code is the code of err's FileError, or "" when it holds none.
func Code(err error) FileErrorCode {
	var fe *FileError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

func newFileError(code FileErrorCode, message, path string) *FileError {
	return &FileError{Code: code, Message: message, Path: path}
}

// aborted is the FileError of a cancelled ctx, or nil while it runs.
func aborted(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return &FileError{Code: CodeAborted, Message: "aborted", Path: path, Cause: err}
	}
	return nil
}

// toFileError classifies an error of the os package. A path the error names
// wins over the fallback.
func toFileError(err error, fallbackPath string) error {
	var fe *FileError
	if errors.As(err, &fe) {
		return fe
	}
	path := fallbackPath
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		path = pathErr.Path
	case errors.As(err, &linkErr):
		path = linkErr.Old
	}
	code := CodeUnknown
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = CodeAborted
	case errors.Is(err, fs.ErrNotExist):
		code = CodeNotFound
	case errors.Is(err, fs.ErrPermission):
		code = CodePermissionDenied
	case isErrno(err, syscall.ENOTDIR), isNotDirectory(err):
		code = CodeNotDirectory
	case isErrno(err, syscall.EISDIR):
		code = CodeIsDirectory
	case isErrno(err, syscall.EINVAL):
		code = CodeInvalid
	}
	return &FileError{Code: code, Message: err.Error(), Path: path, Cause: err}
}

func isErrno(err error, errno syscall.Errno) bool {
	var e syscall.Errno
	return errors.As(err, &e) && e == errno
}
