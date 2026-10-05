package state

import "testing"

// CRW-648: a D-close recovery marker the reader restores as Legacy must keep the distinction across a state write. The port
// persists such a marker as nextWorkPhaseId:null with legacy:true, so the next read must take the stored flag, not the null,
// as the answer; the oracle never reads the key it writes and loses the distinction (a loss of state fixed under the parity
// rule revision of 2026-10-03).

// TestDcloseLegacySurvivesAWriteAndRead is the issue's first case: a marker whose successor is a number reads as Legacy, and
// a write and read keeps it. It is red on dev: the second read answers Legacy:false, an explicit "no successor".
func TestDcloseLegacySurvivesAWriteAndRead(t *testing.T) {
	cwd := t.TempDir()
	putIn(t, cwd, "s1", `{"phase":"IDLE","sessionId":"s1","checkEpoch":"e","dcloseRecovery":{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":7},"unverifiedSubagents":[]}`)
	s, unreadable := ReadStateStrict(cwd, "s1")
	if unreadable || s.DcloseRecovery == nil || !s.DcloseRecovery.Legacy || s.DcloseRecovery.NextWorkPhaseID != nil {
		t.Fatalf("first read: %+v unreadable=%v", s.DcloseRecovery, unreadable)
	}
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	again, unreadable := ReadStateStrict(cwd, "s1")
	if unreadable || again.DcloseRecovery == nil || !again.DcloseRecovery.Legacy || again.DcloseRecovery.NextWorkPhaseID != nil {
		t.Fatalf("after a write: %+v unreadable=%v", again.DcloseRecovery, unreadable)
	}
}

// dcloseLegacyRestored reads a session whose dcloseRecovery holds marker, so the returned pointer is the reader's answer for it.
func dcloseLegacyRestored(t *testing.T, marker string) *DcloseRecoveryMarker {
	t.Helper()
	cwd := t.TempDir()
	putIn(t, cwd, "s1", `{"phase":"IDLE","sessionId":"s1","checkEpoch":"e","dcloseRecovery":`+marker+`,"unverifiedSubagents":[]}`)
	s, unreadable := ReadStateStrict(cwd, "s1")
	if unreadable {
		t.Fatalf("unreadable: %s", marker)
	}
	return s.DcloseRecovery
}

// TestDcloseLegacyStoredShapes is the issue's fourth case: only a boolean legacy true is authoritative, so a null successor
// without a legacy key, with legacy false or with a legacy string stays an explicit "no successor", and a string successor is
// still a successor. The two stored legacy true shapes are red on dev: the null one restores Legacy:false, the successor one
// keeps the successor.
func TestDcloseLegacyStoredShapes(t *testing.T) {
	for _, c := range []struct {
		name   string
		marker string
		legacy bool
		next   string
	}{
		{"null successor without a legacy key", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":null}`, false, ""},
		{"null successor with legacy false", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":null,"legacy":false}`, false, ""},
		{"null successor with a legacy string", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":null,"legacy":"yes"}`, false, ""},
		{"a string successor", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":"wp2"}`, false, "wp2"},
		{"a stored legacy true over a null", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":null,"legacy":true}`, true, ""},
		{"a stored legacy true over a successor", `{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":"wp2","legacy":true}`, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := dcloseLegacyRestored(t, c.marker)
			if m == nil {
				t.Fatalf("no marker for %s", c.marker)
			}
			next := ""
			if m.NextWorkPhaseID != nil {
				next = *m.NextWorkPhaseID
			}
			if m.Legacy != c.legacy || next != c.next {
				t.Fatalf("legacy=%v next=%q, want legacy=%v next=%q", m.Legacy, next, c.legacy, c.next)
			}
		})
	}
}
