package goalplan

import (
	"encoding/json"
	"testing"
)

// CRW-1116: a round's recorded blocker count and finding references survive a read of the stored plan; one that is out of
// range or ill-typed is dropped, not repaired.
func TestReviveLaneKeepsBlockersAndFindings(t *testing.T) {
	lane := func(extra string) *ReviewLane {
		var raw any
		if err := json.Unmarshal([]byte(`{"launchId":"l","verdict":"near-pass"`+extra+`}`), &raw); err != nil {
			t.Fatal(err)
		}
		return reviveLane(raw)
	}
	got := lane(`,"blockers":2,"findings":["c1","r2"]`)
	if got == nil || got.Blockers != 2 || len(got.Findings) != 2 || got.Findings[0] != "c1" || got.Findings[1] != "r2" {
		t.Fatalf("%+v", got)
	}
	for _, extra := range []string{`,"blockers":0`, `,"blockers":-1`, `,"blockers":10000`, `,"blockers":1.5`, `,"blockers":"2"`, `,"findings":"c1"`, `,"findings":[1]`, `,"findings":[""]`} {
		if got := lane(extra); got == nil || got.Blockers != 0 || len(got.Findings) != 0 {
			t.Fatalf("%s: %+v", extra, got)
		}
	}
}
