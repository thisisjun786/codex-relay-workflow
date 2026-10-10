package spawn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// CRW-1122 and CRW-1118 (verification round 1): an answer the hook could not write is not a success. The leg ends with the blocking
// status, so the host does not run the spawn on a missing answer; what the event committed stays (the issued attempt, the spent grant
// and the event's record), so the same call delivered again is answered as the first time and nothing is issued or minted twice.

// spawnLegSink keeps what is written to it and fails after limit bytes (all of them when limit is zero).
type spawnLegSink struct {
	got   bytes.Buffer
	limit int
	short bool // a short write without an error, not an error
}

func (s *spawnLegSink) Write(p []byte) (int, error) {
	n := min(s.limit, len(p))
	s.got.Write(p[:n])
	if n < len(p) && s.short {
		return n, nil
	}
	if n < len(p) {
		return n, errors.New("synthetic write failure")
	}
	return n, nil
}

func TestSpawnLegFailedOutputIsNotSuccessAndTheCallIsAnsweredAgain(t *testing.T) {
	const message = "[CRW-DISPATCH:one:att-1]\nTASK: build"
	for _, sink := range []func() *spawnLegSink{
		func() *spawnLegSink { return &spawnLegSink{} },
		func() *spawnLegSink { return &spawnLegSink{limit: 20} },
		func() *spawnLegSink { return &spawnLegSink{limit: 20, short: true} },
	} {
		rig, ledger := spawnManagedDeepRig(t)
		payload := spawnManagedDeepPayload(rig.ws, message, 0)
		out := sink()
		if code := RunHook(context.Background(), strings.NewReader(payload), out, rig.env); code == 0 {
			t.Fatal("a failed write ended with success")
		}
		if data, _ := os.ReadFile(ledger); !strings.Contains(string(data), `"spawnIssued": true`) && !strings.Contains(string(data), `"spawnIssued":true`) {
			t.Fatalf("the attempt was not issued: %s", data)
		}
		var again bytes.Buffer
		if code := RunHook(context.Background(), strings.NewReader(payload), &again, rig.env); code != 0 || !strings.Contains(again.String(), `"model":"rec/exec-primary"`) {
			t.Fatalf("the same call again: exit %d, %.300q", code, again.String())
		}
	}
}

// A grant minted by an event whose answer was lost is the grant the same event gets again, and a subagent's spent grant answers the
// call that spent it.
func TestSpawnLegFailedOutputKeepsTheEventsGrant(t *testing.T) {
	rig := spawnReapplyRig(t, "")
	payload := spawnReapplyPayload(rig.ws, "same-call", `{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate"}`)
	lost := &spawnLegSink{limit: 40}
	if code := RunHook(context.Background(), strings.NewReader(payload), lost, rig.env); code == 0 {
		t.Fatal("a failed write ended with success")
	}
	if got := spawnLegAnswer(t, rig, payload); got == "" || !strings.Contains(got, "[CRW-SUBSPAWN-GRANT:") {
		t.Fatalf("the same event again = %.300q", got)
	}

	rig, marker, _ := spawnGrantClaimRig(t)
	payload = spawnGrantClaimPayload(rig.ws, "call-1", "TASK: work "+marker)
	if code := RunHook(context.Background(), strings.NewReader(payload), &spawnLegSink{}, rig.env); code == 0 {
		t.Fatal("a failed write ended with success")
	}
	if got := spawnLegAnswer(t, rig, payload); spawnGrantClaimDenied(got) || got == "" {
		t.Fatalf("the spending call again = %.300q", got)
	}
}
