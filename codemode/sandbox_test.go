package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ported from pi v1.2.0 packages/codemode/test/sandbox.test.ts. pi runs every
// test against three backends; Go has one, the goroutine launcher, which plays
// pi's worker backend.

func newSandbox(t *testing.T, opts Options) *Sandbox {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func exec(t *testing.T, s *Sandbox, code string, opts ...ExecuteOptions) Result {
	t.Helper()
	var o ExecuteOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	r, err := s.Execute(context.Background(), code, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// canon re-encodes JSON so that comparisons ignore spacing.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	if raw == nil {
		return "undefined"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func wantValue(t *testing.T, r Result, want string) {
	t.Helper()
	if !r.OK {
		t.Fatalf("execution failed: %+v (output %+v)", r.Error, r.Output)
	}
	if got, w := canon(t, r.Value), want; w != "undefined" {
		w = canon(t, []byte(want))
		if got != w {
			t.Fatalf("value = %s, want %s", got, w)
		}
	} else if got != "undefined" {
		t.Fatalf("value = %s, want undefined", got)
	}
}

func wantError(t *testing.T, r Result, kind ErrorKind) *Error {
	t.Helper()
	if r.OK {
		t.Fatalf("execution succeeded with %s, want a %s error", r.Value, kind)
	}
	if r.Error.Kind != kind {
		t.Fatalf("error = %+v, want kind %s", r.Error, kind)
	}
	return r.Error
}

func echoTool() Tool {
	return Tool{Name: "echo", Execute: func(_ context.Context, args json.RawMessage) (any, error) {
		if args == nil {
			return nil, nil
		}
		return args, nil
	}}
}

const (
	pngB64  = "iVBORw0KGgo="
	jpegB64 = "/9j/4A=="
	gifB64  = "R0lGODlh"
	webpB64 = "UklGRgAAAABXRUJQ"
)

func TestReturnValue(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, "return { a: 1, b: [true, 'x'] }")
	wantValue(t, r, `{"a":1,"b":[true,"x"]}`)
	if len(r.Output) != 0 || len(r.Calls) != 0 {
		t.Fatalf("output %v calls %v", r.Output, r.Calls)
	}
	wantValue(t, exec(t, s, "return 'plain'"), `"plain"`)
	wantValue(t, exec(t, s, ""), "undefined")
}

func TestTopLevelAwait(t *testing.T) {
	s := newSandbox(t, Options{})
	wantValue(t, exec(t, s, "const x = await Promise.resolve(41); return x + 1"), "42")
}

func TestOutputInOrder(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, fmt.Sprintf(`
		console.log("hello", 1, { a: 1 });
		text({ json: true });
		text(undefined);
		text(7);
		image("data:image/png;base64,%[1]s");
		image({ image_url: "data:image/jpeg;base64,%[2]s" });
		image({ type: "image", data: "%[3]s", mimeType: "image/gif" });
		image("data:image/png;base64,%[4]s");
		image({ type: "image", data: "%[1]s" });
		console.error(new Error("bad"));
		return null;
	`, pngB64, jpegB64, gifB64, webpB64))
	wantValue(t, r, "null")
	want := []OutputItem{
		{Type: "text", Text: `hello 1 {"a":1}`, Console: true},
		{Type: "text", Text: `{"json":true}`},
		{Type: "text", Text: "undefined"},
		{Type: "text", Text: "7"},
		{Type: "image", Data: pngB64, MimeType: "image/png"},
		{Type: "image", Data: jpegB64, MimeType: "image/jpeg"},
		{Type: "image", Data: gifB64, MimeType: "image/gif"},
		// The MIME type comes from the data, not from the declared type.
		{Type: "image", Data: webpB64, MimeType: "image/webp"},
		{Type: "image", Data: pngB64, MimeType: "image/png"},
	}
	if len(r.Output) != len(want)+1 {
		t.Fatalf("output %+v", r.Output)
	}
	for i, w := range want {
		if r.Output[i] != w {
			t.Errorf("output[%d] = %+v, want %+v", i, r.Output[i], w)
		}
	}
	last := r.Output[len(r.Output)-1]
	if !last.Console || !strings.HasPrefix(last.Text, "Error: bad") {
		t.Errorf("last output %+v", last)
	}
}

func TestInvalidTextAndImageArguments(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, `
		const errors = [];
		const circular = {};
		circular.self = circular;
		for (const run of [
			() => text(circular),
			() => image(""),
			() => image("https://example.com/a.png"),
			() => image("data:image/png,raw"),
			() => image({ type: "text", text: "x" }),
			() => image({ type: "image", data: "" }),
			() => image(42),
			() => image("data:image/png;base64,AAAA!"),
			() => image("data:image/png;base64,AAAAA"),
			() => image("data:image/png;base64,AA=A"),
			() => image("data:image/png;base64,"),
			() => image("data:image/png;base64,AAAA\n[Output truncated]"),
			() => image({ type: "image", data: "AAAA!", mimeType: "image/png" }),
			() => image("data:image/png;base64,AAAA"),
			() => image("data:image/png;base64,QUJD"),
			() => image("data:image/jpeg;base64,/9j/9w=="),
		]) {
			try { run(); errors.push("no error"); } catch (error) { errors.push(error.name + ": " + error.message); }
		}
		return errors;
	`)
	if !r.OK || len(r.Output) != 0 {
		t.Fatalf("%+v", r)
	}
	var errs []string
	json.Unmarshal(r.Value, &errs)
	if !regexp.MustCompile(`^TypeError: .*circular`).MatchString(errs[0]) {
		t.Errorf("errors[0] = %q", errs[0])
	}
	expects := `TypeError: image expects a non-empty image URL string, an object with image_url, or an image block { type: "image", data, mimeType }`
	want := []string{
		expects,
		"TypeError: remote image URLs are not supported in tool outputs. Pass a base64 data URI instead",
		"TypeError: invalid image output. Pass a base64 data URI instead",
		`TypeError: image only accepts image blocks, got "text"`,
		"TypeError: image expected image block data",
		expects,
	}
	for range 6 {
		want = append(want, "TypeError: invalid image output. The image data is not valid base64 (truncated or corrupted?)")
	}
	for range 3 {
		want = append(want, "TypeError: invalid image output. The image data is not a PNG, JPEG, GIF, or WebP image")
	}
	if got := strings.Join(errs[1:], "\n"); got != strings.Join(want, "\n") {
		t.Errorf("errors:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestWrappedAndLargeImageData(t *testing.T) {
	s := newSandbox(t, Options{})
	large := "iVBORw0KGgoA" + strings.Repeat("QUJD", 256*1024)
	r := exec(t, s, `image("data:image/png;base64,iVBORw0K\r\nGgo=\n"); image("data:image/png;base64,`+large+`");`)
	if !r.OK || len(r.Output) != 2 || r.Output[0].Data != pngB64 || r.Output[1].Data != large {
		t.Fatalf("ok=%v error=%+v outputs=%d", r.OK, r.Error, len(r.Output))
	}
}

func TestExit(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool()}})
	r := exec(t, s, `
		text("before");
		store("k", 1);
		await tools.echo(1);
		try { exit(); } catch {}
		text("after");
		return "unreachable";
	`)
	wantValue(t, r, "undefined")
	if len(r.Output) != 1 || r.Output[0].Text != "before" {
		t.Fatalf("output %+v", r.Output)
	}
	if canon(t, r.StoreWrites.Set["k"]) != "1" || len(r.StoreWrites.Delete) != 0 {
		t.Fatalf("writes %+v", r.StoreWrites)
	}
}

