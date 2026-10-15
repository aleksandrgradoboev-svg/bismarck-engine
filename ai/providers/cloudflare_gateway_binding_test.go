package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

const gatewayBindingBaseURL = "https://gateway.ai.cloudflare.com/v1/account-id/my-gateway"

type capturedGatewayRun struct {
	gatewayID string
	req       AIGatewayUniversalRequest
	ctx       context.Context
}

type fakeGatewayBinding struct {
	mu       sync.Mutex
	runs     []capturedGatewayRun
	response *http.Response
}

type fakeGatewayBindingGateway struct {
	binding *fakeGatewayBinding
	id      string
}

func (b *fakeGatewayBinding) Gateway(id string) AIGatewayBindingGateway {
	return &fakeGatewayBindingGateway{binding: b, id: id}
}

func (b *fakeGatewayBinding) captured() []capturedGatewayRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]capturedGatewayRun(nil), b.runs...)
}

func (g *fakeGatewayBindingGateway) Run(ctx context.Context, req AIGatewayUniversalRequest) (*http.Response, error) {
	g.binding.mu.Lock()
	g.binding.runs = append(g.binding.runs, capturedGatewayRun{gatewayID: g.id, req: req, ctx: ctx})
	response := g.binding.response
	g.binding.mu.Unlock()
	if response != nil {
		return response, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("{}")),
	}, nil
}

func newGatewayBindingTestDoer(t *testing.T) (ai.HTTPDoer, *fakeGatewayBinding) {
	t.Helper()
	binding := &fakeGatewayBinding{}
	doer, err := NewGatewayBindingDoer(GatewayBindingDoerOptions{
		Binding: binding, BaseURL: gatewayBindingBaseURL, Gateway: "my-gateway",
	})
	if err != nil {
		t.Fatalf("NewGatewayBindingDoer: %v", err)
	}
	return doer, binding
}

// gatewayPost builds a POST with a body. Pass a nil body via gatewayRequest to
// exercise the absent-body path — "" here means a present, empty body, which is a
// different case (see TestGatewayBindingEmptyBodyIsPresentButNonJSON).
func gatewayPost(t *testing.T, rawURL, body string) *http.Request {
	t.Helper()
	return gatewayRequest(t, http.MethodPost, rawURL, strings.NewReader(body))
}

func gatewayRequest(t *testing.T, method, rawURL string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

func gatewayQueryString(t *testing.T, run capturedGatewayRun) string {
	t.Helper()
	return string(run.req.Query)
}

func TestGatewayBindingDerivesProviderAndEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, path, body, provider, endpoint string }{
		{"anthropic", "/anthropic/v1/messages", `{"model":"claude"}`, "anthropic", "v1/messages"},
		{"openai", "/openai/responses", `{"model":"gpt"}`, "openai", "responses"},
		{"workers-ai", "/workers-ai/v1/chat/completions", `{"model":"@cf/meta/llama"}`, "workers-ai", "v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, binding := newGatewayBindingTestDoer(t)
			if _, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+tc.path, tc.body)); err != nil {
				t.Fatalf("Do: %v", err)
			}
			runs := binding.captured()
			if len(runs) != 1 {
				t.Fatalf("binding runs = %d, want 1", len(runs))
			}
			if runs[0].req.Provider != tc.provider || runs[0].req.Endpoint != tc.endpoint {
				t.Errorf("split = %s/%s, want %s/%s", runs[0].req.Provider, runs[0].req.Endpoint, tc.provider, tc.endpoint)
			}
			if runs[0].gatewayID != "my-gateway" {
				t.Errorf("gatewayID = %q, want my-gateway", runs[0].gatewayID)
			}
			// The body reaches the binding byte-for-byte: no decode/re-encode, so
			// key order and integer precision survive.
			if got := gatewayQueryString(t, runs[0]); got != tc.body {
				t.Errorf("query = %s, want the body verbatim %s", got, tc.body)
			}
		})
	}
}

func TestGatewayBindingPassesBodyThroughByteForByte(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	// Key order is NOT alphabetical and the id exceeds 2^53: a map round-trip
	// would reorder the first and corrupt the second.
	body := `{"model":"gpt","zeta":1,"alpha":2,"id":9007199254740993}`
	if _, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", body)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := gatewayQueryString(t, binding.captured()[0]); got != body {
		t.Fatalf("query = %s, want %s", got, body)
	}
}

