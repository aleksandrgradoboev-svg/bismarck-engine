//go:build race

package harness

// raceDetector reports a build with the race detector, which stretches the
// depth tests to minutes; they test depth, not concurrency, and the test step
// without it runs them.
const raceDetector = true
