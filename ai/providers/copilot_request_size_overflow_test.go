package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Ported from pi packages/ai/test/github-copilot-request-size-overflow.test.ts
// (73f8de80): Copilot rejects request bodies over its byte-size limit with
// HTTP 413 and a generic "failed to parse request" body, which the overflow
// detection must take for an overflow through the real error path.
func TestCopilotHTTPRequestSizeErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		nested   bool
		overflow bool
	}{
		{413, true, true},
		{413, false, true},
		{400, true, false},
	} {
		t.Run(fmt.Sprintf("HTTP %d nested=%v", tc.status, tc.nested), func(t *testing.T) {
			errorBody := map[string]string{"message": "failed to parse request", "code": ""}
			var body any = errorBody
			if tc.nested {
				body = map[string]any{"error": errorBody}
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write(encoded)
			}))
			defer server.Close()
			model := &ai.Model{
				ID: "copilot-test-model", Name: "Copilot test model", Api: ai.APIOpenAIResponses,
				Provider: "github-copilot", BaseURL: server.URL, Reasoning: true,
				Input: []string{"text", "image"}, ContextWindow: 1_000_000, MaxTokens: 128_000,
			}
			result := StreamOpenAIResponses(context.Background(), model,
				ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("Continue", 0)}}),
				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}}}).
				Result()

			if n := calls.Load(); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
			if result.Provider != "github-copilot" {
				t.Errorf("provider = %q", result.Provider)
			}
			if result.StopReason != ai.StopError {
				t.Errorf("stop reason = %s", result.StopReason)
			}
			if result.Usage.TotalTokens != 0 {
				t.Errorf("total tokens = %d", result.Usage.TotalTokens)
			}
			if !strings.Contains(result.ErrorMessage, fmt.Sprintf("(%d)", tc.status)) || !strings.Contains(result.ErrorMessage, "failed to parse request") {
				t.Errorf("error message = %q", result.ErrorMessage)
			}
			if got := ai.IsContextOverflow(*result, model.ContextWindow); got != tc.overflow {
				t.Errorf("IsContextOverflow(%q) = %v, want %v", result.ErrorMessage, got, tc.overflow)
			}
		})
	}
}