func TestGatewayBindingKeepsQueryStringInEndpoint(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	if _, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+"/openai/responses?stream=true&x=1", `{}`)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := binding.captured()[0].req.Endpoint; got != "responses?stream=true&x=1" {
		t.Fatalf("endpoint = %q, want responses?stream=true&x=1", got)
	}
}

func TestGatewayBindingCollapsesHeaderNamesDeterministically(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	req := gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", `{}`)
	req.Header.Set("Content-Type", "application/json")
	// Go canonicalizes on Set/Add, so case-variant duplicates need raw map writes.
	// pi collapses these in insertion order, which http.Header does not have; the
	// port visits names in sorted order so the result is at least deterministic.
	req.Header["x-custom"] = []string{"a"}
	req.Header["X-Custom"] = []string{"b"}
	req.Header["x-multi"] = []string{"one", "two"}

	for i := 0; i < 20; i++ {
		binding.runs = nil
		if _, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", `{}`)); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}

	binding.runs = nil
	if _, err := doer.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	headers := binding.captured()[0].req.Headers
	if headers["content-type"] != "application/json" {
		t.Errorf("content-type = %q, want application/json", headers["content-type"])
	}
	for name := range headers {
		if name != strings.ToLower(name) {
			t.Errorf("header name %q is not lowercased", name)
		}
	}
	// "X-Custom" sorts before "x-custom", so its value leads.
	if got := headers["x-custom"]; got != "b, a" {
		t.Errorf("x-custom = %q, want %q", got, "b, a")
	}
	if got := headers["x-multi"]; got != "one, two" {
		t.Errorf("x-multi = %q, want %q", got, "one, two")
	}
}

func TestGatewayBindingStripsGatewayAuthAndForwardsTheRest(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	req := gatewayPost(t, gatewayBindingBaseURL+"/anthropic/v1/messages", `{"model":"claude"}`)
	req.Header.Set("cf-aig-authorization", "Bearer "+CloudflareGatewayBindingAuthSentinel)
	req.Header.Set("Content-Length", "17")
	req.Header.Set("Host", "gateway.ai.cloudflare.com")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	// A real provider key rides through: the gateway treats it as BYOK.
	req.Header.Set("x-api-key", "provider-key")
	req.Header.Set("cf-aig-metadata", `{"tenant":"t1"}`)
	if _, err := doer.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	headers := binding.captured()[0].req.Headers
	for _, stripped := range []string{"cf-aig-authorization", "content-length", "host"} {
		if _, ok := headers[stripped]; ok {
			t.Errorf("header %q must not reach the binding", stripped)
		}
	}
	for name, want := range map[string]string{
		"anthropic-version": "2023-06-01",
		"content-type":      "application/json",
		"x-api-key":         "provider-key",
		"cf-aig-metadata":   `{"tenant":"t1"}`,
	} {
		if headers[name] != want {
			t.Errorf("%s = %q, want %q", name, headers[name], want)
		}
	}
}

func TestGatewayBindingForwardsContext(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	req := gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", `{}`).WithContext(ctx)
	if _, err := doer.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := binding.captured()[0].ctx.Value(ctxKey{}); got != "marker" {
		t.Fatalf("binding ctx value = %v, want marker (request context must be forwarded)", got)
	}
}

func TestGatewayBindingReturnsBindingResponseUntouched(t *testing.T) {
	binding := &fakeGatewayBinding{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Cf-Aig-Log-Id": []string{"log-1"}},
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}}
	doer, err := NewGatewayBindingDoer(GatewayBindingDoerOptions{
		Binding: binding, BaseURL: gatewayBindingBaseURL, Gateway: "my-gateway",
	})
	if err != nil {
		t.Fatalf("NewGatewayBindingDoer: %v", err)
	}

	resp, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", `{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp != binding.response {
		t.Fatal("response must be the binding's own response, not a copy")
	}
	if got := resp.Header.Get("cf-aig-log-id"); got != "log-1" {
		t.Errorf("cf-aig-log-id = %q, want log-1", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "data: {}\n\n" {
		t.Errorf("body = %q, want the streamed bytes untouched", body)
	}
}

func TestGatewayBindingRejectsUnexpressibleRequests(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, wantErr string
		body                        io.Reader
	}{
		{name: "non-POST", method: http.MethodGet, path: "/anthropic/v1/messages", wantErr: "cannot express GET"},
		{name: "non-JSON body", method: http.MethodPost, path: "/anthropic/v1/messages",
			body: strings.NewReader("not json"), wantErr: "non-JSON body"},
		{name: "absent body", method: http.MethodPost, path: "/anthropic/v1/messages", wantErr: "missing body"},
		{name: "no endpoint", method: http.MethodPost, path: "/anthropic",
			body: strings.NewReader(`{}`), wantErr: "missing provider/endpoint path"},
		{name: "base path only", method: http.MethodPost, path: "/",
			body: strings.NewReader(`{}`), wantErr: "missing provider/endpoint path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, binding := newGatewayBindingTestDoer(t)
			_, err := doer.Do(gatewayRequest(t, tc.method, gatewayBindingBaseURL+tc.path, tc.body))
			if err == nil {
				t.Fatalf("Do(%s %s) succeeded, want error", tc.method, tc.path)
			}
			if !errors.Is(err, ErrUnexpressibleGatewayRequest) {
				t.Errorf("error %v does not match ErrUnexpressibleGatewayRequest", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "route it over HTTPS with gateway auth instead") {
				t.Errorf("error %q must carry the HTTPS-instead resolution hint", err)
			}
			if len(binding.captured()) != 0 {
				t.Errorf("binding was called, want no dispatch")
			}
		})
	}
}

