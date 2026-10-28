package coding

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/uuidv7"
)

// jsonWhitespace is the padding JSON.parse accepts around a value. A session
// line padded with anything else — U+00A0, U+0085, U+FEFF — is malformed to
// pi.
const jsonWhitespace = " \t\n\r"

// CurrentSessionVersion matches pi's session file format version.
const CurrentSessionVersion = 3

// DefaultSessionDir returns the per-cwd session directory under the agent dir,
// using pi's safe-path encoding (--<cwd with separators as dashes>--).
// It returns "" when AgentDir() does — with no home there is nowhere global to
// put sessions, and the old relative fallback wrote them into whatever repo the
// process was run in.
func DefaultSessionDir(cwd string) string {
	agentDir := AgentDir()
	if agentDir == "" {
		return ""
	}
	resolved, _ := filepath.Abs(cwd)
	return filepath.Join(agentDir, "sessions", "--"+encodeCwdSafePath(resolved)+"--")
}

// encodeCwdSafePath mirrors pi's safe-path encoding (session-manager.ts:441):
// strip exactly ONE leading separator (replace(/^[/\\]/, "")), then replace
// every '/'/'\'/':' with '-'.
func encodeCwdSafePath(resolved string) string {
	if len(resolved) > 0 && (resolved[0] == '/' || resolved[0] == '\\') {
		resolved = resolved[1:]
	}
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(resolved)
}

func genID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// uuidv7Now mints a session id with the process-wide generator pi shares
// across packages (pi-ai utils/uuid.ts); see uuidv7.New.
func uuidv7Now() (string, error) { return uuidv7.New() }

// mustUUIDv7 is the panicking convenience form of uuidv7Now, for the one caller
// that cannot yet propagate an error (compaction.go's per-request routing
// session id). Every caller that returns an error uses uuidv7Now instead, and
// so should any new one.
func mustUUIDv7() string { return uuidv7.Must() }

// randomUUIDv4 returns a canonical random (version 4, RFC-4122 variant) UUID —
// node's randomUUID(), which is what pi's session-manager mints.
func randomUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = 0x40 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// genEntryID returns an 8-hex-char entry id for which taken reports false
// (pi's generateId, session-manager.ts:221-228: randomUUID().slice(0,8),
// collision-checked against a `{ has(id) }`, with a full randomUUID() as the
// after-100-collisions fallback). pi slices a v4 UUID (fully random first 8
// chars); genID() yields the same 8-random-hex-char shape, so we reuse it.
func genEntryID(taken func(id string) bool) string {
	for i := 0; i < 100; i++ {
		id := genID()
		if !taken(id) {
			return id
		}
	}
	return randomUUIDv4()
}

// readSessionEntries decodes a session file's JSONL lines into raw entries —
// malformed lines skipped, as pi's parseSessionEntryLine does — and brings them
// to CurrentSessionVersion in memory (pi migrateToCurrentVersion,
// session-manager.ts:281-291). migrated reports whether any migration ran; pi
// rewrites the file whenever it did (_loadEntries → _rewriteFile), which is the
// caller's decision here because the read-only loaders never write.
//
// Numbers are kept as json.Number so a rewrite reproduces every value verbatim
// rather than through float64. A line is one JSON value and nothing else:
// JSON.parse throws on trailing bytes, so a line carrying two entries fused
// together (an unterminated tail plus a later append, issue #8345) is dropped
// whole rather than resurrecting its first entry as an orphan.
func readSessionEntries(data []byte) (entries []map[string]any, migrated bool) {
	for _, line := range readSessionLines(data) {
		entries = append(entries, line.entry)
	}
	return entries, migrateSessionEntries(entries)
}

// sessionLine is one parsed session file line: the decoded entry and the line
// as written.
type sessionLine struct {
	entry map[string]any
	raw   string
}

