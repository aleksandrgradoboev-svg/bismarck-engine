package ai

import "testing"

// Pins for pi packages/ai/src/utils/text.ts at upstream 9e05370b2. Expected
// strings were captured by running the same inputs through upstream TS under
// node at the sha; they are model-visible bytes.

func TestContentTextSeparator(t *testing.T) {
	content := ContentList{TextContent{Text: "a"}, ImageContent{Data: "x", MimeType: "image/png"}, TextContent{Text: "b"}}
	if got := ContentText(content); got != "a\nb" {
		t.Fatalf("default separator: %q, want %q (non-text blocks skipped)", got, "a\nb")
	}
	if got := ContentText(ContentList{TextContent{Text: "a"}, TextContent{Text: "b"}}, ""); got != "ab" {
		t.Fatalf("explicit separator: %q, want %q", got, "ab")
	}
	if got := ContentText(nil); got != "" {
		t.Fatalf("nil content: %q, want empty", got)
	}
}

// intLikeSections is a sections literal mixing array-index names (ascending
// first in JS), non-index numeric strings ("01", "-1", 2^32-1) and plain names.
func intLikeSections() SystemSections {
	return SystemSections{
		{Name: "z", Value: strp("Z")},
		{Name: "2", Value: strp("two")},
		{Name: "1", Value: strp("one")},
		{Name: "a", Value: strp("A")},
		{Name: "01", Value: strp("zero-one")},
		{Name: "-1", Value: strp("neg")},
		{Name: "4294967295", Value: strp("max")},
		{Name: "4294967294", Value: strp("maxidx")},
	}
}

func TestGetSystemMessageTextFollowsObjectOrder(t *testing.T) {
	m := NewSystemText("base", 1)
	m.Sections = intLikeSections()
	const want = "base\n\none\n\ntwo\n\nmaxidx\n\nZ\n\nA\n\nzero-one\n\nneg\n\nmax"
	if got := GetSystemMessageText(m); got != want {
		t.Fatalf("GetSystemMessageText = %q, want %q", got, want)
	}
}

func TestGetSystemMessageTextDropsEmptyPartsAndRemovals(t *testing.T) {
	m := SystemMessage{Sections: SystemSections{{Name: "a", Value: strp("")}, {Name: "b"}, {Name: "c", Value: strp("C")}}}
	if got := GetSystemMessageText(m); got != "C" {
		t.Fatalf("GetSystemMessageText = %q, want %q", got, "C")
	}
	arrayContent := SystemMessage{
		Content:  ContentList{TextContent{Text: "a"}, TextContent{Text: "b"}},
		Sections: SystemSections{{Name: "s"}, {Name: "t", Value: strp("T")}},
	}
	if got := GetSystemMessageText(arrayContent); got != "a\nb\n\nT" {
		t.Fatalf("array content text = %q, want %q", got, "a\nb\n\nT")
	}
}

func TestRenderSystemMessageUpdateFraming(t *testing.T) {
	m := NewSystemText("base", 1)
	m.Sections = intLikeSections()
	const want = "base\n\n" +
		"Updated system prompt section \"1\":\n\none\n\n" +
		"Updated system prompt section \"2\":\n\ntwo\n\n" +
		"Updated system prompt section \"4294967294\":\n\nmaxidx\n\n" +
		"Updated system prompt section \"z\":\n\nZ\n\n" +
		"Updated system prompt section \"a\":\n\nA\n\n" +
		"Updated system prompt section \"01\":\n\nzero-one\n\n" +
		"Updated system prompt section \"-1\":\n\nneg\n\n" +
		"Updated system prompt section \"4294967295\":\n\nmax"
	if got := RenderSystemMessageUpdate(m); got != want {
		t.Fatalf("RenderSystemMessageUpdate\n got %q\nwant %q", got, want)
	}

	// Empty content is skipped, but an empty section value still renders its
	// frame (pi filters only the content part).
	empty := SystemMessage{Sections: SystemSections{{Name: "a", Value: strp("")}, {Name: "b"}}}
	const wantEmpty = "Updated system prompt section \"a\":\n\n\n\nRemoved system prompt section \"b\"."
	if got := RenderSystemMessageUpdate(empty); got != wantEmpty {
		t.Fatalf("RenderSystemMessageUpdate(empty parts)\n got %q\nwant %q", got, wantEmpty)
	}

	arrayContent := SystemMessage{
		Content:  ContentList{TextContent{Text: "a"}, TextContent{Text: "b"}},
		Sections: SystemSections{{Name: "s"}, {Name: "t", Value: strp("T")}},
	}
	const wantArray = "a\nb\n\nRemoved system prompt section \"s\".\n\nUpdated system prompt section \"t\":\n\nT"
	if got := RenderSystemMessageUpdate(arrayContent); got != wantArray {
		t.Fatalf("RenderSystemMessageUpdate(array content)\n got %q\nwant %q", got, wantArray)
	}
	if got := RenderSystemMessageUpdate(SystemMessage{}); got != "" {
		t.Fatalf("RenderSystemMessageUpdate(empty message) = %q, want empty", got)
	}
}

func TestSystemSectionsMapOperations(t *testing.T) {
	var s SystemSections
	s.Set("b", strp("1"))
	s.Set("10", strp("x"))
	s.Set("a", strp("2"))
	s.Set("b", strp("updated")) // existing name keeps its slot
	if got, ok := s.Get("b"); !ok || got == nil || *got != "updated" {
		t.Fatalf("Get(b) = %v, %v", got, ok)
	}
	if names := sectionNames(s.Entries()); names != "10,b,a" {
		t.Fatalf("entries = %s, want 10,b,a", names)
	}
	s.Delete("b")
	s.Set("b", strp("again")) // delete then set appends
	if names := sectionNames(s.Entries()); names != "10,a,b" {
		t.Fatalf("entries after delete+set = %s, want 10,a,b", names)
	}
	s.Set("gone", nil)
	if v, ok := s.Get("gone"); !ok || v != nil {
		t.Fatalf("Get(gone) = %v, %v, want a present removal", v, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get(missing) reported present")
	}
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4", s.Len())
	}

	// A repeated name in a literal keeps its first slot and its last value.
	dup := SystemSections{{Name: "a", Value: strp("1")}, {Name: "b", Value: strp("2")}, {Name: "a", Value: strp("3")}}
	if dup.Len() != 2 {
		t.Fatalf("Len of a duplicated literal = %d, want 2", dup.Len())
	}
	entries := dup.Entries()
	if sectionNames(entries) != "a,b" || *entries[0].Value != "3" {
		t.Fatalf("duplicated literal entries = %s (a=%v)", sectionNames(entries), entries[0].Value)
	}
	if v, _ := dup.Get("a"); v == nil || *v != "3" {
		t.Fatalf("Get on a duplicated literal = %v, want the last value", v)
	}
	dup.Delete("a")
	if dup.Len() != 1 {
		t.Fatalf("Delete left %d names, want 1", dup.Len())
	}
}

func sectionNames(entries []SystemSection) string {
	out := ""
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += e.Name
	}
	return out
}
