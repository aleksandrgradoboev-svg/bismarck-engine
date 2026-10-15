package coding

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// TestEstimateMessageTokensMatchesPiCapture checks EstimateMessageTokens against
// pi's estimateTokens over the messages in testdata/compaction/estimate-0.87.0.json
// (captured by capture-estimate.mts from the npm build 0.87.0): system messages
// with sections and tool declarations (upstream 466db0fec), and text whose JS
// length or JSON.stringify form differs from Go's byte length and json.Marshal.
func TestEstimateMessageTokensMatchesPiCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/compaction/estimate-0.87.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Message json.RawMessage `json:"message"`
		Tokens  int             `json:"tokens"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no estimate cases captured")
	}
	for i, c := range cases {
		m, err := ai.UnmarshalMessage(c.Message)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got := EstimateMessageTokens(m); got != c.Tokens {
			t.Errorf("case %d (%s): EstimateMessageTokens = %d, pi estimateTokens = %d", i, m.MessageRole(), got, c.Tokens)
		}
	}
}