// readSessionLines parses a session file's JSONL lines as readSessionEntries
// does, without migrating them, keeping each line's text beside its entry.
// Decoding the text again keeps every nested object's key order, which a
// map[string]any loses; the order is data pi keeps (JSON.parse preserves it),
// such as a system message's sections, which render in that order.
func readSessionLines(data []byte) []sessionLine {
	var lines []sessionLine
	for _, line := range strings.Split(string(data), "\n") {
		// pi skips a line blank as JavaScript trims and JSON.parses the rest,
		// untrimmed; the decode's offset check needs JSON's own padding gone,
		// and a line blank only by trim fails JSON.parse just the same.
		line = strings.Trim(line, jsonWhitespace)
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var entry map[string]any
		if dec.Decode(&entry) != nil || entry == nil || dec.InputOffset() != int64(len(line)) {
			continue
		}
		lines = append(lines, sessionLine{entry: entry, raw: line})
	}
	return lines
}

// sessionHeader returns the first session entry, pi's
// `entries.find((e) => e.type === "session")`, or nil when the file has none.
func sessionHeader(entries []map[string]any) map[string]any {
	for _, e := range entries {
		if e["type"] == "session" {
			return e
		}
	}
	return nil
}

// migrateSessionEntries runs pi's migrations in place and reports whether any
// applied. As in pi's _loadEntries the migration is gated on a session header
// (a headerless entry list is not a session file and is left alone), the
// header's missing version means 1 (`header?.version ?? 1`: v1 files predate
// the field), and files at or beyond CurrentSessionVersion are untouched.
func migrateSessionEntries(entries []map[string]any) bool {
	header := sessionHeader(entries)
	if header == nil {
		return false
	}
	version := 1
	if v, ok := header["version"].(json.Number); ok {
		if n, err := v.Int64(); err == nil {
			version = int(n)
		}
	}
	if version >= CurrentSessionVersion {
		return false
	}
	if version < 2 {
		migrateSessionV1ToV2(entries)
	}
	if version < 3 {
		migrateSessionV2ToV3(entries)
	}
	return true
}

// migrateSessionV1ToV2 ports pi's migrateV1ToV2 (session-manager.ts:231-257):
// v1 files are linear and carry no id/parentId, so every entry gets a fresh
// entry id chained to the one before it, and a compaction's firstKeptEntryIndex
// — an index into the file including the header — becomes the id of that entry.
func migrateSessionV1ToV2(entries []map[string]any) {
	taken := map[string]bool{}
	var prevID any // nil: the first entry's parentId is null
	for _, e := range entries {
		if e["type"] == "session" {
			e["version"] = 2
			continue
		}
		id := genEntryID(func(id string) bool { return taken[id] })
		taken[id] = true
		e["id"] = id
		e["parentId"] = prevID
		prevID = id

		if e["type"] != "compaction" {
			continue
		}
		idx, ok := e["firstKeptEntryIndex"].(json.Number)
		if !ok {
			continue
		}
		if i, err := idx.Int64(); err == nil && i >= 0 && i < int64(len(entries)) {
			if target := entries[i]; target["type"] != "session" {
				if targetID, ok := target["id"].(string); ok {
					e["firstKeptEntryId"] = targetID
				}
			}
		}
		delete(e, "firstKeptEntryIndex")
	}
}

// migrateSessionV2ToV3 ports pi's migrateV2ToV3 (session-manager.ts:260-275):
// the hookMessage role was renamed custom.
func migrateSessionV2ToV3(entries []map[string]any) {
	for _, e := range entries {
		if e["type"] == "session" {
			e["version"] = 3
			continue
		}
		if e["type"] != "message" {
			continue
		}
		if msg, ok := e["message"].(map[string]any); ok && msg["role"] == "hookMessage" {
			msg["role"] = "custom"
		}
	}
}

