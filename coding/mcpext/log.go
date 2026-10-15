package mcpext

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Log messages MCP servers send with notifications/message, appended to one
// file (pi extensions/mcp/log.ts). Several processes may write to the same
// file, so every message is one append. The file is rotated to <path>.1 once
// it grows past MaxLogBytes.

// MaxLogBytes is the size past which the log file is rotated.
const MaxLogBytes = 5 * 1024 * 1024

var lineBreak = regexp.MustCompile(`\r?\n`)

// FormatLogMessage formats one notifications/message from server as a log
// line; continuation lines are indented.
func FormatLogMessage(server string, params json.RawMessage, now time.Time) string {
	parsed, _ := jstext.Parse(params)
	message, ok := parsed.(map[string]any)
	if !ok {
		message = map[string]any{"data": parsed}
	}
	level, _ := message["level"].(string)
	if level == "" {
		level = "info"
	}
	logger := ""
	if l, _ := message["logger"].(string); l != "" {
		logger = " " + l + ":"
	}
	var text string
	if s, ok := message["data"].(string); ok {
		text = s
	} else if s, err := jstext.Stringify(message["data"]); err == nil {
		text = s
	}
	text = lineBreak.ReplaceAllString(text, "\n    ")
	return now.UTC().Format("2006-01-02T15:04:05.000Z") + " [" + server + "] " + level + logger + " " + text + "\n"
}

// ServerLog appends server log messages to one file. Write errors are
// ignored: logging must not break tools.
type ServerLog struct {
	Path string
	mu   sync.Mutex
	size int64
	init bool
}

// NewServerLog returns a log appending to path.
func NewServerLog(path string) *ServerLog { return &ServerLog{Path: path} }

// Write appends one message; it is ConnectionOptions.Log's shape.
func (l *ServerLog) Write(server string, params json.RawMessage) {
	line := FormatLogMessage(server, params, time.Now())
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.init {
		if os.MkdirAll(filepath.Dir(l.Path), 0o755) != nil {
			return
		}
		l.size, l.init = l.currentSize(), true
	}
	if l.size > MaxLogBytes {
		// Another process may have rotated it already; check before renaming.
		if l.currentSize() > MaxLogBytes {
			_ = os.Rename(l.Path, l.Path+".1")
		}
		l.size = l.currentSize()
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	n, _ := f.WriteString(line)
	_ = f.Close()
	l.size += int64(n)
}

func (l *ServerLog) currentSize() int64 {
	info, err := os.Stat(l.Path)
	if err != nil {
		return 0
	}
	return info.Size()
}
