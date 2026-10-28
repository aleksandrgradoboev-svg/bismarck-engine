package coding

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

func usageTotalsFixture(total float64, fromProvider ai.OptionalBool) ai.Usage {
	return ai.Usage{Input: 1, Output: 1, TotalTokens: 2, Cost: ai.CostBreakdown{Total: total, TotalFromProvider: fromProvider}}
}

// pi's usage-totals.test.ts (7c22a93d): a combined total is provider-reported
// only when both are, and the flag is always written.
func TestCombineUsageMarksTheTotalProviderReportedOnlyWhenBothAre(t *testing.T) {
	if got := combineUsage(usageTotalsFixture(1, ai.True), usageTotalsFixture(2, ai.True)).Cost; got.Total != 3 || got.TotalFromProvider != ai.True {
		t.Fatalf("both reported: %+v", got)
	}
	for _, tc := range []struct{ first, second ai.OptionalBool }{
		{ai.True, ai.Absent},
		{ai.Absent, ai.True},
		{ai.Absent, ai.Absent},
	} {
		if got := combineUsage(usageTotalsFixture(1, tc.first), usageTotalsFixture(2, tc.second)).Cost.TotalFromProvider; got != ai.False {
			t.Fatalf("combine(%v, %v) flag = %v, want false", tc.first, tc.second, got)
		}
	}
}

// The recorder keeps the first usage as it is and combines the rest, as pi's
// NestedToolCallRecorder.addUsage does: one call's provider-reported total
// stays so, and the optional token splits are summed.
func TestNestedRecorderCombinesUsageLikePi(t *testing.T) {
	r := newNestedCallRecorder()
	first := usageTotalsFixture(1, ai.True)
	first.CacheWrite1h, first.Reasoning = 3, 4
	r.addUsage(first)
	if r.usage.Cost.TotalFromProvider != ai.True {
		t.Fatalf("one usage: %+v", r.usage.Cost)
	}
	r.addUsage(usageTotalsFixture(2, ai.False))
	if got := *r.usage; got.Cost.Total != 3 || got.Cost.TotalFromProvider != ai.False || got.CacheWrite1h != 3 || got.Reasoning != 4 || got.Input != 2 {
		t.Fatalf("combined: %+v", got)
	}
	r.addUsage(usageTotalsFixture(0, ai.True))
	if got := *r.usage; got.CacheWrite1h != 3 || got.Cost.TotalFromProvider != ai.False {
		t.Fatalf("combined twice: %+v", got)
	}
}