func TestOutputBeforeFailure(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, "text(\"partial\");\nthrow new Error(\"boom\")")
	wantError(t, r, ErrorScript)
	if len(r.Output) != 1 || r.Output[0].Text != "partial" {
		t.Fatalf("output %+v", r.Output)
	}
}

func TestErrorsCarryScriptLines(t *testing.T) {
	s := newSandbox(t, Options{})
	e := wantError(t, exec(t, s, "const a = 1;\nconst b = ;\nreturn a"), ErrorScript)
	if e.Name != "SyntaxError" || !strings.Contains(e.Stack, "codemode.js:2") {
		t.Fatalf("syntax error %+v", e)
	}
	e = wantError(t, exec(t, s, "const a = 1;\nthrow new TypeError('boom ' + a)"), ErrorScript)
	if e.Name != "TypeError" || e.Message != "boom 1" || !strings.Contains(e.Stack, "codemode.js:2") {
		t.Fatalf("thrown error %+v", e)
	}
}

func TestStacksLikeV8WithoutPreludeFrames(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, "console.log(new Error('inner'));\nthrow new RangeError('outer')")
	e := wantError(t, r, ErrorScript)
	if !regexp.MustCompile(`^RangeError: outer\n {4}at .*codemode\.js:2`).MatchString(e.Stack) || strings.Contains(e.Stack, "codemode-prelude.js") {
		t.Fatalf("stack %q", e.Stack)
	}
	if !regexp.MustCompile(`^Error: inner\n {4}at .*codemode\.js:1`).MatchString(r.Output[0].Text) {
		t.Fatalf("output %q", r.Output[0].Text)
	}
	if strings.Contains(fmt.Sprint(r.Output), "codemode-prelude.js") {
		t.Fatal("prelude frames in output")
	}
}