// pi rejects only an ABSENT body. A present-but-empty one reaches JSON.parse(""),
// which throws — so it is "non-JSON body", not "missing body".
func TestGatewayBindingEmptyBodyIsPresentButNonJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{"strings.NewReader empty (http.NoBody)", strings.NewReader("")},
		{"opaque zero-byte reader", io.NopCloser(bytes.NewReader(nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, _ := newGatewayBindingTestDoer(t)
			_, err := doer.Do(gatewayRequest(t, http.MethodPost, gatewayBindingBaseURL+"/openai/responses", tc.body))
			if err == nil || !strings.Contains(err.Error(), "non-JSON body") {
				t.Fatalf("error = %v, want non-JSON body", err)
			}
		})
	}
}

func TestGatewayBindingRejectsOutOfPrefixURLs(t *testing.T) {
	// Silent passthrough would ship the auth sentinel to whatever host the URL
	// names; a misconfigured baseURL must fail loudly instead.
	for _, tc := range []struct{ name, rawURL string }{
		{"other host", "https://api.openai.com/v1/chat/completions"},
		{"other account, same origin", "https://gateway.ai.cloudflare.com/v1/other-account/my-gateway/anthropic/v1/messages"},
		{"scheme mismatch", "http://gateway.ai.cloudflare.com/v1/account-id/my-gateway/anthropic/v1/messages"},
		{"prefix is not a path boundary", "https://gateway.ai.cloudflare.com/v1/account-id/my-gateway-2/anthropic/v1/messages"},
		{"dot segments escape the prefix", gatewayBindingBaseURL + "/../other-gateway/anthropic/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, binding := newGatewayBindingTestDoer(t)
			_, err := doer.Do(gatewayPost(t, tc.rawURL, `{}`))
			if err == nil {
				t.Fatalf("Do(%s) succeeded, want error", tc.rawURL)
			}
			if !errors.Is(err, ErrOutsideGatewayPrefix) {
				t.Errorf("error %v does not match ErrOutsideGatewayPrefix", err)
			}
			if !strings.Contains(err.Error(), "outside the configured gateway prefix") {
				t.Errorf("error = %q, want it to name the prefix violation", err)
			}
			if len(binding.captured()) != 0 {
				t.Errorf("binding was called, want no dispatch")
			}
		})
	}
}

// The origin comparison is WHATWG-normalized, not a raw string compare: pi's
// `new URL()` lowercases the host and drops a scheme-default port, so requests
// carrying either form must still route.
func TestGatewayBindingNormalizesOrigin(t *testing.T) {
	for _, tc := range []struct{ name, rawURL string }{
		{"uppercase host", "https://GATEWAY.AI.CLOUDFLARE.COM/v1/account-id/my-gateway/anthropic/v1/messages"},
		{"explicit default port", "https://gateway.ai.cloudflare.com:443/v1/account-id/my-gateway/anthropic/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, binding := newGatewayBindingTestDoer(t)
			if _, err := doer.Do(gatewayPost(t, tc.rawURL, `{"model":"claude"}`)); err != nil {
				t.Fatalf("Do: %v", err)
			}
			runs := binding.captured()
			if len(runs) != 1 {
				t.Fatalf("binding runs = %d, want 1 (pi dispatches this)", len(runs))
			}
			if runs[0].req.Provider != "anthropic" || runs[0].req.Endpoint != "v1/messages" {
				t.Errorf("split = %s/%s, want anthropic/v1/messages", runs[0].req.Provider, runs[0].req.Endpoint)
			}
		})
	}
}

