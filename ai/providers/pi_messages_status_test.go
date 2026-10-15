package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// piMessagesStatusCaptureFile is written by
// testdata/pi-messages/capture-pi-messages-status.mts, which streams pi's
// pi-messages adapter from source at the named sha against a raw server.
const piMessagesStatusCaptureFile = "testdata/pi-messages/pi-messages-status-49681e1b7.json"

// TestPiMessagesStatusTextMatchesPi serves each captured status line byte for
// byte and requires pi's errorMessage and the response-failure details'
// status and statusText: undici's statusText is the reason phrase as the
// server sent it — nothing when there is none, a phrase of the server's own,
// spaces kept — decoded as UTF-8, never the standard phrase for the code.
func TestPiMessagesStatusTextMatchesPi(t *testing.T) {
	data, err := os.ReadFile(piMessagesStatusCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/pi-messages/capture-pi-messages-status.mts)", piMessagesStatusCaptureFile, err)
	}
	var capture struct {
		Body string `json:"body"`
		Rows []struct {
			Name       string `json:"name"`
			StatusLine string `json:"statusLine"`
			// StatusLineBase64 is the status line instead when it is not UTF-8.
			StatusLineBase64 []byte `json:"statusLineBase64"`
			ErrorMessage     string `json:"errorMessage"`
			Status           int    `json:"status"`
			StatusText       string `json:"statusText"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", piMessagesStatusCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", piMessagesStatusCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			line := row.StatusLine
			if row.StatusLineBase64 != nil {
				line = string(row.StatusLineBase64)
			}
			raw := fmt.Sprintf("%s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", line, len(capture.Body), capture.Body)
			baseURL := strings.TrimSuffix(serveRaw(t, raw), "/") + "/v1"
			final := StreamPiMessages(context.Background(), piMessagesTestModel(baseURL), ai.NormalizeContext(piMessagesTestContext()),
				&PiMessagesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key"}}}).Result()
			if final.ErrorMessage != row.ErrorMessage {
				t.Errorf("errorMessage = %q, pi %q", final.ErrorMessage, row.ErrorMessage)
			}
			if len(final.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %+v, want the response failure", final.Diagnostics)
			}
			status, _ := final.Diagnostics[0].Details.Get("status")
			statusText, _ := final.Diagnostics[0].Details.Get("statusText")
			if status != row.Status || statusText != row.StatusText {
				t.Errorf("details status %v statusText %q, pi %d %q", status, statusText, row.Status, row.StatusText)
			}
		})
	}
}