func TestNonErrorThrowsAndBadReturns(t *testing.T) {
	s := newSandbox(t, Options{})
	if e := wantError(t, exec(t, s, "throw { code: 7 }"), ErrorScript); e.Message != `{"code":7}` {
		t.Fatalf("%+v", e)
	}
	if e := wantError(t, exec(t, s, "return 10n"), ErrorScript); e.Name != "TypeError" {
		t.Fatalf("%+v", e)
	}
}

func TestToolsRecordCalls(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	s := newSandbox(t, Options{Tools: []Tool{{Name: "add", Execute: func(_ context.Context, args json.RawMessage) (any, error) {
		mu.Lock()
		seen = append(seen, string(args))
		mu.Unlock()
		var in struct{ A, B int }
		json.Unmarshal(args, &in)
		return map[string]int{"sum": in.A + in.B}, nil
	}}}})
	r := exec(t, s, `
		const first = await tools.add({ a: 1, b: 2 });
		const second = await tools.add({ a: first.sum, b: 10 });
		return second.sum;
	`)
	wantValue(t, r, "13")
	if strings.Join(seen, " ") != `{"a":1,"b":2} {"a":3,"b":10}` {
		t.Fatalf("seen %v", seen)
	}
	if len(r.Calls) != 2 || r.Calls[0].Status != CallOK || r.Calls[1].Status != CallOK || r.Calls[0].Name != "add" {
		t.Fatalf("calls %+v", r.Calls)
	}
}

func TestConcurrentCallsAndToolNames(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool(), {Name: "delay", Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
		time.Sleep(20 * time.Millisecond)
		return args, nil
	}}}})
	r := exec(t, s, `
		const [a, b, c] = await Promise.all([tools.delay(1), tools.delay(2), tools.echo(3)]);
		return { values: [a, b, c], names: Object.keys(tools) };
	`)
	wantValue(t, r, `{"values":[1,2,3],"names":["echo","delay"]}`)
}

func TestNormalizedIdentifiersAndAllTools(t *testing.T) {
	val := func(v string) func(context.Context, json.RawMessage) (any, error) {
		return func(context.Context, json.RawMessage) (any, error) { return v, nil }
	}
	s := newSandbox(t, Options{Tools: []Tool{
		{Name: "my-tool", Description: "Dashes", Execute: val("dash")},
		{Name: "my_tool", Description: "Shadowed", Execute: val("underscore")},
		{Name: "mcp__docs__search", Execute: val("mcp")},
	}})
	r := exec(t, s, `
		try { ALL_TOOLS.push({}); } catch {}
		return {
			all: ALL_TOOLS,
			calls: [await tools.my_tool(), await tools["my-tool"](), await tools.mcp__docs__search()],
		};
	`)
	wantValue(t, r, `{"all":[{"name":"my_tool","description":"Dashes"},{"name":"mcp__docs__search","description":""}],"calls":["dash","dash","mcp"]}`)
}