// rewriteSessionFile replaces path's contents with entries, one JSON object per
// line (pi _rewriteFile, session-manager.ts:990-1000: open "w", write every
// entry). It runs only after a migration, so the file is already known to be a
// pi session.
func rewriteSessionFile(path string, entries []map[string]any) error {
	var buf strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("cannot re-encode migrated session entry for %s: %w", path, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("cannot rewrite migrated session file: %w (check that %s is writable, or resume a different session)", err, path)
	}
	return nil
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// SessionInfo summarizes a stored session file.
type SessionInfo struct {
	Path      string
	ID        string
	Cwd       string
	Timestamp string
	Messages  int
}

// SessionRecorder appends an agent transcript to a JSONL session file, matching
// pi's append-only format (header + linear message/model/thinking entries).
//
// A new session's file is created only once the session has a user or
// assistant message (pi _persist, upstream ff72faba2): setup entries alone
// (model, thinking level) stay buffered, so a started-but-unused session
// leaves no file on disk, while the first prompt survives a first turn that
// never completes. The buffered entries are flushed atomically then.
type SessionRecorder struct {
	mu              sync.Mutex
	path            string
	id              string
	lastID          string
	file            *os.File
	byID            map[string]*recordedEntry // every entry, for ids and the branch
	pending         []map[string]any          // entries held back until the file exists
	flushed         bool
	hasConversation bool
	createFn        func() (*os.File, error)
	err             error // see Err
}

// StartSession creates a new session for cwd and buffers the header plus an
// initial model entry. The session file is created lazily on the first recorded
// user or assistant message.
//
// When thinkingLevel is given, a thinking_level_change entry is recorded after
// the model_change, matching pi's createAgentSession for new sessions
// (sdk.ts:362-373: appendModelChange then appendThinkingLevelChange; the
// thinking entry is written even when there is no model). The variadic
// parameter keeps existing callers source-compatible.
func StartSession(cwd string, model *ai.Model, thinkingLevel ...string) (*SessionRecorder, error) {
	return StartSessionIn("", cwd, model, thinkingLevel...)
}

// StartSessionIn is StartSession with the session file in sessionDir, pi's
// SessionManager.create(cwd, sessionDir): a host that keeps sessions apart
// from the agent directory, such as a server storing them per user, names
// the directory; an empty sessionDir is the default per-cwd directory.
func StartSessionIn(sessionDir, cwd string, model *ai.Model, thinkingLevel ...string) (*SessionRecorder, error) {
	dir := DefaultSessionDir(cwd)
	if sessionDir != "" {
		dir = normalizePath(sessionDir)
	}
	if dir == "" {
		return nil, fmt.Errorf("cannot locate the agent directory: no home directory is set, so there is nowhere to store sessions. Set HOME (or USERPROFILE on Windows), or pass an explicit session directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id, err := uuidv7Now()
	if err != nil {
		return nil, err
	}
	ts := isoNow()
	fileTS := strings.NewReplacer(":", "-", ".", "-").Replace(ts)
	path := filepath.Join(dir, fileTS+"_"+id+".jsonl")
	resolved, _ := filepath.Abs(cwd)
	r := &SessionRecorder{
		path: path,
		id:   id,
		byID: map[string]*recordedEntry{},
		createFn: func() (*os.File, error) {
			// pi opens the first flush with "wx" (O_EXCL): never clobber an
			// existing file (session-manager.ts _persist).
			return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
		},
	}
	r.persist(map[string]any{
		"type": "session", "version": CurrentSessionVersion, "id": id, "timestamp": ts, "cwd": resolved,
	})
	if model != nil {
		r.appendEntry(map[string]any{
			"type": "model_change", "provider": model.Provider, "modelId": model.ID,
		})
	}
	if len(thinkingLevel) > 0 && thinkingLevel[0] != "" {
		r.appendEntry(map[string]any{
			"type": "thinking_level_change", "thinkingLevel": thinkingLevel[0],
		})
	}
	return r, nil
}

// ResumeSession opens an existing session file for appending, porting pi's
// SessionManager.setSessionFile resume semantics (session-manager.ts:898-954):
// entries load from the file and are migrated to the current version (a v1/v2
// file is rewritten in place, as pi's _loadEntries does), the leaf is the
// file's last entry (new entries branch from it), and the manager is marked
// flushed so every subsequent entry appends to the file immediately (no
// withhold-until-conversation buffering).
func ResumeSession(path string) (*SessionRecorder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := readSessionLines(data)
	entries := make([]map[string]any, len(lines))
	for i, line := range lines {
		entries[i] = line.entry
	}
	migrated := migrateSessionEntries(entries)
	var id, lastID string
	byID := map[string]*recordedEntry{}
	for i, e := range entries {
		entryID, _ := e["id"].(string)
		if e["type"] == "session" {
			if id == "" { // pi: entries.find(e => e.type === "session")
				id = entryID
			}
			continue
		}
		if entryID != "" {
			raw := ""
			if !migrated {
				raw = lines[i].raw
			}
			byID[entryID] = recordEntry(e, raw)
			lastID = entryID
		}
	}
	if id == "" {
		return nil, fmt.Errorf("not a pi session file (no session header): %s", path)
	}
	// Both writes below happen only AFTER the session header validates — pi
	// never writes to a file that is not a pi session.
	if migrated {
		if err := rewriteSessionFile(path, entries); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// Repair an unterminated tail (pi 0b5ee5d8b, issue #8345): a file whose last
	// line has no trailing newline would have the next appended entry fused onto
	// it, losing both. pi repairs inside its shared loader, but only after the
	// header check; the port keeps that ordering. The read-only loaders
	// (readSessionInfo, LoadSessionMessages, LoadSessionTree) split on "\n" and
	// already tolerate an unterminated tail, so appending is the only path where
	// the corruption can occur. A rewritten file is newline-terminated already.
	if !migrated && len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &SessionRecorder{
		path:    path,
		id:      id,
		lastID:  lastID,
		file:    f,
		byID:    byID,
		flushed: true, // resumed sessions append immediately
	}, nil
}

// Path returns the session file path.
func (r *SessionRecorder) Path() string { return r.path }

// ID returns the session id.
func (r *SessionRecorder) ID() string { return r.id }

func writeLine(f *os.File, entry map[string]any) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// persist writes entry per pi's _persist(entry): until the session has a user
// or assistant message (pi _hasConversation) entries are held back and the
// file is not even created; the entry that completes the conversation flushes
// them all atomically, and every later entry is appended on its own. A failed
// create or write is returned, with the file closed if the flush opened it.
func (r *SessionRecorder) persist(entry map[string]any) error {
	if r.flushed {
		if r.file != nil {
			return writeLine(r.file, entry)
		}
		return nil
	}
	r.pending = append(r.pending, entry)
	if !r.hasConversation {
		return nil
	}
	f, err := r.createFn()
	if err != nil {
		return err
	}
	for _, e := range r.pending {
		if err := writeLine(f, e); err != nil {
			f.Close()
			return err
		}
	}
	r.file = f
	r.pending = nil
	r.flushed = true
	return nil
}

// appendEntry adds entry as the child of the leaf and makes it the leaf, and
// returns its id. An entry that fails to persist is not kept: the leaf stays,
// so later entries chain to the last entry actually written instead of to a
// parent missing from the file, which a resume would stop at, losing all the
// history before it (pi 807a5c05, #11460). It then returns "" and Err reports
// the failure.
func (r *SessionRecorder) appendEntry(entry map[string]any) string {
	id := genEntryID(func(id string) bool { return r.byID[id] != nil })
	entry["id"] = id
	if r.lastID == "" {
		entry["parentId"] = nil
	} else {
		entry["parentId"] = r.lastID
	}
	entry["timestamp"] = isoNow()
	previousLeaf, wasFlushed := r.lastID, r.flushed
	r.byID[id] = recordEntry(entry, "")
	r.lastID = id
	if err := r.persist(entry); err != nil {
		delete(r.byID, id)
		r.lastID = previousLeaf
		if !wasFlushed {
			r.pending = r.pending[:len(r.pending)-1]
		}
		r.err = err
		return ""
	}
	return id
}

// Err is the error of the most recent entry that failed to persist, nil when
// none has: such an entry is not kept, and the call that appended it returned
// "" (pi's SessionManager throws it out of the append instead).
func (r *SessionRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// recordedEntry is what the recorder keeps of an entry: its place in the tree
// and, for a custom entry, its data (pi's SessionManager keeps every entry;
// the recorder needs no more than Branch and CustomEntries read).
type recordedEntry struct {
	parentID   string
	typ        string
	customType string
	data       json.RawMessage // custom entries only; nil when absent
}

// recordEntry reads entry's tree fields. raw, when not empty, is the entry's
// line as written: a custom entry's data is taken from it, keeping its key
// order, which re-encoding the decoded map would sort.
func recordEntry(entry map[string]any, raw string) *recordedEntry {
	e := &recordedEntry{}
	e.parentID, _ = entry["parentId"].(string)
	e.typ, _ = entry["type"].(string)
	if e.typ != "custom" {
		return e
	}
	e.customType, _ = entry["customType"].(string)
	if raw != "" {
		var line struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(raw), &line) == nil {
			e.data = line.Data
		}
		return e
	}
	switch data := entry["data"].(type) {
	case nil:
	case json.RawMessage:
		e.data = data
	default:
		if encoded, err := json.Marshal(data); err == nil {
			e.data = encoded
		}
	}
	return e
}

// AppendCustomEntry appends a custom entry — an extension's own data, which
// stays out of the model's context — as a child of the current leaf and
// returns its id (pi SessionManager.appendCustomEntry). A nil data writes no
// data field, as JSON.stringify drops an undefined one. Like every other entry
// it waits for the session's first user or assistant message before the file
// is created.
func (r *SessionRecorder) AppendCustomEntry(customType string, data any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := map[string]any{"type": "custom", "customType": customType}
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		entry["data"] = json.RawMessage(encoded)
	}
	return r.appendEntry(entry)
}

// CustomEntries returns the data of the customType entries on the current
// branch — the path from the leaf up to the root — in root→leaf order (pi
// getBranch filtered by customType). An entry without data contributes nil.
// After ForkFrom the branch is the one the fork point lies on.
func (r *SessionRecorder) CustomEntries(customType string) []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []json.RawMessage
	// pi walks byId from the leaf; an unknown leaf is an empty branch. The
	// step bound stops a parent cycle in a damaged file, where pi would loop.
	for id, steps := r.lastID, 0; id != "" && steps <= len(r.byID); steps++ {
		e := r.byID[id]
		if e == nil {
			break
		}
		if e.typ == "custom" && e.customType == customType {
			out = append(out, e.data)
		}
		id = e.parentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// LastEntryID returns the id of the most recently written entry (a branch point).
func (r *SessionRecorder) LastEntryID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastID
}

// ForkFrom sets the parent for subsequent entries to entryID, so new entries
// branch off an earlier point in the tree instead of extending the latest leaf.
func (r *SessionRecorder) ForkFrom(entryID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastID = entryID
}

// RecordMessage appends a message entry for an agent transcript message and
// returns its entry id (usable as a fork point), or "" when it could not be
// written (see Err).
func (r *SessionRecorder) RecordMessage(m agent.AgentMessage) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	// pi's _hasConversation reads the kept entries, so a conversation message
	// that is not kept does not count.
	hadConversation := r.hasConversation
	if role := m.MessageRole(); role == ai.RoleUser || role == ai.RoleAssistant {
		r.hasConversation = true
	}
	id := r.appendEntry(map[string]any{"type": "message", "message": json.RawMessage(raw)})
	if id == "" {
		r.hasConversation = hadConversation
	}
	return id
}

// RecordThinkingLevel appends a thinking-level-change entry.
func (r *SessionRecorder) RecordThinkingLevel(level string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendEntry(map[string]any{"type": "thinking_level_change", "thinkingLevel": level})
}

// RecordModelChange appends a model-change entry.
func (r *SessionRecorder) RecordModelChange(provider, modelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendEntry(map[string]any{"type": "model_change", "provider": provider, "modelId": modelID})
}

// Close closes the session file.
func (r *SessionRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// ListSessions returns stored sessions for cwd, newest first (pi
// SessionManager.list). sessionDir "" means the default directory for cwd; in
// an explicit directory that is not the default, only sessions recorded for
// cwd are listed.
func ListSessions(cwd, sessionDir string) []SessionInfo {
	dir, filterCwd, resolvedCwd := sessionListingDir(cwd, sessionDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	// pi orders the directory entries by name DESCENDING before reading any of
	// them and then sorts the sessions with a STABLE sort, so files sharing a
	// timestamp come back newest-name-first (upstream dfbf793b7). os.ReadDir
	// hands entries back ascending, so walk it in reverse.
	//
	// That is byte order, and pi's `b.localeCompare(a)` is not: it uses the
	// host's default ICU collation. Under the root and English-like collations
	// Node normally runs with, the two agree on the `<timestamp>_<uuid>` names
	// the recorder writes. Under Danish or Norwegian collation they do not —
	// "aa" collates as "å", after "z", and a hex uuid can contain "aa" — so pi's
	// own tie order depends on the machine. The port keeps byte order, which is
	// the answer pi gives by default and the only deterministic one. It decides
	// ties alone: sessions whose timestamps are equal to the millisecond.
	var infos []SessionInfo
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, ok := readSessionInfo(filepath.Join(dir, e.Name()))
		if !ok || (filterCwd && !sessionCwdMatches(info.Cwd, resolvedCwd)) {
			continue
		}
		infos = append(infos, info)
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Timestamp > infos[j].Timestamp })
	return infos
}

// readSessionInfo builds a SessionInfo from a session file the way pi's
// buildSessionInfo does (session-manager.ts:688-770): the first parsed entry
// must be the header — a file whose first entry is anything else is not a
// session — and only that header's id, cwd and timestamp are read; a
// session-typed line further down is transcript content. Messages are counted
// after the header. pi lists a header that has no string id with an undefined
// id; the port has always dropped such a file, since nothing can address it.
func readSessionInfo(path string) (SessionInfo, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionInfo{}, false
	}
	info := SessionInfo{Path: path}
	headerSeen := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !headerSeen {
			h, decided := parseSessionHeaderCandidate(line)
			if !decided {
				continue
			}
			if h == nil {
				return SessionInfo{}, false
			}
			info.ID, info.Cwd, info.Timestamp = h.id, h.cwd, h.timestamp
			headerSeen = true
			continue
		}
		var entry struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type == "message" {
			info.Messages++
		}
	}
	return info, headerSeen
}

