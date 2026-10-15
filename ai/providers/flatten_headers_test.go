package providers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// responseHeadersCaptureFile is written by
// testdata/response-headers/capture-response-headers.mts, which runs pi's
// headersToRecord on node's fetch Response for each row's raw bytes.
const responseHeadersCaptureFile = "testdata/response-headers/response-headers-49681e1b7.json"

// serveRaw answers one request on a loopback listener with raw, byte for byte,
// and returns the URL to request.
func serveRaw(t *testing.T, raw string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64*1024)
		_, _ = conn.Read(buf)
		_, _ = io.WriteString(conn, raw)
	}()
	return "http://" + ln.Addr().String() + "/"
}

// TestResponseHeadersRecordMatchesPi reads each captured raw response through
// Go's client and requires responseHeadersRecord to build pi's
// headersToRecord record: names lowercased, a repeated name's values joined
// with ", " in wire order, set-cookie holding its last value, and the
// headers net/http takes out of its header map — Transfer-Encoding, Trailer,
// Connection: close, and Content-Encoding of a body it gunzipped — kept as
// undici keeps them. The record is taken before the body is read, as the
// adapters take it: reading a chunked body merges every trailer it carries
// into resp.Trailer, where undici's Headers never hold one.
func TestResponseHeadersRecordMatchesPi(t *testing.T) {
	data, err := os.ReadFile(responseHeadersCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/response-headers/capture-response-headers.mts)", responseHeadersCaptureFile, err)
	}
	var capture struct {
		Rows []struct {
			Name     string `json:"name"`
			Response string `json:"response"`
			// ResponseBase64 is the response instead when it is not text.
			ResponseBase64 []byte            `json:"responseBase64"`
			Record         map[string]string `json:"record"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", responseHeadersCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", responseHeadersCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			raw := row.Response
			if row.ResponseBase64 != nil {
				raw = string(row.ResponseBase64)
			}
			resp, err := http.Get(serveRaw(t, raw))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := responseHeadersRecord(resp); !reflect.DeepEqual(got, row.Record) {
				t.Errorf("responseHeadersRecord = %v, want pi's %v", got, row.Record)
			}
			_, _ = io.ReadAll(resp.Body)
		})
	}
}

// TestOnResponseGetsTheRecordPiHands requires the anthropic and pi-messages
// adapters to hand OnResponse the record TestResponseHeadersRecordMatchesPi
// pins — for the chunked event stream they read, Transfer-Encoding included —
// by serving each captured response to each adapter.
func TestOnResponseGetsTheRecordPiHands(t *testing.T) {
	data, err := os.ReadFile(responseHeadersCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name           string            `json:"name"`
			Response       string            `json:"response"`
			ResponseBase64 []byte            `json:"responseBase64"`
			Record         map[string]string `json:"record"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	adapters := map[string]func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessageEventStream{
		"anthropic": func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessageEventStream {
			model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: baseURL, MaxTokens: 4096}
			return StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Hello", 1)}}), &AnthropicOptions{StreamOptions: opts})
		},
		"pi-messages": func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessageEventStream {
			return StreamPiMessages(context.Background(), piMessagesTestModel(baseURL), ai.NormalizeContext(piMessagesTestContext()), &PiMessagesOptions{StreamOptions: opts})
		},
	}
	for _, row := range capture.Rows {
		raw := row.Response
		if row.ResponseBase64 != nil {
			raw = string(row.ResponseBase64)
		}
		for name, stream := range adapters {
			t.Run(row.Name+"/"+name, func(t *testing.T) {
				var got map[string]string
				opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
					APIKey: "k",
					OnResponse: func(r ai.ProviderResponse, _ *ai.Model) error {
						got = r.Headers
						return nil
					},
				}}
				stream(strings.TrimSuffix(serveRaw(t, raw), "/"), opts).Result()
				if !reflect.DeepEqual(got, row.Record) {
					t.Errorf("OnResponse headers = %v, want pi's %v", got, row.Record)
				}
			})
		}
	}
}