func TestUndefinedArgumentsAndResults(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{{Name: "noop", Execute: func(_ context.Context, args json.RawMessage) (any, error) {
		if args == nil {
			return nil, nil
		}
		return args, nil
	}}}})
	wantValue(t, exec(t, s, "return [await tools.noop(), await tools.noop(null)]"), "[null,null]")
}

func TestToolErrorsAreCatchable(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{{Name: "fail", Execute: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("tool exploded")
	}}}})
	r := exec(t, s, `
		try {
			await tools.fail();
			return "no error";
		} catch (error) {
			return { isError: error instanceof Error, message: error.message };
		}
	`)
	wantValue(t, r, `{"isError":true,"message":"tool exploded"}`)
	if r.Calls[0].Status != CallError {
		t.Fatalf("calls %+v", r.Calls)
	}
}

func TestToolPanicBecomesAnError(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{{Name: "boom", Execute: func(context.Context, json.RawMessage) (any, error) {
		panic("kaboom")
	}}}})
	r := exec(t, s, "try { await tools.boom() } catch (e) { return e.message }")
	wantValue(t, r, `"tool \"boom\" panicked: kaboom"`)
}

func TestUnknownTool(t *testing.T) {
	s := newSandbox(t, Options{})
	if e := wantError(t, exec(t, s, "return await tools.missing()"), ErrorScript); e.Name != "TypeError" {
		t.Fatalf("%+v", e)
	}
}

func TestUnawaitedCallsAreCancelled(t *testing.T) {
	aborted := make(chan struct{})
	s := newSandbox(t, Options{Tools: []Tool{{Name: "slow", Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		close(aborted)
		return nil, ctx.Err()
	}}}})
	r := exec(t, s, "tools.slow(); return 'early'")
	wantValue(t, r, `"early"`)
	if len(r.Calls) != 1 || r.Calls[0].Status != CallCancelled {
		t.Fatalf("calls %+v", r.Calls)
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool's context was not canceled")
	}
}

func TestCloseMatchesForMissingTools(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool(), {Name: "web-search", Execute: func(context.Context, json.RawMessage) (any, error) { return "", nil }}}})
	attempt := func(expr string) string {
		r := exec(t, s, "return "+expr+";")
		if r.OK {
			return string(r.Value)
		}
		return r.Error.Message
	}
	if got := attempt("tools.Echo"); got != `tools.Echo does not exist. Did you mean tools.echo? ALL_TOOLS lists every tool; searchTools(query) finds tools by topic. Check for a member with "Echo" in tools.` {
		t.Errorf("got %q", got)
	}
	if got := attempt("tools.websearch"); !strings.Contains(got, "Did you mean tools.web_search?") {
		t.Errorf("got %q", got)
	}
	if got := attempt("tools.nothing"); !strings.Contains(got, "Available: echo, web_search.") {
		t.Errorf("got %q", got)
	}
	if got := attempt("['echo' in tools, 'nothing' in tools, String(tools.toString), JSON.stringify(tools)]"); canon(t, []byte(got)) != `[true,false,"undefined","{}"]` {
		t.Errorf("got %q", got)
	}
}

func TestRegisterAndUnregister(t *testing.T) {
	s := newSandbox(t, Options{})
	if err := s.RegisterTool(echoTool()); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTool(echoTool()); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate: %v", err)
	}
	wantValue(t, exec(t, s, "return await tools.echo('a')"), `"a"`)
	if !s.UnregisterTool("echo") {
		t.Fatal("unregister")
	}
	wantValue(t, exec(t, s, "return 'echo' in tools"), "false")
}