// sessionFileHeader is the part of a session file's first entry that
// discovery and listing read (pi SessionHeader, session-manager.ts:32). cwd
// and timestamp are "" when the header carries no string value for them.
type sessionFileHeader struct {
	id        string
	cwd       string
	timestamp string
}

const (
	// sessionHeaderReadBufferSize and maxSessionHeaderScanBytes are pi's
	// SESSION_HEADER_READ_BUFFER_SIZE and MAX_SESSION_HEADER_SCAN_BYTES
	// (session-manager.ts:492-494): header discovery reads 4 KiB at a time and
	// gives up after 1 MiB, bounding the work per file while still allowing
	// large cwd and custom metadata fields.
	sessionHeaderReadBufferSize = 4096
	maxSessionHeaderScanBytes   = 1024 * 1024
)

// parseSessionHeaderCandidate inspects one physical line while looking for a
// file's first parsed entry (pi parseSessionHeaderCandidate). decided is
// false for a blank or malformed line, which the scan skips; when decided,
// header is nil for a parsed non-header entry, meaning the file is not a
// session.
//
// pi parses the line with JSON.parse and then tests the value's truthiness, so
// a literal null, false, 0 or "" is skipped like a blank line while any other
// non-object value counts as a parsed non-header entry.
func parseSessionHeaderCandidate(line []byte) (header *sessionFileHeader, decided bool) {
	line = bytes.Trim(line, jsonWhitespace)
	if len(line) == 0 {
		return nil, false
	}
	// Decode as readSessionEntries does: UseNumber keeps a literal outside
	// float64 range as the value JSON.parse would produce (±Infinity, truthy)
	// rather than a malformed line, and the offset guard rejects trailing bytes
	// the way JSON.parse does (a fused line, issue #8345). encoding/json's
	// 10000-level nesting cap has no JSON.parse counterpart; a deeper line is
	// malformed here only.
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.InputOffset() != int64(len(line)) {
		return nil, false
	}
	switch v {
	case nil, false, "":
		return nil, false
	}
	if n, ok := v.(json.Number); ok {
		// JS falsiness for numbers: 0, -0 and 0e5 are all 0; an out-of-range
		// literal parses to ±Inf with ErrRange and is truthy.
		if f, err := strconv.ParseFloat(string(n), 64); err == nil && f == 0 {
			return nil, false
		}
		return nil, true
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, true
	}
	id, ok := obj["id"].(string)
	if obj["type"] != "session" || !ok {
		return nil, true
	}
	h := &sessionFileHeader{id: id}
	if cwd, ok := obj["cwd"].(string); ok {
		h.cwd = cwd
	}
	if ts, ok := obj["timestamp"].(string); ok {
		h.timestamp = ts
	}
	return h, true
}

