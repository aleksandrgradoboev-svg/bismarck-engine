package cbor

import (
	"encoding/hex"
	"testing"
)

// TestDecodeNegativeIntegerSafeRangeBoundary pins the one-off in major type 1.
//
// pi decodes a negative integer as -1 - n and then tests Number.isSafeInteger on
// the *result*, which is false for -2^53. The largest argument it will accept is
// therefore 2^53-2, not 2^53-1: bounding n with > rather than >= admits -2^53,
// which Encode then refuses to write back, so the value could be read off the
// wire and never relayed. Upstream's vectors stop at the major-type-0 edge, so
// this boundary is only covered here.
func TestDecodeNegativeIntegerSafeRangeBoundary(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		want int64
		err  string
	}{
		{
			name: "min_safe_integer",
			hex:  "3b001ffffffffffffe", // -1 - (2^53-2) = -(2^53-1)
			want: -maxSafeInteger,
		},
		{
			name: "one_below_min_safe_integer",
			hex:  "3b001fffffffffffff", // -1 - (2^53-1) = -2^53
			err:  "Decoded CBOR integer is outside the safe range",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := hex.DecodeString(test.hex)
			if err != nil {
				t.Fatalf("bad test hex: %v", err)
			}
			got, err := Decode(raw, nil)
			if test.err != "" {
				if err == nil {
					t.Fatalf("Decode accepted %s, want error %q", test.hex, test.err)
				}
				if err.Error() != test.err {
					t.Errorf("error text\n got %q\nwant %q", err.Error(), test.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode rejected %s: %v", test.hex, err)
			}
			if got != any(test.want) {
				t.Fatalf("decoded %#v, want %d", got, test.want)
			}
			// The accepted edge must survive the round trip, which is the
			// property the bound exists to protect.
			again, err := Encode(got, nil)
			if err != nil {
				t.Fatalf("Encode rejected a value Decode accepted: %v", err)
			}
			if hex.EncodeToString(again) != test.hex {
				t.Errorf("round trip diverges\n got %s\nwant %s", hex.EncodeToString(again), test.hex)
			}
		})
	}
}
