module pidiff

go 1.26

require github.com/aleksandrgradoboev-svg/bismarck-engine v0.0.0

require golang.org/x/text v0.37.0 // indirect

// difftest is its OWN module, and that is load-bearing rather than incidental:
// a subdirectory containing a go.mod is excluded from the parent module, so the
// github.com/aleksandrgradoboev-svg/bismarck-engine module ships not one byte of this
// harness. It stays in the repo for lockstep versioning with the code it
// verifies, and out of the artefact consumers download.
//
// Relative so the harness works in any clone: difftest -> repo root.
// The two sides live in pi/ (real pi) and port/ (this port); the module root is
// difftest/ itself, so `go build ./...` here cannot collide with a dir named go.
replace github.com/aleksandrgradoboev-svg/bismarck-engine => ..
