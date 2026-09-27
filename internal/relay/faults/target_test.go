package faults

import (
	"testing"
)

func Test22_FLT_11_WritesFollowCurrentTarget(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	record(t, l, c, o)
	id := FaultID("crw", o.FaultClass, o.Signature)
	r, e := l.Store.One(c, "SELECT tracker_ref FROM fault_publications WHERE fault_id = ?", id)
	if e != nil || r.Get("tracker_ref") != nil {
		t.Fatalf("write did not wait for target: %+v %v", r, e)
	}
	answer, e := l.SetTarget(c, "crw", "CRW", "team-new", "proj-CRW")
	if e != nil || answer["backfilled"] != int64(1) {
		t.Fatalf("target not backfilled: %+v %v", answer, e)
	}
	r, e = l.Store.One(c, "SELECT tracker_ref FROM fault_publications WHERE fault_id = ?", id)
	if e != nil || text(r, "tracker_ref") != "team-new" {
		t.Fatalf("not repointed: %+v %v", r, e)
	}
}
func Test22_FLT_12_UnconfiguredTargetWaits(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	record(t, l, c, o)
	id := FaultID("crw", o.FaultClass, o.Signature)
	if publicationAnswer(c, l, id, true).(map[string]any)["awaitingTarget"] != true {
		t.Fatal("unconfigured target not held")
	}
	if _, err := l.SetTarget(c, "crw", "CRW", "team-relay", "proj-CRW"); err != nil {
		t.Fatal(err)
	}
	if publicationAnswer(c, l, id, true).(map[string]any)["awaitingTarget"] != false {
		t.Fatal("configured target still held")
	}
}
