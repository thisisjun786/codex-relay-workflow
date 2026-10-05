package evidence

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-648: RecordTombstone and ResolveTombstone read the session file with the strict reader and write back what it rebuilt, so
// a D-close recovery marker the reader restored as Legacy must keep the distinction across such a write. The port persists the
// marker as nextWorkPhaseId:null with legacy:true and the reader takes that stored flag; the oracle loses it here (a loss of
// state fixed under the parity rule revision of 2026-10-03). Both cases are red on dev: the marker reads back as Legacy:false.

// dcloseLegacySeed writes the issue's session file: a D-close marker whose successor is a number, which the reader restores as
// Legacy with no successor. list is the stored unverifiedSubagents array, verbatim.
func dcloseLegacySeed(t *testing.T, cwd, list string) {
	t.Helper()
	put(t, state.StatePath(cwd, "s1"), []byte(`{"phase":"IDLE","sessionId":"s1","checkEpoch":"e","dcloseRecovery":{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":7},"unverifiedSubagents":`+list+`}`))
}

// dcloseLegacyKept fails unless the stored marker still reads as a refused Legacy marker with no successor.
func dcloseLegacyKept(t *testing.T, cwd string) {
	t.Helper()
	m := state.ReadState(cwd, "s1").DcloseRecovery
	if m == nil || !m.Legacy || m.NextWorkPhaseID != nil {
		t.Fatalf("the marker lost its legacy distinction: %+v", m)
	}
}

func TestRecordTombstoneKeepsALegacyDcloseMarker(t *testing.T) {
	cwd := t.TempDir()
	dcloseLegacySeed(t, cwd, "[]")
	if !RecordTombstone(cwd, "s1", Payload{AgentID: "a1", TurnID: "t1", AgentType: "executor"}, 3, WriteUnrecordableMarker) {
		t.Fatal("the tombstone was not recorded")
	}
	dcloseLegacyKept(t, cwd)
}

func TestResolveTombstoneKeepsALegacyDcloseMarker(t *testing.T) {
	cwd := t.TempDir()
	dcloseLegacySeed(t, cwd, `[{"agentId":"a1","turnId":"t1","agentType":"executor","attempts":3,"receiptClaimed":"none","recordedAt":"2026-01-01T00:00:00.000Z","resolvable":true}]`)
	if !ResolveTombstone(cwd, "s1", Payload{AgentID: "a1", TurnID: "t1"}) {
		t.Fatal("the tombstone was not resolved")
	}
	dcloseLegacyKept(t, cwd)
}