// readSessionHeader returns the header of the session file at path, reading
// only as far as the first parsed entry (pi readSessionHeader). It returns
// (nil, nil) when that entry is not a session header, and an error when the
// file cannot be read or nothing is decided within maxSessionHeaderScanBytes.
func readSessionHeader(path string) (*sessionFileHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := &io.LimitedReader{R: f, N: maxSessionHeaderScanBytes}
	br := bufio.NewReaderSize(limited, sessionHeaderReadBufferSize)
	for {
		line, err := br.ReadBytes('\n')
		if err == nil {
			if h, decided := parseSessionHeaderCandidate(line[:len(line)-1]); decided {
				return h, nil
			}
			continue
		}
		if err != io.EOF {
			return nil, err
		}
		// EOF from the limited reader: either the file ended or the scan limit
		// did. pi probes one more byte so a final header that ends exactly at
		// the limit without a newline is still allowed; any further byte means
		// the header exceeds the bounded scan.
		if limited.N == 0 {
			var probe [1]byte
			n, perr := f.Read(probe[:])
			if n > 0 {
				return nil, fmt.Errorf("session header exceeds %d-byte scan limit: %s", maxSessionHeaderScanBytes, path)
			}
			if perr != nil && perr != io.EOF {
				return nil, perr
			}
		}
		h, _ := parseSessionHeaderCandidate(line)
		return h, nil
	}
}

