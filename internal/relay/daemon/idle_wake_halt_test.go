package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-904 (correction, d1): a store failure the idle pass could not apply reaches the daemon's own
// halt instead of becoming a note. The halt is CRW-848's: the daemon marks the store from its own
// write and from its own read, ends the pass without an error, and every later pass is write-free
// (I-564). The pass has two sites of its own, and each is handed over with the failure that was seen
// there. This file pins the wiring: the failure reaches the halt, and the halt's answer decides
// whether the tick ends. The marker itself is CRW-848's, proved by its own tests.

// idleWakeHalt records what the pass handed to the daemon's halt and answers what the halt answers.
type idleWakeHalt struct {
	calls  []idleWakeHaltCall
	answer bool
}

type idleWakeHaltCall struct {
	site string
	err  error
}

// idleWakeHaltDamage breaks the tables the idle pass writes and reads, so the pass's own statement
// fails with a store error the way a damaged store answers one. keepWake leaves the wake table in a
// shape its insert refuses (original_deadline is missing), so the failure is the pass's own write
// while the head read still answers; without it the wake table is gone too, so the head read is the
// failure instead.
func idleWakeHaltDamage(t *testing.T, s *store.Store, keepWake bool) {
	t.Helper()
	exec(t, s, "DROP TABLE delivery_wakes")
	if keepWake {
		exec(t, s, "CREATE TABLE delivery_wakes (event_id TEXT PRIMARY KEY, spent_at TEXT)")
		return
	}
	exec(t, s, "ALTER TABLE deliveries RENAME TO deliveries_intact")
}

func idleWakeHaltDaemon(t *testing.T, host *idleHost, halt *idleWakeHalt) (*Daemon, *store.Store) {
	t.Helper()
	d, s := idleDaemon(t, host)
	d.idle.halt = func(_ context.Context, _ *Report, site string, err error) bool {
		halt.calls = append(halt.calls, idleWakeHaltCall{site: site, err: err})
		return halt.answer
	}
	return d, s
}

// idleWakeHaltNote is what the pass wrote as the note for a failure it could not apply.
func idleWakeHaltNote(report Report, needle string) int {
	n := 0
	for _, note := range report.Notes {
		if strings.Contains(note, needle) {
			n++
		}
	}
	return n
}

// TestIdleWakeHalt_a_failed_wake_write_reaches_the_daemons_halt: the wake is the pass's own write, so
// a store failure there is the write site, and the halt's answer ends the tick before the delivery
// pass runs.
func TestIdleWakeHalt_a_failed_wake_write_reaches_the_daemons_halt(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: true}
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, s := idleWakeHaltDaemon(t, host, halt)
	idleWakeHaltDamage(t, s, true)
	report, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("the tick returned an error instead of ending at the halt: %v", err)
	}
	if len(halt.calls) != 1 || halt.calls[0].site != idleHaltSiteWrite {
		t.Fatalf("the halt was handed %+v, want one call at the write site", halt.calls)
	}
	if halt.calls[0].err == nil {
		t.Fatal("the halt was handed no failure")
	}
	// The halt ended the tick: the delivery pass that follows the idle pass did not run, so the
	// recipient's waiting head was not attempted.
	if n := busyAnswers(t, s); n != 0 {
		t.Fatalf("the delivery pass ran after the halt (%d busy answers)", n)
	}
	if n := idleWakeHaltNote(report, "not applied"); n != 1 {
		t.Fatalf("the tick reported the wake failure %d times, want once: %v", n, report.Notes)
	}
}

// TestIdleWakeHalt_a_failed_head_read_is_the_observation_site: the waiting heads are read out of the
// store, so a failure there is an observation, exactly as the observation pass's own census read is.
// The pass is driven directly because a store that cannot answer this read cannot answer the delivery
// pass either, and the site this file pins is the one the hold's own read is recorded at.
func TestIdleWakeHalt_a_failed_head_read_is_the_observation_site(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: true}
	d, s := idleWakeHaltDaemon(t, &idleHost{}, halt)
	idleWakeHaltDamage(t, s, false)
	w := d.idle
	w.begin()
	report := Report{Notes: []string{}}
	w.hold(context.Background(), &report, &idleHost{}, idleNow)
	if !w.halted {
		t.Fatal("the pass did not end at the halt for its unapplied head read")
	}
	report.Notes = append(report.Notes, w.take()...)
	if len(halt.calls) != 1 || halt.calls[0].site != idleHaltSiteObservation {
		t.Fatalf("the halt was handed %+v, want one call at the observation site", halt.calls)
	}
	if halt.calls[0].err == nil {
		t.Fatal("the halt was handed no failure")
	}
	if n := idleWakeHaltNote(report, "busy heads not read"); n != 1 {
		t.Fatalf("the tick reported the head read failure %d times, want once: %v", n, report.Notes)
	}
}

