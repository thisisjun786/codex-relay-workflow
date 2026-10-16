package supervisor

import (
	"fmt"
	"testing"
)

// TestAttemptCarriesByIDThroughRestatement pins who asks the hold rule as a send by id (CRW-1500,
// plan-r3 section 2): an attempt that names its message (Channel.Attempt, the CLI's supervisor-send
// --message) reads it as send_by_id, the daemon's AutoSend reads it as restage, and the retry an
// attempt makes after its proposal was restated at the transport start reads again with the entry
// of the attempt that made it. The channel's onReadEntry hook reports the entry once per read.
//
// The restated fixture is TestSweep24b_D7RestatesCurrentClaimAtTransportStart's scene: a second
// submission lands once, at the transport start, so every caller restates once and its retry reads
// again; a dropped entry in the retry would read [send_by_id, restage]. The plain fixture has no
// such hook, so each caller reads exactly once.
func TestAttemptCarriesByIDThroughRestatement(t *testing.T) {
	const correction = `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','corrected during claim','merge','2023-11-14T22:13:20.000000+00:00')`
	for _, tc := range []struct {
		name     string
		restated bool
		byID     bool
		want     []holdEntry
	}{
		{"restated/by_id", true, true, []holdEntry{entrySendByID, entrySendByID}},
		{"restated/autosend", true, false, []holdEntry{entryRestage, entryRestage}},
		{"plain/by_id", false, true, []holdEntry{entrySendByID}},
		{"plain/autosend", false, false, []holdEntry{entryRestage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture24(t)
			id := stagedID(t, f)
			r := newBRun(t, f)
			restatedOnce := false
			if tc.restated {
				f.c.beforeTransport = func() {
					sweepExec(t, f, correction)
					restatedOnce = true
					f.c.beforeTransport = nil
				}
			}
			var read []holdEntry
			f.c.onReadEntry = func(entry holdEntry) { read = append(read, entry) }
			if tc.byID {
				if _, err := f.c.Attempt(f.ctx, id, r.h, r.now); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.c.AutoSend(f.ctx, r.h, r.now, 0, 1, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.restated && !restatedOnce {
				t.Fatal("the transport never started, so nothing was restated")
			}
			if fmt.Sprint(read) != fmt.Sprint(tc.want) {
				t.Fatalf("the attempt read the message as %v, want %v", read, tc.want)
			}
		})
	}
}
