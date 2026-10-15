package durable

import (
	"errors"
	"fmt"
)

// The errors of durable (pi packages/durable/src/errors.ts).

// ErrorMessage is the message of an error value, never failing itself.
func ErrorMessage(err any) string {
	if e, ok := err.(error); ok {
		return e.Error()
	}
	return fmt.Sprint(err)
}

// ReadAfterWriteError is a transaction that read a table after its first
// table write. Read every required row before writing.
type ReadAfterWriteError struct{ Method string }

func (e *ReadAfterWriteError) Error() string {
	return fmt.Sprintf("Tx.%s() cannot read tables after the first table write", e.Method)
}

// StorageRequestError is a Storage read that rejected an invalid request,
// such as an unknown conversation or a cursor from another scan, with no
// durable effect. Unlike any other Storage error it fails only that read, not
// the Session; from Commit it is fatal like any other.
type StorageRequestError struct{ Message string }

func (e *StorageRequestError) Error() string { return e.Message }

// NewStorageRequestError makes a StorageRequestError.
func NewStorageRequestError(format string, args ...any) *StorageRequestError {
	return &StorageRequestError{Message: fmt.Sprintf(format, args...)}
}

// IsStorageRequestError reports whether err is, or wraps, a
// StorageRequestError.
func IsStorageRequestError(err error) bool {
	var target *StorageRequestError
	return errors.As(err, &target)
}

// SessionFailedError is a Session that failed, so nothing runs or commits any
// more: a Storage method failed, a commit could not be adopted, or the
// harness's own bookkeeping failed. Cause is that first error. Reopen the
// Session; reopening recovers from what was committed.
type SessionFailedError struct{ Cause error }

func (e *SessionFailedError) Error() string {
	return "Session failed after a storage error; close and reopen it"
}

func (e *SessionFailedError) Unwrap() error { return e.Cause }

// ConversationBusyError is a submission that reached a busy conversation and
// was not admitted.
type ConversationBusyError struct{ ConversationID ConversationID }

func (e *ConversationBusyError) Error() string {
	return fmt.Sprintf("Conversation %d is busy", e.ConversationID)
}
