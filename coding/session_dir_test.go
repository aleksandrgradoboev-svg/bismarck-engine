package coding

import (
	"path/filepath"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi SessionManager.create(cwd, sessionDir): the session file goes to the
// named directory, outside the agent directory, and a listing of that
// directory finds it with the header's cwd.
func TestStartSessionInWritesTheSessionToTheNamedDirectory(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(t.TempDir(), "users", "a", "sessions")
	rec, err := StartSessionIn(dir, cwd, &ai.Model{ID: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordMessage(ai.NewUserText("question", 1))
	rec.RecordMessage(&ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "answer"}}, StopReason: ai.StopStop, Timestamp: 2})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(rec.Path()); got != dir {
		t.Fatalf("session file in %s, want %s", got, dir)
	}
	sessions := ListSessions(cwd, dir)
	if len(sessions) != 1 || sessions[0].Path != rec.Path() {
		t.Fatalf("listing = %+v", sessions)
	}
	messages, err := LoadSessionMessages(rec.Path())
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %d, %v", len(messages), err)
	}
	if def, err := StartSessionIn("", cwd, nil); err != nil || filepath.Dir(def.Path()) != DefaultSessionDir(cwd) {
		t.Fatalf("empty sessionDir: %v, %v", def, err)
	}
}
