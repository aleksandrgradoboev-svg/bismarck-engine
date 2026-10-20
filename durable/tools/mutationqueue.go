package tools

import (
	"context"
	"strings"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// mutationQueues holds the tail of the mutation chain of each file, keyed by
// file system ID and canonical path: a channel closed when the last call to
// take the slot finishes.
var (
	mutationMu     sync.Mutex
	mutationQueues = map[string]chan struct{}{}
)

func mutationKey(ctx context.Context, e env.ExecutionEnv, path string) (string, error) {
	absolute, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalPath(ctx, e, absolute)
	if err != nil {
		return "", err
	}
	return e.ID() + "\x00" + canonical, nil
}

// canonicalPath is the canonical path; for a file that does not exist yet,
// its canonical parent joined with its name, so a write that creates a file
// and a later mutation of it share one key even under a symlinked directory.
func canonicalPath(ctx context.Context, e env.ExecutionEnv, absolute string) (string, error) {
	canonical, err := e.CanonicalPath(ctx, absolute)
	if err == nil {
		return canonical, nil
	}
	switch env.Code(err) {
	case env.CodeNotSupported:
		return absolute, nil
	case env.CodeNotFound:
	default:
		return "", err
	}
	// The file system splits the path, so a name may contain characters that
	// are separators elsewhere.
	parent, err := e.JoinPath(ctx, []string{absolute, ".."})
	if err != nil {
		return "", err
	}
	if parent == absolute || !strings.HasPrefix(absolute, parent) {
		return absolute, nil
	}
	skip := 1
	if strings.HasSuffix(parent, "/") || strings.HasSuffix(parent, `\`) {
		skip = 0
	}
	name := absolute[len(parent)+skip:]
	canonicalParent, err := canonicalPath(ctx, e, parent)
	if err != nil {
		return "", err
	}
	return e.JoinPath(ctx, []string{canonicalParent, name})
}

// withFileMutationQueue serializes edit and write mutations of one file
// within this process: same file system ID and canonical path, whichever
// environment object the call got. Other files, and other file systems, never
// wait. Concurrent calls on one file run in the order their keys resolve. Not
// a lock against bash or other processes. Waiting ignores ctx, as in pi: a
// call that was cancelled still waits its turn, and fn sees the cancellation.
func withFileMutationQueue[T any](ctx context.Context, e env.ExecutionEnv, path string, fn func() (T, error)) (T, error) {
	key, err := mutationKey(ctx, e, path)
	if err != nil {
		var zero T
		return zero, err
	}
	done := make(chan struct{})
	mutationMu.Lock()
	previous := mutationQueues[key]
	mutationQueues[key] = done
	mutationMu.Unlock()
	defer func() {
		mutationMu.Lock()
		if mutationQueues[key] == done {
			delete(mutationQueues, key)
		}
		mutationMu.Unlock()
		close(done)
	}()
	if previous != nil {
		<-previous
	}
	return fn()
}
