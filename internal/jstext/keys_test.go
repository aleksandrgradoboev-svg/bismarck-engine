package jstext

import (
	"strconv"
	"testing"
)

// ArrayIndexKey agrees with node on which names an object lists ahead of the
// others: for each, node was asked whether Object.keys({x: 1, [name]: 1})
// puts name first.
func TestArrayIndexKeyMatchesNode(t *testing.T) {
	cases := map[string]bool{
		"0": true, "1": true, "12": true, "4294967294": true,
		"4294967295": false, "4294967296": false, "01": false, "00": false, "-1": false,
		"1.0": false, "1e1": false, " 1": false, "+1": false, "": false,
		"99999999999": false, "abc": false, "\u0663": false,
	}
	for name, want := range cases {
		n, got := ArrayIndexKey(name)
		if got != want {
			t.Errorf("ArrayIndexKey(%q) = %v, node %v", name, got, want)
		}
		if got && strconv.FormatUint(uint64(n), 10) != name {
			t.Errorf("ArrayIndexKey(%q) = %d", name, n)
		}
	}
}
