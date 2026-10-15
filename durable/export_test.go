package durable

import "time"

// Internals the external tests synchronize on: Go calls block where pi's
// return promises, so a test that needs a call admitted or registered before
// the next one waits for it here.

// LineWaiting is how many admitted jobs wait for their turn on the line.
func LineWaiting(s *Session) int {
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	return s.lineWaiting
}

// Closing reports whether close has sealed admission.
func Closing(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// TxPending is how many operations of tx are underway.
func TxPending(tx *Tx) int {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.pending
}

// TxSealed reports whether the callback of tx has settled.
func TxSealed(tx *Tx) bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.sealed
}

// SetDefaultDocumentRetention sets the retention of every Session that keeps
// the default, and returns the previous one.
func SetDefaultDocumentRetention(d time.Duration) time.Duration {
	return time.Duration(defaultDocumentRetention.Swap(int64(d)))
}
