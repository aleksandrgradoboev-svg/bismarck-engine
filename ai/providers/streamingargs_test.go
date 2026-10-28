package providers

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// Ported from pi packages/ai/test/streaming-json.test.ts (upstream f715a15c):
// the ToolCallArgumentsStream cases and "matches today's parseStreamingJson
// at every prefix of compact documents", which lives here with the engine's
// parseStreamingJSON. n/a: "is collected with its snapshots once the stream
// is dropped" (a Go value is collected once unreachable). Added: the map form
// of the partial arguments, converted once per completed container.

func TestToolCallArgumentsShowPartialArgumentsAndFinishWithOneFullParse(t *testing.T) {
	args := NewToolCallArgumentsStream()
	if got, _ := args.Append("  "); len(got) != 0 {
		t.Fatalf("whitespace = %v", got)
	}
	got, _ := args.Append(`{"path":"a.ts","content":"x`)
	if want := map[string]any{"path": "a.ts", "content": "x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("partial = %v, want %v", got, want)
	}
	before, _ := args.Append("y")
	if again, _ := args.Append(""); reflect.ValueOf(again).Pointer() != reflect.ValueOf(before).Pointer() {
		t.Fatal("an empty piece gave new arguments")
	}
	if args.Text() != `  {"path":"a.ts","content":"xy` {
		t.Fatalf("text = %q", args.Text())
	}
	m, order, errText := args.Finish()
	wm, worder, werr := parseToolCallArguments(`  {"path":"a.ts","content":"xy`)
	if !reflect.DeepEqual(m, wm) || !reflect.DeepEqual(order, worder) || errText != werr {
		t.Fatalf("finish = %v %v %q", m, order, errText)
	}
	args.Append(`"}`)
	m, _, errText = args.Finish()
	if want := map[string]any{"path": "a.ts", "content": "xy"}; !reflect.DeepEqual(m, want) || errText != "" {
		t.Fatalf("finish = %v %q", m, errText)
	}
}

func TestToolCallArgumentsShowNonObjectRootsAsNoArgumentsYet(t *testing.T) {
	args := NewToolCallArgumentsStream()
	if got, order := args.Append("[1,2"); len(got) != 0 || order != nil {
		t.Fatalf("array root = %v %v", got, order)
	}
	args.Append("]")
	if _, _, errText := args.Finish(); !strings.HasPrefix(errText, "Tool arguments must be a JSON object") {
		t.Fatalf("error = %q", errText)
	}
}

func TestToolCallArgumentsReplaceTheTextWithTheProvidersCompleteText(t *testing.T) {
	args := NewToolCallArgumentsStream()
	args.Append(`{"a":"x`)
	delta, continued, got, _ := args.Replace(`{"a":"xyz"}`)
	if delta != `yz"}` || !continued || !reflect.DeepEqual(got, map[string]any{"a": "xyz"}) {
		t.Fatalf("continued = %q %v %v", delta, continued, got)
	}
	delta, continued, got, _ = args.Replace(`{"b":2}`)
	if delta != "" || continued || !reflect.DeepEqual(got, map[string]any{"b": 2.0}) {
		t.Fatalf("restarted = %q %v %v", delta, continued, got)
	}
	if args.Text() != `{"b":2}` {
		t.Fatalf("text = %q", args.Text())
	}
}

// capturedDocuments are the documents of ai's StreamingJsonParser capture:
// pi's generator, 40 spaced and 40 compact, then the malformed list.
func capturedDocuments(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("../testdata/streaming-json/capture-f715a15c.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Cases []struct {
			Text string `json:"text"`
			Size int    `json:"size"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, c := range capture.Cases {
		if c.Size == 1 {
			docs = append(docs, c.Text)
		}
	}
	return docs
}

func TestToolCallArgumentsMatchParseStreamingJSONAtEveryPrefixOfCompactDocuments(t *testing.T) {
	docs := capturedDocuments(t)[40:80]
	for _, text := range docs {
		args := NewToolCallArgumentsStream()
		read := ""
		for _, r := range text {
			read += string(r)
			got, gotOrder := args.Append(string(r))
			// Documented difference: parseStreamingJson trims trailing
			// whitespace (JavaScript's \s, U+2028 included).
			if jstext.IsWhitespace(r) {
				continue
			}
			want, wantOrder := parseStreamingJSON(read)
			gotJSON, _ := json.Marshal(gotOrder)
			wantJSON, _ := json.Marshal(wantOrder)
			if !reflect.DeepEqual(got, want) || string(gotJSON) != string(wantJSON) {
				t.Fatalf("%q:\n got  %v %s\n want %v %s", read, got, gotJSON, want, wantJSON)
			}
		}
	}
}

// Added: every partial map is the plain form of its ordered twin, and a
// completed container keeps the map it was converted to once.
func TestToolCallArgumentsConvertEachCompletedContainerOnce(t *testing.T) {
	for _, text := range capturedDocuments(t) {
		args := NewToolCallArgumentsStream()
		for _, r := range text {
			got, order := args.Append(string(r))
			if order == nil {
				if len(got) != 0 {
					t.Fatalf("%q: map %v without an object", text, got)
				}
				continue
			}
			if !reflect.DeepEqual(got, order.Plain()) {
				t.Fatalf("%q: map %v, plain %v", text, got, order.Plain())
			}
		}
	}
	args := NewToolCallArgumentsStream()
	first, _ := args.Append(`{"done":{"a":[1,2]},"open":"x`)
	second, _ := args.Append(`y`)
	if reflect.ValueOf(first["done"]).Pointer() != reflect.ValueOf(second["done"]).Pointer() {
		t.Fatal("a completed container was converted again")
	}
	if second["open"] != "xy" || first["open"] != "x" {
		t.Fatalf("open strings = %v, %v", first["open"], second["open"])
	}
}
