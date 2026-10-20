//go:build race

package images

// raceDetector reports a build with the race detector, which stretches the
// encodings of the large noisy images to minutes; they test sizes and limits,
// not concurrency, and the test step without it runs them.
const raceDetector = true
