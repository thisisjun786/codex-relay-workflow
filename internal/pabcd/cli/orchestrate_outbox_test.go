package cli

// The CRW-1097 suite of the orchestrate CLI: a transition whose state was published while its ledger
// row was not written - the append failed, or the writer stopped right after the publication - is
// recorded by the next locked call of the session, exactly once; the unbound and the all-done closes
// keep the same identity on a retry. The cases marked red reproduce on dev (6a5851f4f): the row is lost
// for good.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateOutboxUnblock removes the directory orchestrateCommitLedgerDirectory put where the
// ledger goes, so appends work again.
func orchestrateOutboxUnblock(t *testing.T, cwd string) {
	t.Helper()
	if err := os.Remove(filepath.Join(cwd, ".crw", "ledger.jsonl")); err != nil {
		t.Fatal(err)
	}
}

// orchestrateOutboxEdges is the from>to of every PABCD ledger row, in order.
func orchestrateOutboxEdges(t *testing.T, cwd string) []string {
	t.Helper()
	edges := []string{}
	for _, row := range orchestrateTransitionLedger(t, cwd) {
		from, _ := row["from"].(string)
		to, _ := row["to"].(string)
		edges = append(edges, from+">"+to)
	}
	return edges
}

// Red on dev: the three write paths answered the warning and the row was gone for good. Now the next
// locked call of the session (a reset here) records it first, so the ledger holds both transitions in
// the order the session went through them.
func TestOrchestrateCommitFailedRowIsRecordedByTheNextCall(t *testing.T) {
	cases := []struct {
		name, seed string
		argv       []string
		want       []string
	}{
		{"interview-override", `{"phase":"I"}`, []string{"P", "--attest", `{"from":"I","to":"P","did":"interview done","override":true}`}, []string{"I>P", "P>IDLE"}},
		{"reset", `{"phase":"P","orchestrationActive":true}`, []string{"reset"}, []string{"P>IDLE"}},
		{"ordinary-edge", `{"phase":"IDLE"}`, []string{"P"}, []string{"IDLE>P", "P>IDLE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd, id := orchestrateTransitionRoot(t), "outbox-"+tc.name
			orchestrateTransitionSession(t, cwd, id, tc.seed)
			orchestrateCommitLedgerDirectory(t, cwd)
			got := orchestrateCommitRunOK(t, cwd, nil, append(tc.argv[:1:1], append([]string{"--session", id}, tc.argv[1:]...)...)...)
			if got.Code != 0 || !strings.Contains(got.Output, "could not be written: ") {
				t.Fatalf("the failed row's answer: %+v", got)
			}
			orchestrateOutboxUnblock(t, cwd)
			orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
			if edges := orchestrateOutboxEdges(t, cwd); strings.Join(edges, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("the ledger after the next call: %v, want %v", edges, tc.want)
			}
		})
	}
}

// A writer that stops right after the publication - the panic stands in for a kill there; the session
// lock is released by its own defer, as a removed stale lock would be - leaves the row pending, and the
// next call records it once.
func TestOrchestrateCommitStoppedAfterPublishIsRecorded(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "outbox-stopped"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	stop := &orchestrateCommitSeams{writeState: func(cwd string, next state.State) error {
		if err := state.WriteState(cwd, next); err != nil {
			return err
		}
		panic("stopped after the publication")
	}}
	func() {
		defer func() { _ = recover() }()
		_, _ = orchestrateCommitTry(t, cwd, stop, "P", "--session", id)
	}()
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseP {
		t.Fatalf("the transition was not published: %+v", s)
	}
	if edges := orchestrateOutboxEdges(t, cwd); len(edges) != 0 {
		t.Fatalf("a stopped writer appended %v", edges)
	}
	orchestrateCommitRunOK(t, cwd, nil, "A", "--session", id) // refused (no plan unit) after the drain
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	if edges := orchestrateOutboxEdges(t, cwd); strings.Join(edges, " ") != "IDLE>P P>IDLE" {
		t.Fatalf("the ledger after the next calls: %v, want [IDLE>P P>IDLE]", edges)
	}
}