// readSessionHeaderForDiscovery is readSessionHeader for directory scans (pi
// readSessionHeaderForDiscovery): discovery is best-effort, so an unreadable
// or oversized file is simply not a session, and one corrupt file must not
// hide the others.
func readSessionHeaderForDiscovery(path string) *sessionFileHeader {
	h, err := readSessionHeader(path)
	if err != nil {
		return nil
	}
	return h
}

// sessionListingDir resolves the directory a discovery call scans and whether
// its matches are filtered to cwd — the prologue pi's SessionManager.list and
// findById share. sessionDir "" means the default per-cwd directory. An
// explicit directory that is not the default holds sessions from many working
// directories, so matches are filtered to those whose header cwd is cwd.
func sessionListingDir(cwd, sessionDir string) (dir string, filterCwd bool, resolvedCwd string) {
	resolvedCwd, _ = filepath.Abs(cwd)
	defaultDir := DefaultSessionDir(cwd)
	if sessionDir == "" {
		return defaultDir, false, resolvedCwd
	}
	return sessionDir, sessionDir != defaultDir, resolvedCwd
}

// sessionCwdMatches reports whether a header cwd names resolvedCwd (pi
// sessionCwdMatches). Paths are compared textually after resolution, so a
// working directory reached through a symlink matches only the spelling it
// was recorded under.
func sessionCwdMatches(cwd, resolvedCwd string) bool {
	if cwd == "" {
		return false
	}
	abs, err := filepath.Abs(cwd)
	return err == nil && abs == resolvedCwd
}

