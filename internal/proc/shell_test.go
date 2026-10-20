package proc

import "testing"

// Beyond pi: WinNormalize and WinJoin stand in for node's path.win32 on any
// OS; these are node's results for the same inputs.
func TestWinNormalizeAndJoinFollowNodePathWin32(t *testing.T) {
	for in, want := range map[string]string{
		`d:/WINNT/System32/BASH.EXE`:           `d:\WINNT\System32\BASH.EXE`,
		`C:\Windows\\System32\.\bash.exe`:      `C:\Windows\System32\bash.exe`,
		`C:\Windows\Temp\..\System32\bash.exe`: `C:\Windows\System32\bash.exe`,
		`C:\..\Windows`:                        `C:\Windows`,
		`D:\WinNT\`:                            `D:\WinNT\`,
		`a\..\..\b`:                            `..\b`,
		``:                                     `.`,
	} {
		if got := WinNormalize(in); got != want {
			t.Errorf("WinNormalize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WinJoin(`D:\WinNT\`, "System32", "bash.exe"); got != `D:\WinNT\System32\bash.exe` {
		t.Errorf("WinJoin = %q", got)
	}
	if got := WinJoin("", `E:\WinNT`, "", "System32"); got != `E:\WinNT\System32` {
		t.Errorf("WinJoin with empty parts = %q", got)
	}
}