func TestStoreAndLoad(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, `
		const seen = load("counter");
		store("counter", seen + 1);
		store("list", [1, { a: null }]);
		store("old", undefined);
		return [seen, load("counter"), load("missing"), load("old")];
	`, ExecuteOptions{Store: map[string]any{"counter": 41, "old": "x"}})
	wantValue(t, r, "[41,42,null,null]")
	if canon(t, r.StoreWrites.Set["counter"]) != "42" || canon(t, r.StoreWrites.Set["list"]) != `[1,{"a":null}]` ||
		len(r.StoreWrites.Delete) != 1 || r.StoreWrites.Delete[0] != "old" {
		t.Fatalf("writes %+v", r.StoreWrites)
	}

	r = exec(t, s, `const value = load("obj"); value.a = 2; const kept = { b: 1 }; store("kept", kept); kept.b = 2;
		return [load("obj").a, load("kept").b];`, ExecuteOptions{Store: map[string]any{"obj": map[string]int{"a": 1}}})
	wantValue(t, r, "[1,1]")

	r = exec(t, s, `
		const attempt = (fn) => { try { fn(); return "ok"; } catch (error) { return error.name; } };
		return [
			attempt(() => store(1, "x")),
			attempt(() => load({})),
			attempt(() => store("fn", () => 1)),
			attempt(() => store("big", "x".repeat(300 * 1024))),
			attempt(() => { for (let i = 0; i < 8; i++) store("k" + i, "x".repeat(200 * 1024)); }),
		];
	`)
	wantValue(t, r, `["TypeError","TypeError","TypeError","RangeError","RangeError"]`)

	e := wantError(t, exec(t, s, `store("img", "x".repeat(300 * 1024));`), ErrorScript)
	if !strings.Contains(e.Message, `store("img") value has 307202 characters of JSON`) || !strings.Contains(e.Message, "Show images with image()") {
		t.Fatalf("%q", e.Message)
	}
}

func TestGlobals(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	record := func(_ context.Context, args json.RawMessage) (any, error) {
		mu.Lock()
		seen = append(seen, string(args))
		mu.Unlock()
		return nil, nil
	}
	s := newSandbox(t, Options{Tools: []Tool{echoTool()}, Globals: []Tool{{Name: "attach", Execute: record}}})
	r := exec(t, s, `
		await attach({ ref: 1 });
		attach("not awaited");
		return [typeof attach, typeof globalThis.attach, await tools.echo(2)];
	`)
	wantValue(t, r, `["function","function",2]`)
	if len(r.Calls) != 1 || r.Calls[0].Name != "echo" {
		t.Fatalf("calls %+v", r.Calls)
	}
	// pi runs a call synchronously up to its first await, so an unawaited
	// global has run when the script settles. Here each call runs on its own
	// goroutine; the unawaited one is started before the script settles but
	// may still be running, so wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if strings.Join(seen, " ") != `{"ref":1} "not awaited"` {
		t.Fatalf("seen %v", seen)
	}
}

func TestNamespacedAndSpreadGlobals(t *testing.T) {
	var seen []string
	s := newSandbox(t, Options{Globals: []Tool{
		{Name: "models.list", Spread: true, Execute: func(_ context.Context, args json.RawMessage) (any, error) {
			seen = append(seen, string(args))
			return nil, nil
		}},
		{Name: "models.first", Execute: func(_ context.Context, args json.RawMessage) (any, error) { return args, nil }},
	}})
	r := exec(t, s, `
		await models.list("classifier", undefined, 3);
		await models.list();
		try { models.extra = 1; } catch {}
		return [Object.keys(models), await models.first("a", "ignored"), "extra" in models];
	`)
	wantValue(t, r, `[["list","first"],"a",false]`)
	if strings.Join(seen, " ") != `["classifier",null,3] []` {
		t.Fatalf("seen %v", seen)
	}

	none := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	s = newSandbox(t, Options{Globals: []Tool{{Name: "models.classify", Execute: none}, {Name: "models.generateImages", Execute: none}}})
	e := wantError(t, exec(t, s, "await models.generateImage();"), ErrorScript)
	if e.Message != `models.generateImage does not exist. Did you mean models.generateImages? Check for a member with "generateImage" in models.` {
		t.Fatalf("%q", e.Message)
	}
}

