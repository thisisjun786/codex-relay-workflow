package delivery

import (
	"encoding/json"
	"strconv"
	"testing"

	pabcdreview "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
)

// CRW-1116: the verdict line the relay renders is a line the PABCD sign-off parser reads back, with the same count.
func TestRenderedVerdictLineIsReadByTheSignoffParser(t *testing.T) {
	for _, n := range []int{1, 3, pabcdreview.MaxBlockers} {
		review := map[string]any{"kind": "GO-WITH-FIXES", "blockers": json.Number(strconv.Itoa(n))}
		report := map[string]any{"review": review}
		x := reviewIndex{review: review}
		sections := x.verdictSections(report)
		if len(sections) != 1 {
			t.Fatalf("%d sections", len(sections))
		}
		line := sections[0].lines[1]
		got := pabcdreview.ParseSignoff("LAUNCH: l\n" + line)
		if got == nil || got.Blockers != n || got.Verdict != "near-pass" {
			t.Fatalf("%q read back as %+v", line, got)
		}
	}
}
