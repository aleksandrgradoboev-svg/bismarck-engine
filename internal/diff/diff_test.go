package diff

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// golden is testdata/jsdiff-8.0.4.json, written by testdata/capture.mjs.
type golden struct {
	JSDiff string `json:"jsdiff"`
	Cases  []struct {
		Name    string  `json:"name"`
		Old     string  `json:"old"`
		New     string  `json:"new"`
		Context int     `json:"context"`
		Changes [][]any `json:"changes"`
		Patch   string  `json:"patch"`
	} `json:"cases"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	b, err := os.ReadFile("testdata/jsdiff-8.0.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if g.JSDiff != "8.0.4" || len(g.Cases) < 400 {
		t.Fatalf("golden from jsdiff %s with %d cases", g.JSDiff, len(g.Cases))
	}
	return g
}

func TestDiffLinesMatchesJSDiff(t *testing.T) {
	for _, c := range loadGolden(t).Cases {
		want := []Change{}
		for _, p := range c.Changes {
			want = append(want, Change{Count: int(p[0].(float64)), Added: p[1].(bool), Removed: p[2].(bool), Value: p[3].(string)})
		}
		if got := DiffLines(c.Old, c.New); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: DiffLines(%q, %q)\n got  %+v\n want %+v", c.Name, c.Old, c.New, got, want)
		}
	}
}

func TestCreateTwoFilesPatchMatchesJSDiff(t *testing.T) {
	for _, c := range loadGolden(t).Cases {
		if got := CreateTwoFilesPatch("f.txt", "f.txt", c.Old, c.New, c.Context); got != c.Patch {
			t.Errorf("%s (context %d): CreateTwoFilesPatch(%q, %q)\n got  %q\n want %q", c.Name, c.Context, c.Old, c.New, got, c.Patch)
		}
	}
}