func TestInvalidGlobalNames(t *testing.T) {
	none := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	for _, name := range []string{"a.b.c", "a.", ".a", "tools.x", "store.x", "a.not-valid", "not-valid", "tools", "console", "store", "load"} {
		if _, err := New(Options{Globals: []Tool{{Name: name, Execute: none}}}); err == nil || !strings.Contains(err.Error(), "Invalid global") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := New(Options{Globals: []Tool{{Name: "models", Execute: none}, {Name: "models.list", Execute: none}}})
	if err == nil || !strings.Contains(err.Error(), "conflicts with the namespace") {
		t.Errorf("namespace conflict: %v", err)
	}
}

func TestTimeouts(t *testing.T) {
	s := newSandbox(t, Options{})
	for _, code := range []string{"while (true) {}", "while (true) await null"} {
		start := time.Now()
		wantError(t, exec(t, s, code, ExecuteOptions{Timeout: 200 * time.Millisecond}), ErrorTimeout)
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%s: took %v", code, time.Since(start))
		}
	}
	wait := Tool{Name: "wait", Execute: func(context.Context, json.RawMessage) (any, error) {
		time.Sleep(50 * time.Millisecond)
		return "late", nil
	}}
	s.RegisterTool(wait)
	wantValue(t, exec(t, s, "return await tools.wait()", ExecuteOptions{Timeout: -1}), `"late"`)
}

func TestStalledScript(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool()}})
	e := wantError(t, exec(t, s, "await tools.echo(1); await new Promise(() => {}); return 'never'", ExecuteOptions{Timeout: -1}), ErrorScript)
	if !strings.Contains(e.Message, "can never settle") {
		t.Fatal(e.Message)
	}
	// Returning while a call is still pending is not a stall.
	wantValue(t, exec(t, s, "tools.echo(2); return 'early'", ExecuteOptions{Timeout: -1}), `"early"`)
}

func TestAbortCancelsCalls(t *testing.T) {
	called := make(chan context.Context, 4)
	s := newSandbox(t, Options{Tools: []Tool{{Name: "hang", Execute: func(ctx context.Context, _ json.RawMessage) (any, error) {
		called <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}}}})

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("user cancelled"))
	r, _ := s.Execute(ctx, "await tools.hang()", ExecuteOptions{})
	if e := wantError(t, r, ErrorAborted); e.Message != "user cancelled" {
		t.Fatalf("%+v", e)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan Result)
	go func() {
		r, _ := s.Execute(ctx2, "await tools.hang(); return 'never'", ExecuteOptions{})
		done <- r
	}()
	toolCtx := <-called
	cancel2()
	r = <-done
	if e := wantError(t, r, ErrorAborted); e.Message != "Execution aborted" {
		t.Fatalf("%+v", e)
	}
	if len(r.Calls) != 1 || r.Calls[0].Status != CallCancelled || toolCtx.Err() == nil {
		t.Fatalf("calls %+v, tool ctx %v", r.Calls, toolCtx.Err())
	}

	go func() {
		r, _ := s.Execute(context.Background(), "await tools.hang(); return 'never'", ExecuteOptions{})
		done <- r
	}()
	<-called
	s.Close()
	if e := wantError(t, <-done, ErrorAborted); e.Message != "Sandbox closed" {
		t.Fatalf("%+v", e)
	}
	if _, err := s.Execute(context.Background(), "return 1", ExecuteOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("execute after close: %v", err)
	}
}

