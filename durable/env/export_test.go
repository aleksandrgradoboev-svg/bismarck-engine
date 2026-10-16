package env

import "context"

// SetSpillFile replaces where a command's spill goes, so a test can make
// the spill fail (pi's FailingSpillExecutionEnv).
func SetSpillFile(l *Local, spillFile func(ctx context.Context) (string, error)) {
	l.spillFile = spillFile
}