// Path resolution follows the WHATWG URL parser, which differs from path.Clean in
// two ways that change behavior here: empty segments survive, and a final "." or
// ".." leaves a trailing slash.
func TestGatewayBindingResolvesPathLikeWHATWG(t *testing.T) {
	for _, tc := range []struct {
		name, path         string
		provider, endpoint string
		wantErr            string
	}{
		{name: "dot segments normalize away", path: "/anthropic/../anthropic/v1/./messages",
			provider: "anthropic", endpoint: "v1/messages"},
		{name: "empty segment is preserved, not collapsed", path: "//anthropic/v1/messages",
			wantErr: "missing provider/endpoint path"},
		{name: "trailing .. keeps a slash and lands on the base", path: "/anthropic/..",
			wantErr: "missing provider/endpoint path"},
		{name: "trailing .. inside a provider yields an empty endpoint", path: "/anthropic/v1/..",
			provider: "anthropic", endpoint: ""},
		{name: "trailing . inside a provider yields an empty endpoint", path: "/anthropic/.",
			provider: "anthropic", endpoint: ""},
		{name: "percent-encoded separator is not a separator", path: "/anthropic%2Fv1/messages",
			provider: "anthropic%2Fv1", endpoint: "messages"},
		{name: "percent-encoding is preserved in the endpoint", path: "/anthropic/v1/mes%20sages",
			provider: "anthropic", endpoint: "v1/mes%20sages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, binding := newGatewayBindingTestDoer(t)
			_, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+tc.path, `{"model":"claude"}`))
			runs := binding.captured()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(runs) != 0 {
					t.Fatalf("binding was called, want no dispatch")
				}
				return
			}
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if len(runs) != 1 {
				t.Fatalf("binding runs = %d, want 1", len(runs))
			}
			if runs[0].req.Provider != tc.provider || runs[0].req.Endpoint != tc.endpoint {
				t.Errorf("split = %q/%q, want %q/%q",
					runs[0].req.Provider, runs[0].req.Endpoint, tc.provider, tc.endpoint)
			}
		})
	}
}

func TestGatewayBindingReadsStreamingRequestBody(t *testing.T) {
	// A one-shot body (no GetBody, unknown length) must still reach the JSON probe.
	// Consuming it is fine: unexpressible requests reject rather than replay.
	t.Run("valid JSON dispatches", func(t *testing.T) {
		doer, binding := newGatewayBindingTestDoer(t)
		req := gatewayRequest(t, http.MethodPost, gatewayBindingBaseURL+"/openai/responses", nil)
		req.Body = io.NopCloser(bytes.NewReader([]byte(`{"model":"gpt","stream":true}`)))
		req.ContentLength = -1
		if _, err := doer.Do(req); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if got := gatewayQueryString(t, binding.captured()[0]); got != `{"model":"gpt","stream":true}` {
			t.Fatalf("query = %s, want the streamed body verbatim", got)
		}
	})

	t.Run("non-JSON rejects and is never replayed", func(t *testing.T) {
		doer, binding := newGatewayBindingTestDoer(t)
		req := gatewayRequest(t, http.MethodPost, gatewayBindingBaseURL+"/openai/responses", nil)
		req.Body = io.NopCloser(bytes.NewReader([]byte("data: not json")))
		req.ContentLength = -1
		_, err := doer.Do(req)
		if err == nil || !strings.Contains(err.Error(), "non-JSON body") {
			t.Fatalf("error = %v, want non-JSON body", err)
		}
		if len(binding.captured()) != 0 {
			t.Fatal("binding was called, want no dispatch")
		}
	})
}