// FindSessionByID returns the path of the stored session whose header id is
// id, reading headers only — never transcript bodies (pi
// SessionManager.findById). sessionDir "" means the default directory for
// cwd; in an explicit directory that is not the default, only a session
// recorded for cwd matches. Discovery is best-effort like ListSessions: an
// unreadable directory or file yields no match rather than an error.
func FindSessionByID(cwd, id, sessionDir string) (string, bool) {
	dir, filterCwd, resolvedCwd := sessionListingDir(cwd, sessionDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		h := readSessionHeaderForDiscovery(path)
		if h == nil || h.id != id {
			continue
		}
		if filterCwd && !sessionCwdMatches(h.cwd, resolvedCwd) {
			continue
		}
		return path, true
	}
	return "", false
}

// LoadSessionMessages reconstructs the LLM message transcript from a session
// file for resume. It routes through SessionTree.BuildContext so compacted,
// branched, and custom-message sessions resume identically to pi (emitting the
// compaction summary in place of the pre-compaction turns) rather than naively
// concatenating every message entry. The messages alone do not carry the
// compaction a later one extends: to resume with it, pass the tree's
// BuildProjection to Session.LoadBranch.
func LoadSessionMessages(path string) ([]agent.AgentMessage, error) {
	tree, err := LoadSessionTree(path)
	if err != nil {
		return nil, err
	}
	return tree.BuildContext().Messages, nil
}

