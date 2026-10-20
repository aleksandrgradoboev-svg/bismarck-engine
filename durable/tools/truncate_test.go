package tools

import (
	"strings"
	"testing"
)

// Ported from pi packages/durable/test/env-truncate.test.ts. n/a: "counts
// UTF-8 bytes and truncates correctly in a runtime without Buffer" — Go has
// one UTF-8 implementation, and its strings cannot hold the lone surrogates
// the case feeds.

func TestTruncateReportsUTF8ByteCounts(t *testing.T) {
	content := "aé🙂\nb"
	result := TruncateHead(content, TruncationOptions{MaxBytes: 100, MaxLines: 10})
	if result.Truncated || result.TotalBytes != 9 || result.OutputBytes != 9 {
		t.Fatalf("result = %+v, want untruncated with 9 bytes", result)
	}
}

func TestTruncateDoesNotCountATrailingNewlineAsAnExtraLine(t *testing.T) {
	content := strings.Repeat("line\n", 3)
	head := TruncateHead(content, TruncationOptions{MaxBytes: 100, MaxLines: 3})
	if head.Truncated || head.TotalLines != 3 || head.OutputLines != 3 {
		t.Fatalf("head = %+v, want 3 untruncated lines", head)
	}
}

func TestTruncateHeadByLineLimits(t *testing.T) {
	head := TruncateHead("one\ntwo\nthree\nfour", TruncationOptions{MaxBytes: 100, MaxLines: 2})
	if head.Content != "one\ntwo" || !head.Truncated || head.TruncatedBy != TruncatedByLines || head.TotalLines != 4 || head.OutputLines != 2 {
		t.Fatalf("head = %+v", head)
	}
}

func TestTruncateReportsBytesWhenOnlyATrailingNewlineExceedsLimitsAtTheLineCap(t *testing.T) {
	head := TruncateHead("hello\nworld\n", TruncationOptions{MaxBytes: 11, MaxLines: 2})
	if head.Content != "hello\nworld" || !head.Truncated || head.TruncatedBy != TruncatedByBytes || head.TotalLines != 2 || head.OutputLines != 2 {
		t.Fatalf("head = %+v", head)
	}
}

func TestTruncateHeadOnUTF8ByteLimitsWithoutPartialLines(t *testing.T) {
	result := TruncateHead("éé\nabc", TruncationOptions{MaxBytes: 4, MaxLines: 10})
	if result.Content != "éé" || !result.Truncated || result.TruncatedBy != TruncatedByBytes || result.OutputBytes != 4 || result.FirstLineExceedsLimit {
		t.Fatalf("result = %+v", result)
	}
}

func TestTruncateReportsHeadTruncationWhenTheFirstLineExceedsTheByteLimit(t *testing.T) {
	result := TruncateHead("éé\nabc", TruncationOptions{MaxBytes: 3, MaxLines: 10})
	if result.Content != "" || !result.Truncated || result.TruncatedBy != TruncatedByBytes || !result.FirstLineExceedsLimit {
		t.Fatalf("result = %+v", result)
	}
}

func TestTruncateFormatsSizes(t *testing.T) {
	for bytes, want := range map[int]string{1023: "1023B", 1536: "1.5KB", 3 * 1024 * 1024: "3.0MB"} {
		if got := FormatSize(bytes); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}