func TestOutputLimits(t *testing.T) {
	s := newSandbox(t, Options{Timeout: time.Minute})
	for _, print := range []string{"text(s)", "console.log(s)", `image("data:image/png;base64," + p)`} {
		r := exec(t, s, `
			const s = "x".repeat(1 << 20);
			const p = "iVBORw0KGgoA" + "A".repeat(1 << 20);
			for (;;) { try { `+print+`; } catch {} }
		`)
		e := wantError(t, r, ErrorScript)
		if e.Name != "RangeError" || !strings.Contains(e.Message, "script output exceeded") {
			t.Fatalf("%s: %+v", print, e)
		}
		chars := 0
		for _, item := range r.Output {
			chars += len(item.Text) + len(item.Data)
		}
		if chars > MaxOutputChars || chars <= MaxOutputChars-(2<<20) {
			t.Fatalf("%s: %d chars", print, chars)
		}
	}
	r := exec(t, s, `for (;;) text("");`)
	if e := wantError(t, r, ErrorScript); e.Name != "RangeError" || len(r.Output) != MaxOutputItems {
		t.Fatalf("%+v, %d items", e, len(r.Output))
	}
}

func TestParallelExecutionsShareNothing(t *testing.T) {
	s := newSandbox(t, Options{})
	codes := []string{
		"globalThis.shared = 'a'; await null; return globalThis.shared",
		"globalThis.shared = 'b'; await null; return globalThis.shared",
		"return typeof globalThis.shared",
	}
	results := make([]Result, len(codes))
	var wg sync.WaitGroup
	for i, code := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = s.Execute(context.Background(), code, ExecuteOptions{})
		}()
	}
	wg.Wait()
	for i, want := range []string{`"a"`, `"b"`, `"undefined"`} {
		wantValue(t, results[i], want)
	}
}

func TestDeepRecursion(t *testing.T) {
	s := newSandbox(t, Options{})
	r := exec(t, s, `
		let depth = 0;
		function dive() { depth++; dive(); }
		try { dive(); } catch (error) { return [error.name, depth > 1000]; }
	`)
	wantValue(t, r, `["RangeError",true]`)
}

func TestNoHostGlobalsAndNoEscape(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool()}})
	r := exec(t, s, `return [typeof process, typeof require, typeof module, typeof setTimeout, typeof fetch,
		typeof WebAssembly, typeof std, typeof os, typeof globalThis.constructor]`)
	wantValue(t, r, `["undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","function"]`)
	r = exec(t, s, `
		return [
			eval("typeof process"),
			new Function("return typeof process")(),
			tools.echo.constructor("return typeof require")(),
			(async () => {}).constructor("return typeof setTimeout")() instanceof Promise,
		];
	`)
	wantValue(t, r, `["undefined","undefined","undefined",true]`)
	r = exec(t, s, `try { await import("node:fs"); return "imported"; } catch (error) { return error.constructor.name; }`)
	if !r.OK || string(r.Value) == `"imported"` {
		t.Fatalf("%+v", r)
	}
}

func TestPatchedBuiltinsDoNotCorruptTheBridge(t *testing.T) {
	s := newSandbox(t, Options{Tools: []Tool{echoTool()}})
	r := exec(t, s, `
		Array.prototype.toJSON = () => null;
		Object.prototype.toJSON = () => 5;
		Promise.prototype.then = () => {};
		Map.prototype.get = () => undefined;
		globalThis.JSON = { stringify: () => "x", parse: () => "x" };
		store("k", [1]);
		return [await tools.echo([2]), JSON.stringify({ a: 1 })];
	`)
	wantValue(t, r, `[[2],"{\"a\":1}"]`)
	if canon(t, r.StoreWrites.Set["k"]) != "[1]" {
		t.Fatalf("%+v", r.StoreWrites)
	}
	r = exec(t, s, `
		return [
			Object.getPrototypeOf(function* () {}).prototype,
			Object.getPrototypeOf(async function () {}),
			Object.getPrototypeOf(Int8Array).prototype,
			Object.getPrototypeOf([][Symbol.iterator]()),
			Object.getPrototypeOf(Object.getPrototypeOf([][Symbol.iterator]())),
			Object.getPrototypeOf(new Map()[Symbol.iterator]()),
			Object.getPrototypeOf(/a/[Symbol.matchAll]("")),
		].every((object) => Object.isFrozen(object));
	`)
	wantValue(t, r, "true")
	r = exec(t, s, `
		const object = {};
		object.toString = () => "custom";
		function Legacy() {}
		Legacy.prototype = Object.create(Error.prototype);
		Legacy.prototype.constructor = Legacy;
		const bare = new Error();
		bare.message = "set later";
		class MyError extends Error {
			constructor(message) {
				super(message);
				this.name = "MyError";
			}
		}
		let patched = "silent";
		try { Error.prototype.name = "Patched"; } catch (error) { patched = error.constructor.name; }
		return [String(object), new Legacy().constructor === Legacy, bare.message, new MyError("x").name, Error.prototype.name, patched];
	`)
	wantValue(t, r, `["custom",true,"set later","MyError","Error","TypeError"]`)
	if e := wantError(t, exec(t, s, "const error = new Error('x'); error.message = 42; throw error;"), ErrorScript); e.Name != "Error" || e.Message != "42" {
		t.Fatalf("%+v", e)
	}
	r = exec(t, s, `
		try { tools.echo = () => 'nope'; } catch {}
		try { tools.extra = () => 'nope'; } catch {}
		try { globalThis.tools = null; } catch {}
		return ["extra" in tools, await tools.echo('still')];
	`)
	wantValue(t, r, `[false,"still"]`)
}