// LatestSession returns the session `--continue` reopens: the most recently
// WRITTEN one for cwd, if any. sessionDir is as for ListSessions.
//
// This is pi's findMostRecentSession, the discovery step of
// SessionManager.continueRecent — a different function from list(), ordering
// by file mtime rather than by the header timestamp, and returning the first
// header that parses and matches instead of the whole listing. Upstream
// dd01f5b24 made the header read lazy: every candidate is stat'd up front and
// headers are read only until one matches, so a large session directory no
// longer pays a full header scan per `--continue`.
//
// The header timestamp is written once, at creation, so answering this from
// ListSessions — as the port used to — ranked a freshly created session above
// one that had been resumed and written to for hours.
func LatestSession(cwd, sessionDir string) (SessionInfo, bool) {
	dir, filterCwd, resolvedCwd := sessionListingDir(cwd, sessionDir)
	// findMostRecentSession gates its filter on TRUTHINESS —
	// `cwd ? resolvePath(cwd) : undefined` — so an empty cwd means no cwd filter
	// at all. list() has no such gate and resolves "" to the process cwd, which
	// is why this lives here and not in the shared prologue.
	if cwd == "" {
		filterCwd = false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return SessionInfo{}, false
	}
	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Stat(path)
		if err != nil {
			// pi stats every candidate inside the same try that wraps the whole
			// scan, so one unreadable entry — a file deleted between readdir and
			// stat — abandons discovery rather than being skipped.
			return SessionInfo{}, false
		}
		candidates = append(candidates, candidate{path: path, mtime: fi.ModTime()})
	}
	// Newest file first, then read headers only until one matches. Ties keep
	// readdir order, which pi leaves to the OS and Go fixes at ascending name.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].mtime.After(candidates[j].mtime) })

	for _, c := range candidates {
		h := readSessionHeaderForDiscovery(c.path)
		if h == nil || (filterCwd && !sessionCwdMatches(h.cwd, resolvedCwd)) {
			continue
		}
		// pi stops here with the path. The port's callers want the listing shape,
		// so fill it from a full read — and fall back to the discovery header if
		// that read cannot complete, rather than skipping a file pi would return.
		if info, ok := readSessionInfo(c.path); ok {
			return info, true
		}
		return SessionInfo{Path: c.path, ID: h.id, Cwd: h.cwd, Timestamp: h.timestamp}, true
	}
	return SessionInfo{}, false
}