// TestIdleWakeHalt_a_failure_the_halt_does_not_claim_keeps_its_note: a store failure that is not the
// halting class leaves the pass exactly as it was, with the note the pass wrote. The pass is driven
// directly, as in the observation-site case: what this pins is the pass's own decision, not what a
// broken store does to the rest of the tick.
func TestIdleWakeHalt_a_failure_the_halt_does_not_claim_keeps_its_note(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: false}
	d, s := idleWakeHaltDaemon(t, &idleHost{}, halt)
	idleWakeHaltDamage(t, s, false)
	w := d.idle
	w.begin()
	report := Report{Notes: []string{}}
	w.hold(context.Background(), &report, &idleHost{}, idleNow)
	if w.halted {
		t.Fatal("the pass ended at a halt that did not claim its failure")
	}
	if len(halt.calls) != 1 {
		t.Fatalf("the halt was handed %d failures, want the one the pass could not apply", len(halt.calls))
	}
	report.Notes = append(report.Notes, w.take()...)
	if n := idleWakeHaltNote(report, "busy heads not read"); n != 1 {
		t.Fatalf("the note the pass wrote was lost or repeated (%d): %v", n, report.Notes)
	}
}

// TestIdleWakeHalt_no_failure_leaves_the_halt_alone: a pass that applied everything hands the halt
// nothing, so a healthy tick is never marked.
func TestIdleWakeHalt_no_failure_leaves_the_halt_alone(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: true}
	d, _ := idleWakeHaltDaemon(t, &idleHost{}, halt)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(halt.calls) != 0 {
		t.Fatalf("a healthy tick handed the halt %+v", halt.calls)
	}
}

// TestIdleWakeHalt_the_daemons_own_halt_is_the_one_wired: the pass reaches the halt through the
// method the daemon satisfies, so a build whose daemon has one wires it and a build whose daemon does
// not leaves the field nil rather than failing to build. A failure that is not the halting class is
// never claimed, which is what keeps an ordinary store error from marking the store.
func TestIdleWakeHalt_the_daemons_own_halt_is_the_one_wired(t *testing.T) {
	t.Parallel()
	d, _ := idleDaemon(t, &idleHost{})
	if _, has := any(d).(interface {
		halted(context.Context, *Report, string, error) bool
	}); has && d.idle.halt == nil {
		t.Fatal("the daemon has a halt and the pass did not take it")
	}
	if d.idle.halt != nil {
		if d.idle.halt(context.Background(), &Report{}, idleHaltSiteWrite, errors.New("probe")) {
			t.Fatal("the wired halt claimed a failure outside its class")
		}
	}
}

// TestIdleWakeHalt_a_batch_of_reports_stops_at_the_detection: the halt is consulted as each failure
// happens, so a report batch does not keep writing after a corrupting one. The first report's wake
// fails, the halt stands, and the second report issues no statement at all (I-564).
func TestIdleWakeHalt_a_batch_of_reports_stops_at_the_detection(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: true}
	host := &idleHost{reports: []delivery.IdleReport{
		{ThreadID: idleThread, Status: "idle"},
		{ThreadID: "second-thread", Status: "idle"},
	}}
	d, s := idleWakeHaltDaemon(t, host, halt)
	idleWakeHaltDamage(t, s, true)
	report := Report{Notes: []string{}}
	d.idle.begin()
	d.idle.idle(context.Background(), &report, host, idleNow)
	if !d.idle.halted {
		t.Fatal("the pass did not end at the halt for its failed wake write")
	}
	if len(halt.calls) != 1 {
		t.Fatalf("the halt was handed %d failures, want the one detection", len(halt.calls))
	}
	// The second report wrote nothing: only the first report's failure is noted, and the second is
	// not attempted, so no statement follows the halt.
	report.Notes = append(report.Notes, d.idle.take()...)
	if n := idleWakeHaltNote(report, "not applied"); n != 1 {
		t.Fatalf("the batch reported %d wake failures, want only the detection: %v", n, report.Notes)
	}
}

// TestIdleWakeHalt_a_later_failure_is_not_masked: the failure the pass hands to the halt is the one it
// has just met, not the first one it met. Keeping the first would let an earlier ordinary failure (a
// locked database, say) hide a later corrupting one, and the store would take the writes of the
// passes after it (CRW-904 correction, d1).
func TestIdleWakeHalt_a_later_failure_is_not_masked(t *testing.T) {
	t.Parallel()
	halt := &idleWakeHalt{answer: true}
	d, _ := idleWakeHaltDaemon(t, &idleHost{}, halt)
	w := d.idle
	w.begin()
	first := errors.New("SQL logic error: database is locked (5)")
	second := errors.New("SQL logic error: database disk image is malformed (11)")
	w.unapplied(idleHaltSiteObservation, first)
	w.unapplied(idleHaltSiteWrite, second)
	if !w.haltStore(context.Background(), &Report{}) {
		t.Fatal("the pass did not reach the halt for the failure it had just met")
	}
	if len(halt.calls) != 1 || halt.calls[0].err != second || halt.calls[0].site != idleHaltSiteWrite {
		t.Fatalf("the halt was handed %+v, want the later failure at the write site", halt.calls)
	}
}
