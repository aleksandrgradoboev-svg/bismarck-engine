package coding

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// WriteOutputFile writes data to a new output file and returns its path (pi
// utils/output-files.ts writeOutputFile). Output files are what the model may
// use of output it was not shown in full — the full text of truncated tool
// output, binary MCP resources, images shown by codemode scripts. They go to
// the OS temp directory as <prefix>-<16 hex digits><extension>; extension
// includes the dot. Output can carry private data, so only the user may read
// them, and the file is created exclusively, never following a link someone
// else placed at the path.
func WriteOutputFile(prefix, extension string, data []byte) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), prefix+"-"+hex.EncodeToString(b[:])+extension)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}