// TestBrokenBridge feeds the host malformed VM messages directly, as pi does
// with a raw worker fixture.
func TestBrokenBridge(t *testing.T) {
	cases := []struct {
		m      vmMessage
		reason string
	}{
		{vmMessage{kind: "done", ok: true, value: ptr("1"), writes: "null"}, "store writes are not an array"},
		{vmMessage{kind: "done", ok: true, value: ptr("1"), writes: "[1]"}, "store writes contain a malformed entry"},
		{vmMessage{kind: "done", ok: true, value: ptr("1"), writes: `[["k", "{"]]`}, `store value for "k" is not valid JSON`},
		{vmMessage{kind: "done", ok: true, value: ptr("{"), writes: "[]"}, "return value is not valid JSON"},
		{vmMessage{kind: "done", ok: false, errorJSON: "5"}, "script error is not an object"},
		{vmMessage{kind: "done", ok: false, errorJSON: "{}"}, "script error is malformed"},
		{vmMessage{kind: "nonsense"}, "unknown message from the worker"},
	}
	for _, c := range cases {
		e := &execution{pending: map[int]*pendingCall{}, result: make(chan Result, 1), released: make(chan struct{}), release: func(*execution) {}}
		e.receive(c.m)
		r := <-e.result
		if r.OK || r.Error.Kind != ErrorSandbox || !strings.Contains(r.Error.Message, "Sandbox bridge broken: "+c.reason) {
			t.Errorf("%s: %+v", c.reason, r.Error)
		}
	}
}

func TestPreludeIsPis(t *testing.T) {
	// FNV-1a of pi v1.2.0's PRELUDE_SOURCE, computed by pi's own function.
	if preludeHash != "8c6c9242" {
		t.Fatalf("prelude hash %s: the embedded prelude differs from pi v1.2.0's", preludeHash)
	}
	// The limits are inlined in the prelude; the Go constants must match them.
	for _, want := range []string{
		fmt.Sprintf("json.length > %d", MaxStoreValueChars),
		fmt.Sprintf("next > %d", MaxStoreTotalChars),
		fmt.Sprintf("outputChars > %d || outputItems > %d", MaxOutputChars, MaxOutputItems),
	} {
		if !strings.Contains(preludeSource, want) {
			t.Errorf("prelude lacks %q", want)
		}
	}
	if !bytes.HasPrefix([]byte(preludeSource), []byte("(function (bridge, toolsJson, globalsJson, storeJson) {")) {
		t.Error("unexpected prelude head")
	}
}

func TestIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"mcp__docs__search": "mcp__docs__search", "my-tool": "my_tool", "": "_", "1abc": "_abc", "a1": "a1", "поиск": "_____", "$x": "$x",
	} {
		if got := Identifier(in); got != want {
			t.Errorf("Identifier(%q) = %q, want %q", in, got, want)
		}
	}
}