// Red on dev: the all-done close's row failed with the close reported as an error after the row was
// appended inside the first lock, or - when the append itself failed - the close never published. Now an
// append that fails after IDLE is published answers success with the pending warning, and the next call
// records the one row, with the null closed work phase and the session's check epoch.
func TestOrchestrateDcloseAllDoneRowSurvivesAFailedAppend(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "outbox-all-done", "outbox-all-done-plan"
	orchestrateDcloseCancelAllDoneAtC(t, cwd, id, slug)
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	// A ledger the close can read (its row guard does) but not append to.
	ledger := filepath.Join(cwd, ".crw", "ledger.jsonl")
	if err := os.WriteFile(ledger, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil || got.Code != 0 || !strings.Contains(got.Output, "warning: ledger row for C -> IDLE could not be written: ") {
		t.Fatalf("an all-done close whose row failed answered (%+v, %v); want success with the pending warning", got, err)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseIdle {
		t.Fatalf("the close was not published: %+v", s)
	}
	if err := os.Chmod(ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	rows := orchestrateTransitionLedger(t, cwd)
	if len(rows) != 1 || rows[0]["reason"] != "done" || rows[0]["checkEpoch"] != "c-test-epoch" {
		t.Fatalf("the ledger after the next calls: %+v, want exactly the all-done row", rows)
	}
	if v, present := rows[0]["closedWorkPhaseId"]; !present || v != nil {
		t.Fatalf("the all-done row's closedWorkPhaseId: %v (present %v), want a present null", v, present)
	}
}

// The all-done close stopped right after IDLE was published (a kill there) leaves its row pending; the
// next call records it once, with the same identity.
func TestOrchestrateDcloseAllDoneStoppedAfterPublishIsRecorded(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "outbox-all-done-stop", "outbox-all-done-stop-plan"
	orchestrateDcloseCancelAllDoneAtC(t, cwd, id, slug)
	func() {
		defer func() { _ = recover() }()
		_, _ = orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{writeState: func(cwd string, next state.State) error {
			if err := state.WriteState(cwd, next); err != nil {
				return err
			}
			panic("stopped after the publication")
		}})
	}()
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseIdle {
		t.Fatalf("the close was not published: %+v", s)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 0 {
		t.Fatalf("a stopped close appended %d row(s)", n)
	}
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows after the next calls = %d, want 1", n)
	}
}

// Status is a read (phase-control.md): it reports the pending ledger rows of the session it selects and records nothing - not the
// rows, not a cleanup - whichever way the session was selected, and a call that records them (any orchestrate command) still does.
// Red on 3fceb240, where status drained the selected session's outbox: through the unverified latest-file fallback it recorded the
// rows, and finished the plan-audit cleanup, of a session nobody had selected.
func TestOrchestrateStatusOnlyReportsAPendingRow(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "outbox-status"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	stop := &orchestrateCommitSeams{writeState: func(cwd string, next state.State) error {
		if err := state.WriteState(cwd, next); err != nil {
			return err
		}
		panic("stopped after the publication")
	}}
	func() {
		defer func() { _ = recover() }()
		_, _ = orchestrateCommitTry(t, cwd, stop, "P", "--session", id)
	}()
	outboxBytes := func() string {
		dir := state.StatePath(cwd, id) + ".ledger-outbox"
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out += entry.Name() + "\n" + string(raw)
		}
		return out
	}
	before := outboxBytes()
	for _, argv := range [][]string{{"status", "--session", id}, {"status"}, {"status", "--json"}} {
		read, err := RunOrchestrateRead(ParseOrchestrateCliArgs(argv, cwd), ReadEnv{})
		if err != nil || read.Result == nil || read.Result.Code != 0 || !strings.Contains(read.Result.Output, "phase=P") && !strings.Contains(read.Result.Output, `"phase":"P"`) {
			t.Fatalf("%v: %+v %v", argv, read, err)
		}
		if !strings.Contains(read.Result.Output, "pending") {
			t.Fatalf("%v: the answer does not report the pending row: %q", argv, read.Result.Output)
		}
		if edges := orchestrateOutboxEdges(t, cwd); len(edges) != 0 {
			t.Fatalf("%v: status recorded %v", argv, edges)
		}
		if got := outboxBytes(); got != before {
			t.Fatalf("%v: status changed the outbox", argv)
		}
	}
	// The next orchestrate command of the session records it.
	orchestrateCommitRunOK(t, cwd, nil, "status", "--session", id)
	orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", id)
	if edges := orchestrateOutboxEdges(t, cwd); strings.Join(edges, " ") != "IDLE>P P>IDLE" {
		t.Fatalf("the ledger after the next command: %v", edges)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, id); len(pending) != 0 {
		t.Fatalf("pending after the next command: %+v", pending)
	}
}

// CRW-1109 fix round: `reset --attest --cwd=<elsewhere>` is a parse error, so the reset never runs against the input workspace.
func TestOrchestrateResetRefusesASwallowedCwd(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "swallow-reset"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P","orchestrationActive":true}`)
	parsed := ParseOrchestrateCliArgs([]string{"reset", "--session", id, "--attest", "--cwd=" + t.TempDir()}, cwd)
	if parsed.Error == nil {
		t.Fatalf("the swallowing form was parsed: %+v", parsed.Args)
	}
	read, err := RunOrchestrateRead(parsed, ReadEnv{})
	if err != nil || read.Result == nil || read.Result.Code == 0 {
		t.Fatalf("the read entry did not refuse: %+v %v", read, err)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseP {
		t.Fatalf("the input workspace's session moved: %+v", s)
	}
}

// Verification round 2: P>A, A>P and P>A all applied while the ledger cannot be appended to. Each call's drain stops at the oldest
// blocked row and still records that it published its own, so the next call that can append records all three, in order (red at
// d9a805df: the ledger held P>A P>A, the round trip's A>P was dropped because the phase was back at its pre-phase).
func TestOrchestrateRoundTripsDuringALedgerFailureAreAllRecorded(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "outbox-roundtrip"
	unit := orchestrateReplanSeed(t, cwd, id)
	orchestrateCommitLedgerDirectory(t, cwd)
	for _, argv := range [][]string{
		{"A", "--session", id, "--attest", orchestrateReplanAttest(unit)},
		{"P", "--session", id},
		{"A", "--session", id, "--attest", orchestrateReplanAttest(unit)},
	} {
		if got, err := orchestrateCommitTry(t, cwd, nil, argv...); err != nil || got.Code != 0 {
			t.Fatalf("%v: %+v %v", argv, got, err)
		}
	}
	orchestrateOutboxUnblock(t, cwd)
	// The next orchestrate command (refused: A>C is no edge) records the rows first; status would only report them.
	orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id)
	if edges := strings.Join(orchestrateOutboxEdges(t, cwd), " "); edges != "P>A A>P P>A" {
		t.Fatalf("the ledger: %q, want P>A A>P P>A", edges)
	}
	if pending, _, _ := state.PendingLedgerEvents(cwd, id); len(pending) != 0 {
		t.Fatalf("pending after the next command: %+v", pending)
	}
}

// Verification round 2: `reset --attest` and `reset --attest-file` with no value are parse errors, so the reset refuses and the
// session's bytes stay as they were (red at d9a805df: both parsed, the reset ran and the session went to IDLE).
func TestOrchestrateResetRefusesAMissingAttestValue(t *testing.T) {
	for _, flag := range []string{"--attest", "--attest-file"} {
		t.Run(flag, func(t *testing.T) {
			cwd, id := orchestrateTransitionRoot(t), "missing-attest"
			orchestrateTransitionSession(t, cwd, id, `{"phase":"P","orchestrationActive":true}`)
			before, err := os.ReadFile(state.StatePath(cwd, id))
			if err != nil {
				t.Fatal(err)
			}
			parsed := ParseOrchestrateCliArgs([]string{"reset", "--session", id, flag}, cwd)
			if parsed.Error == nil {
				t.Fatalf("the missing value was parsed: %+v", parsed.Args)
			}
			got, err := orchestrateCommitTry(t, cwd, nil, "reset", "--session", id, flag)
			if err != nil || got.Code == 0 {
				t.Fatalf("the reset did not refuse: %+v %v", got, err)
			}
			if after, err := os.ReadFile(state.StatePath(cwd, id)); err != nil || string(after) != string(before) {
				t.Fatalf("the session changed: %v\n%s", err, after)
			}
		})
	}
}
