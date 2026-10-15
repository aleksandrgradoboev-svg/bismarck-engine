package jstext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// utf8Capture is testdata/utf8-node.json, written by capture-utf8.mjs: what a
// TextDecoder made of "a", a lead byte, up to three bytes from Follow, and "b".
type utf8Capture struct {
	Node    string            `json:"node"`
	Follow  []byte            `json:"follow"`
	Digests map[string]string `json:"digests"`
	Samples []struct {
		Bytes   string `json:"bytes"`
		Decoded string `json:"decoded"`
	} `json:"samples"`
}

func loadUTF8Capture(t *testing.T) utf8Capture {
	t.Helper()
	data, err := os.ReadFile("testdata/utf8-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var c utf8Capture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("testdata/utf8-node.json: %v; rerun capture-utf8.mjs", err)
	}
	return c
}

// decodeUTF8Case is the capture's decode of "a", bytes, "b".
func decodeUTF8Case(bytes []byte) string {
	return DecodeUTF8(append(append([]byte{'a'}, bytes...), 'b'))
}

// DecodeUTF8 agrees with node's TextDecoder on every case the capture
// enumerates: each lead byte from 0x80 to 0xFF, alone and followed by up to
// three bytes straddling every boundary the WHATWG decoder checks.
func TestDecodeUTF8MatchesNode(t *testing.T) {
	c := loadUTF8Capture(t)
	if len(c.Digests) != 0x80 {
		t.Fatalf("capture has %d lead bytes, want 128; rerun capture-utf8.mjs", len(c.Digests))
	}
	for lead := 0x80; lead <= 0xFF; lead++ {
		hash := sha256.New()
		write := func(bytes ...byte) {
			hash.Write([]byte(decodeUTF8Case(bytes)))
			hash.Write([]byte{0xFF})
		}
		write(byte(lead))
		for _, b1 := range c.Follow {
			write(byte(lead), b1)
			for _, b2 := range c.Follow {
				write(byte(lead), b1, b2)
				for _, b3 := range c.Follow {
					write(byte(lead), b1, b2, b3)
				}
			}
		}
		key := strconv.FormatInt(int64(lead), 16)
		if got := hex.EncodeToString(hash.Sum(nil)); got != c.Digests[key] {
			t.Errorf("lead byte 0x%s: the decodes differ from node %s's", key, c.Node)
		}
	}
	for _, s := range c.Samples {
		var bytes []byte
		for _, h := range strings.Fields(s.Bytes) {
			b, err := strconv.ParseUint(h, 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			bytes = append(bytes, byte(b))
		}
		var got []string
		for _, r := range decodeUTF8Case(bytes) {
			got = append(got, fmt.Sprintf("%04x", r))
		}
		if strings.Join(got, " ") != s.Decoded {
			t.Errorf("% x decodes to %s, node %s", bytes, strings.Join(got, " "), s.Decoded)
		}
	}
}

// Valid UTF-8 comes back as the same string, whatever it holds.
func TestDecodeUTF8KeepsValidText(t *testing.T) {
	for _, s := range []string{"", "plain", "caf\u00e9 \u20ac \U0001F600", "\uFEFFbom", "\x00\x7f"} {
		if got := DecodeUTF8([]byte(s)); got != s {
			t.Errorf("DecodeUTF8(%q) = %q", s, got)
		}
	}
}