// pi rejects only an absent body; a literal JSON null parses and dispatches.
func TestGatewayBindingAcceptsJSONNullBody(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	if _, err := doer.Do(gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", `null`)); err != nil {
		t.Fatalf("Do: %v", err)
	}
	runs := binding.captured()
	if len(runs) != 1 {
		t.Fatalf("binding runs = %d, want 1", len(runs))
	}
	if got := gatewayQueryString(t, runs[0]); got != "null" {
		t.Fatalf("query = %q, want null", got)
	}
}

type errGatewayReader struct{}

func (errGatewayReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestGatewayBindingRejectsUnreadableBody(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	req := gatewayRequest(t, http.MethodPost, gatewayBindingBaseURL+"/openai/responses", nil)
	req.Body = io.NopCloser(errGatewayReader{})
	_, err := doer.Do(req)
	if err == nil || !strings.Contains(err.Error(), "unreadable body: boom") {
		t.Fatalf("error = %v, want it to name the read failure", err)
	}
	if !errors.Is(err, ErrUnexpressibleGatewayRequest) {
		t.Errorf("error %v does not match ErrUnexpressibleGatewayRequest", err)
	}
	if len(binding.captured()) != 0 {
		t.Error("binding was called, want no dispatch")
	}
}

// The doer closes the request body, as the *http.Client it substitutes for does.
type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestGatewayBindingClosesRequestBody(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"on dispatch", "/openai/responses", `{}`},
		{"on rejection", "/openai", `{}`},
		{"on out-of-prefix rejection", "", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer, _ := newGatewayBindingTestDoer(t)
			body := &closeTrackingBody{Reader: strings.NewReader(tc.body)}
			rawURL := gatewayBindingBaseURL + tc.path
			if tc.path == "" {
				rawURL = "https://api.openai.com/v1/chat/completions"
			}
			req := gatewayRequest(t, http.MethodPost, rawURL, nil)
			req.Body = body
			_, _ = doer.Do(req)
			if !body.closed {
				t.Fatal("request body was not closed")
			}
		})
	}
}

func TestGatewayBindingIsSafeForConcurrentUse(t *testing.T) {
	doer, binding := newGatewayBindingTestDoer(t)

	// The requests are built here: gatewayPost fails the test on an error,
	// which only the test's goroutine may do.
	reqs := make([]*http.Request, 32)
	for i := range reqs {
		reqs[i] = gatewayPost(t, gatewayBindingBaseURL+"/openai/responses", fmt.Sprintf(`{"n":%d}`, i))
	}
	var wg sync.WaitGroup
	for _, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := doer.Do(req); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := len(binding.captured()); got != 32 {
		t.Fatalf("binding runs = %d, want 32", got)
	}
}

func TestNewGatewayBindingDoerValidatesOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    GatewayBindingDoerOptions
		wantErr string
	}{
		{"nil binding", GatewayBindingDoerOptions{BaseURL: gatewayBindingBaseURL, Gateway: "g"},
			"Binding is required"},
		{"relative BaseURL", GatewayBindingDoerOptions{Binding: &fakeGatewayBinding{}, BaseURL: "/v1/acct/gw", Gateway: "g"},
			"must be absolute"},
		{"unparseable BaseURL", GatewayBindingDoerOptions{Binding: &fakeGatewayBinding{}, BaseURL: "https://%zz", Gateway: "g"},
			"is not a valid URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewGatewayBindingDoer(tc.opts)
			if err == nil {
				t.Fatalf("NewGatewayBindingDoer succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveGatewayPathMatchesWHATWG(t *testing.T) {
	// Pinned against the WHATWG URL parser (verified via new URL()); path.Clean
	// gets the last three wrong.
	for _, tc := range []struct{ in, want string }{
		{"", "/"},
		{"/v1/a/gw", "/v1/a/gw"},
		{"/v1/a/gw/", "/v1/a/gw/"},
		{"/a/b/../c", "/a/c"},
		{"/a/./b", "/a/b"},
		{"/..", "/"},
		{"/v1/..", "/"},
		{"/v1/a/gw/anthropic/..", "/v1/a/gw/"},
		{"/v1/a/gw/anthropic/.", "/v1/a/gw/anthropic/"},
		{"/v1/a/gw//anthropic//v1/messages", "/v1/a/gw//anthropic//v1/messages"},
	} {
		if got := resolveGatewayPath(tc.in); got != tc.want {
			t.Errorf("resolveGatewayPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizedGatewayOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://Gateway.AI.Cloudflare.COM/v1", "https://gateway.ai.cloudflare.com"},
		{"https://gateway.ai.cloudflare.com:443/v1", "https://gateway.ai.cloudflare.com"},
		{"http://example.com:80/v1", "http://example.com"},
		{"https://example.com:8443/v1", "https://example.com:8443"},
		{"http://example.com:443/v1", "http://example.com:443"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.in, err)
		}
		if got := normalizedGatewayOrigin(u); got != tc.want {
			t.Errorf("normalizedGatewayOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGatewayBindingComposesWithOpenAICompletions is the composition proof: the
// doer sits under a real provider stream, and the sentinel pairing keeps the
// SDK's placeholder auth headers out of the binding entry (pi's null
// ProviderHeaders suppression, ported under the 2026-08-04 ruling).
func TestGatewayBindingComposesWithOpenAICompletions(t *testing.T) {
	binding := &fakeGatewayBinding{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n")),
	}}
	doer, err := NewGatewayBindingDoer(GatewayBindingDoerOptions{
		Binding: binding, BaseURL: gatewayBindingBaseURL, Gateway: "my-gateway",
	})
	if err != nil {
		t.Fatalf("NewGatewayBindingDoer: %v", err)
	}

	model := &ai.Model{
		ID:       "gpt-4o",
		Api:      ai.APIOpenAICompletions,
		Provider: "cloudflare-ai-gateway",
		BaseURL:  gatewayBindingBaseURL + "/openai",
	}
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:     "unused",
			HTTPClient: doer,
			Headers: ai.ProviderHeaders{
				"Authorization":        nil,
				"x-api-key":            nil,
				"cf-aig-authorization": strPtr("Bearer " + CloudflareGatewayBindingAuthSentinel),
			},
		},
	}}

	result := StreamSimpleOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		opts).
		Result()
	if result == nil {
		t.Fatal("stream produced no result")
	}

	runs := binding.captured()
	if len(runs) != 1 {
		t.Fatalf("binding runs = %d, want 1", len(runs))
	}
	entry := runs[0]
	if entry.req.Provider != "openai" || entry.req.Endpoint != "chat/completions" {
		t.Errorf("split = %s/%s, want openai/chat/completions", entry.req.Provider, entry.req.Endpoint)
	}
	for _, banned := range []string{"authorization", "x-api-key", "cf-aig-authorization"} {
		if v, ok := entry.req.Headers[banned]; ok {
			t.Errorf("header %q reached the binding entry with value %q; it must be suppressed or stripped", banned, v)
		}
	}
	if !strings.Contains(gatewayQueryString(t, entry), `"model":"gpt-4o"`) {
		t.Errorf("query body = %s, want it to carry the model", gatewayQueryString(t, entry))
	}
}

// A header value's Latin-1 characters reach the binding as the characters pi's
// shim collected from a Headers iterator, not as the bytes the adapters wrote:
// in node, iterating `new Headers({"x-a": "café"})` yields the code units
// 99,97,102,233. The request goes through the real openai-completions adapter,
// whose Headers stand-in writes é as the single byte 0xE9.
func TestGatewayBindingDecodesHeaderBytesLikeFetch(t *testing.T) {
	binding := &fakeGatewayBinding{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(attrDoneSSE)),
	}}
	doer, err := NewGatewayBindingDoer(GatewayBindingDoerOptions{
		Binding: binding, BaseURL: gatewayBindingBaseURL, Gateway: "my-gateway",
	})
	if err != nil {
		t.Fatalf("NewGatewayBindingDoer: %v", err)
	}
	model := &ai.Model{ID: "gpt-4o", Api: ai.APIOpenAICompletions, Provider: "openai",
		BaseURL: gatewayBindingBaseURL + "/openai"}
	cafe := "caf" + string(rune(0xe9))
	result := StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(
		ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey: "unused", HTTPClient: doer, Headers: ai.ProviderHeaders{"X-A": strPtr(cafe)},
		}}}).Result()
	if result.StopReason == ai.StopError {
		t.Fatalf("stream failed: %s", result.ErrorMessage)
	}
	runs := binding.captured()
	if len(runs) != 1 {
		t.Fatalf("binding runs = %d, want 1", len(runs))
	}
	if got := runs[0].req.Headers["x-a"]; got != cafe {
		t.Fatalf("x-a = %q, want %q", got, cafe)
	}
}
