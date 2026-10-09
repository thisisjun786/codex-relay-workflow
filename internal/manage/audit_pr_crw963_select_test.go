package manage

import (
	"testing"
	"time"
)

// The selector itself: targets that never failed come first, newest first; failed ones come
// after them, the oldest last failure first; a target with three failures at its current
// head is left out and named, and failures at another head do not count.
func TestCRW963SelectOrdersFailedTargetsAfterFreshOnes(t *testing.T) {
	pattern, err := auditPRPattern(auditPRSection{})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	entries := []auditPRListEntry{
		auditPRListEntryOf(30, "CRW-30: newest, failed once", "2026-10-06T00:00:00Z", "m30"),
		auditPRListEntryOf(20, "CRW-20: failed longer ago", "2026-10-05T00:00:00Z", "m20"),
		auditPRListEntryOf(10, "CRW-10: fresh", "2026-10-02T00:00:00Z", "m10"),
		auditPRListEntryOf(40, "CRW-40: three at m40", "2026-10-07T00:00:00Z", "m40"),
		auditPRListEntryOf(50, "CRW-50: three at an old head", "2026-10-08T00:00:00Z", "m50"),
	}
	fail := func(target, head string) auditPRFailureRow {
		return auditPRFailureRow{Target: target, Head: head, At: "2026-10-09T00:00:00Z", Reason: "x"}
	}
	failures := []auditPRFailureRow{
		fail("pr-20", "m20"),
		fail("pr-40", "m40"), fail("pr-40", "m40"),
		fail("pr-50", "old"), fail("pr-50", "old"), fail("pr-50", "old"),
		fail("pr-30", "m30"),
		fail("pr-40", "m40"),
	}
	targets, skipped, err := auditPRSelect(entries, pattern, since, map[string]bool{}, failures, 9)
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, target := range targets {
		order = append(order, target.Number)
	}
	want := []int{50, 10, 20, 30}
	if len(order) != len(want) {
		t.Fatalf("selected %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("selected %v, want %v", order, want)
		}
	}
	if len(skipped) != 1 || skipped[0].Number != 40 || skipped[0].Head != "m40" || skipped[0].Failures != 3 {
		t.Errorf("skipped = %+v, want #40 at m40 after 3 failures", skipped)
	}
	capped, _, err := auditPRSelect(entries, pattern, since, map[string]bool{}, failures, 1)
	if err != nil || len(capped) != 1 || capped[0].Number != 50 {
		t.Errorf("--max 1 selected %+v (%v), want #50 (never failed at its head)", capped, err)
	}
	// A skipped target does not take a --max place.
	all, _, _ := auditPRSelect(entries, pattern, since, map[string]bool{}, failures, 4)
	if len(all) != 4 {
		t.Errorf("--max 4 selected %d targets, want 4 (the skipped one takes no place)", len(all))
	}
}
