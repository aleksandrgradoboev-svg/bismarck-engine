package providers

import (
	"regexp"
	"runtime"
	"testing"
)

// piUserAgent must render Node's tokens, not Go's: pi user-agent strings are
// compared byte-for-byte against pi (utils/pi-user-agent.ts getPiUserAgent).
func TestPiUserAgentShape(t *testing.T) {
	ua := piUserAgent()
	if _, ok := osRelease(); !ok {
		// GOOSes without a release syscall degrade to pi's no-node:os string.
		if ua != "pi (browser)" {
			t.Fatalf("piUserAgent() = %q, want \"pi (browser)\" fallback", ua)
		}
		return
	}
	m := regexp.MustCompile(`^pi \((\S+) (.+); (\S+)\)$`).FindStringSubmatch(ua)
	if m == nil {
		t.Fatalf("piUserAgent() = %q, want \"pi (<platform> <release>; <arch>)\"", ua)
	}
	wantPlatform := runtime.GOOS
	if wantPlatform == "windows" {
		wantPlatform = "win32"
	}
	if m[1] != wantPlatform {
		t.Fatalf("platform token = %q, want Node name %q", m[1], wantPlatform)
	}
	archMap := map[string]string{"amd64": "x64", "386": "ia32", "mipsle": "mipsel", "ppc64le": "ppc64"}
	wantArch := archMap[runtime.GOARCH]
	if wantArch == "" {
		wantArch = runtime.GOARCH
	}
	if m[3] != wantArch {
		t.Fatalf("arch token = %q, want Node name %q", m[3], wantArch)
	}
	// Node os.release() is a kernel/OS version on every platform the port
	// builds for (e.g. darwin "25.5.0", linux "6.8.0-...", win32 "10.0.22631").
	if !regexp.MustCompile(`^[0-9]`).MatchString(m[2]) {
		t.Fatalf("release token = %q, want a version starting with a digit", m[2])
	}
}
