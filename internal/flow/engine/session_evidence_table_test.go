package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// CRW-1496: one record-lifetime invariant for a flow evidence assignment. The engine writes a record under a directory only once a
// durable row of the call's newest generation names that directory (evidencePlace, the evidence_placed row), and every release and
// restore reads the directories from the rows. TestSessionEvidenceLifetimeTable runs the invariant over its entry paths (prepare,
// publish, bind, held resume, supersede, re-prepare, other-cwd resume) and death points (before and after evidence_placed, after
// evidence_placed with no file, before and after step_published, thread_started and turn_started): after the recovery no open
// record is left under EvidenceCwd, the owner's cwd or the host cwd, and on a path that goes on the bound child's receipt is
// accepted at its host cwd. A row is either a body or a skip reason that names where its scene is run.

// evidenceLifetimeRow is one scene of the table: entry path, death point, and a body that sets the scene up, recovers it and returns
// the directories besides EvidenceCwd that must hold no open record; or a skip reason.
type evidenceLifetimeRow struct {
	name, entry, death string
	body               func(t *testing.T, k *evidenceKit) []string
	skip               string
}

// evidenceExeAgain is the executor call of evidenceExe with another input: a resumed run prepares the call again.
const evidenceExeAgain = `const r = await agent.call('build node n1 once more', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`

// dieAt is die for a process that dies right after it stored the row stop matches, with the record directories of EvidenceCwd and
// of every cwd in frozen made read-only then.
func (k *evidenceKit) dieAt(t *testing.T, stop func(Event) bool, body string, frozen ...string) {
	t.Helper()
	cwds := append([]string{k.evCwd}, frozen...)
	sw := &deadSwitch{}
	mem := NewMemoryJournal()
	k.h.cfg.Journal = &dyingJournal{Journal: mem, sw: sw, stop: func(e Event) bool {
		if !stop(e) {
			return false
		}
		for _, cwd := range cwds {
			freezeRecords(t, cwd, true)
		}
		return true
	}}
	k.jr = mem
	k.dieRun(t, sw, body, cwds)
	k.h.cfg.Resume = &ResumeOptions{}
}

// claimAtHost makes every executor subagent the owner spawns from now on claim its receipt at host once its record is there, and
// finish its turn (a subagent of another role answers at once); the count of accepted claims is returned.
func (k *evidenceKit) claimAtHost(t *testing.T, host string) *atomic.Int32 {
	var accepted atomic.Int32
	k.setOrch(func(st Step, _ string, _ int) orchAnswer {
		return orchAnswer{hang: evidence.IsGatedAgentType(st.AgentType)}
	}, func(st Step, agent string) {
		if !evidence.IsGatedAgentType(st.AgentType) {
			return
		}
		go func() {
			if !waitRecord(t, st, host) {
				return
			}
			if v, _ := claim(t, st, host, agent, agent+"-turn1", []byte("red then green\n")); v != evidence.AssignedAccepted {
				t.Errorf("the claim of %s under its host cwd %s: %v", agent, host, v)
			} else {
				accepted.Add(1)
			}
			k.complete(agent, st)
		}()
	})
	return &accepted
}

// noOpenLeft checks that no open record is left under cwd (a claimed one stays: it is the receipt the hook accepted) and that the
// owner's plain child is not refused there.
func noOpenLeft(t *testing.T, what, cwd string) {
	t.Helper()
	for _, path := range evidenceRecords(t, cwd) {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if rec, ok := evidence.RecordedAssignment(cwd, sessionOwner, id); ok && rec.Status == evidence.AssignmentOpen {
			t.Errorf("%s: the open record %s is left under %s", what, id, cwd)
		}
	}
	if v := plainChildVerdict(cwd); v != evidence.NoContract {
		t.Errorf("%s: a child of the owner that shows no packet is judged %v under %s, want NoContract", what, v, cwd)
	}
}

// hostApart gives the kit an owner thread and a host cwd that are both other directories than EvidenceCwd and each other.
func hostApart(t *testing.T, k *evidenceKit) (owner, host string) {
	owner, host = t.TempDir(), t.TempDir()
	k.setOwnerCwd(owner)
	k.setHostCwd(host)
	return owner, host
}

// evidenceLifetimeRows are the scenes of the table.
func evidenceLifetimeRows() []evidenceLifetimeRow {
	isRow := func(name string) func(Event) bool {
		return func(e Event) bool { return e.Kind == KindCall && e.Event == name }
	}
	// hostCopy is the C9 scene of a host copy written before thread_started: the bind writes the record under a host cwd that is
	// neither EvidenceCwd nor the owner's cwd, and the process dies before the thread_started row that would have named it. The
	// resumed run supersedes the call or prepares it again: the host copy is released all the same, its directory named by the
	// evidence_placed row the bind wrote before the copy.
	hostCopy := func(resumed string) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			owner, host := hostApart(t, k)
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
			k.dieBefore(t, isRow(EvThreadStarted), evidenceExe, owner, host)
			if got := evidenceRecords(t, host); len(got) != 1 {
				t.Fatalf("records the bind left under the host cwd %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
			}
			if placed := rowsNamed(k.rowsOf("exe"), EvEvidencePlaced); len(placed) != 1 || rowStr(placed[0].Data, "evidence_cwd") != host {
				t.Errorf("evidence_placed rows %v, want the one that names the host cwd %s (rows %s)", placed, host, eventsOf(k.rowsOf("exe")))
			}
			body := evidenceExploreOnly
			if resumed == "preparedAgain" {
				body = evidenceExeAgain
			}
			accepted := k.claimAtHost(t, host)
			res := k.h.run(body)
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			if resumed == "preparedAgain" && accepted.Load() != 1 {
				t.Errorf("%d receipts of the new generation accepted under the host cwd, want 1", accepted.Load())
			}
			return []string{owner, host}
		}
	}
	// orphan is OrphanBeforeStepRow: the record is written and the process dies before the step_published row.
	orphan := func(body string) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			k.dieBefore(t, isRow(EvStepPublished), evidenceExe)
			if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
				t.Fatalf("records the dead process left %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
			}
			k.claimAtHost(t, k.evCwd)
			if got := value[[]any](t, k.h.run(body)); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			return nil
		}
	}
	// otherCwd is ResumedFromAnotherCwd: the step was published and never taken, and the run is resumed from another directory.
	otherCwd := func(body string, goesOn bool) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			first := k.evCwd
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
			k.dieAt(t, isRow(EvStepPublished), evidenceExe)
			second := t.TempDir()
			k.cfg.EvidenceCwd, k.evCwd = second, second
			k.setHostCwd(second)
			accepted := k.claimAtHost(t, second)
			if got := value[[]any](t, k.h.run(body)); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			if goesOn && accepted.Load() != 1 {
				t.Errorf("%d receipts accepted under the host cwd %s, want 1", accepted.Load(), second)
			}
			return []string{first}
		}
	}
	// placedNoFile: the bind wrote the evidence_placed row of its host cwd and the process died before the file was there (the
	// host cwd cannot hold it). The resumed run supersedes the call: nothing is left anywhere.
	placedNoFile := func(t *testing.T, k *evidenceKit) []string {
		owner, host := hostApart(t, k)
		link := filepath.Join(host, ".crw")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		k.dieAt(t, isRow(EvEvidencePlaced), evidenceExe, owner)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		k.setOrch(nil, nil)
		if got := value[[]any](t, k.h.run(evidenceExploreOnly)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		return []string{owner, host}
	}
	// placed: the process dies right after the evidence_placed row of the host cwd; the bind's copy may be written after it. The
	// resumed run supersedes the call: that copy goes, its directory named by the row.
	placed := func(t *testing.T, k *evidenceKit) []string {
		owner, host := hostApart(t, k)
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		k.dieAt(t, isRow(EvEvidencePlaced), evidenceExe, owner)
		k.setOrch(nil, nil)
		if got := value[[]any](t, k.h.run(evidenceExploreOnly)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		return []string{owner, host}
	}
	// beforePlaced: the process dies before the evidence_placed row of the host cwd is stored, so no copy is written there.
	beforePlaced := func(t *testing.T, k *evidenceKit) []string {
		owner, host := hostApart(t, k)
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		k.dieBefore(t, isRow(EvEvidencePlaced), evidenceExe, owner)
		if got := evidenceRecords(t, host); len(got) != 0 {
			t.Errorf("a copy under the host cwd before its evidence_placed row was stored: %v", got)
		}
		k.setOrch(nil, nil)
		if got := value[[]any](t, k.h.run(evidenceExploreOnly)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		return []string{owner, host}
	}
	// bound: the process dies after the bind (thread_started) with the host cwd apart; the resumed run adopts the subagent, whose
	// receipt is accepted at its host cwd and recorded on the completed row.
	bound := func(t *testing.T, k *evidenceKit) []string {
		owner, host := hostApart(t, k)
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		k.dieAt(t, isRow(EvThreadStarted), evidenceExe, owner, host)
		st := k.orch.spawned()[0]
		if v, _ := claim(t, st, host, "agent-1", "agent-1-turn1", []byte("red then green\n")); v != evidence.AssignedAccepted {
			t.Fatalf("the adopted subagent's claim under its host cwd: %v", v)
		}
		k.complete("agent-1", st)
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		rows := k.rowsOf("exe")
		if r := receiptOf(t, rows[len(rows)-1]); r["state"] != "accepted" {
			t.Errorf("evidence_receipt %v (rows %s)", r, eventsOf(rows))
		}
		return []string{owner, host}
	}
	// heldDelivered is HeldDeliveredCrashBeforeEnd: the held row of a delivered turn is written and the process dies before it ended
	// the call; the resumed held call goes on with a new attempt whose receipt is accepted.
	heldDelivered := func(t *testing.T, k *evidenceKit) []string {
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		ctl := &Control{}
		k.h.cfg.Control = ctl
		paused := make(chan struct{})
		k.dieAt(t, func(e Event) bool {
			if e.Kind == KindCall && e.Event == EvTurnStarted {
				select {
				case <-paused:
				default:
					close(paused)
					go ctl.Pause("operator", "test")
				}
			}
			return e.Kind == KindCall && e.Event == EvHeld
		}, evidenceExe)
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records the dead process left %v", got)
		}
		k.h.cfg.Control = &Control{}
		accepted := k.claimAtHost(t, k.evCwd)
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		if accepted.Load() != 1 {
			t.Errorf("%d receipts of the new attempt accepted, want 1", accepted.Load())
		}
		return nil
	}
	// terminal is TerminalReplayedAfterDeath: a null_model or refused row is written and the process dies before it ended the call.
	terminal := func(fail, row, status string) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{fail: fail} }, nil)
			k.dieAt(t, isRow(row), evidenceExe)
			if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
				t.Fatalf("records the dead process left %v", got)
			}
			k.setOrch(nil, nil)
			if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != status {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			return nil
		}
	}
	return []evidenceLifetimeRow{
		{name: "PreparedOnly", entry: "prepare", death: "after prepared",
			skip: "no record exists before the step: TestSessionRunDeathAtEachRow/*/prepared resumes it, and every later row of this table starts from it"},
		{name: "OrphanBeforeStepRow/superseded", entry: "supersede", death: "before step_published", body: orphan(evidenceExploreOnly)},
		{name: "OrphanBeforeStepRow/preparedAgain", entry: "re-prepare", death: "before step_published", body: orphan(evidenceExeAgain)},
		{name: "PublishedSuperseded", entry: "publish", death: "after step_published",
			skip: "TestSessionEvidenceAttemptLifetime/OpenSupersededByResume and HeldSupersededInSuffix run it with the host cwd at EvidenceCwd; the rows below run it from another cwd"},
		{name: "ResumedFromAnotherCwd/superseded", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExploreOnly, false)},
		{name: "ResumedFromAnotherCwd/preparedAgain", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExeAgain, true)},
		{name: "ResumedFromAnotherCwd/sameStep", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExe, true)},
		{name: "BindBeforePlaced", entry: "bind", death: "before evidence_placed", body: beforePlaced},
		{name: "BindAfterPlaced", entry: "bind", death: "after evidence_placed", body: placed},
		{name: "BindPlacedNoFile", entry: "bind", death: "after evidence_placed, no file", body: placedNoFile},
		{name: "HostCopyBeforeThreadStarted/superseded", entry: "supersede", death: "before thread_started", body: hostCopy("superseded")},
		{name: "HostCopyBeforeThreadStarted/preparedAgain", entry: "re-prepare", death: "before thread_started", body: hostCopy("preparedAgain")},
		{name: "BoundAfterThreadStarted", entry: "bind", death: "after thread_started", body: bound},
		{name: "TurnStarted", entry: "bind", death: "before and after turn_started",
			skip: "TestSessionEvidence/RecoveredStepReusesAssignment/adopted open and adopted claimed, and TestSessionRunDeathAtEachRow/*/turn_started"},
		{name: "HeldDeliveredCrashBeforeEnd", entry: "held resume", death: "after held", body: heldDelivered},
		{name: "TerminalReplayedAfterDeath/null_model", entry: "prepare", death: "after null_model", body: terminal("contextWindowExceeded", EvNullModel, "null_model")},
		{name: "TerminalReplayedAfterDeath/refused", entry: "prepare", death: "after refused", body: terminal("badRequest", EvRefused, "refused")},
	}
}

func TestSessionEvidenceLifetimeTable(t *testing.T) {
	rows := evidenceLifetimeRows()
	t.Run("Meta", func(t *testing.T) {
		names := map[string]bool{}
		for _, r := range rows {
			if r.body == nil && strings.TrimSpace(r.skip) == "" {
				t.Errorf("the row %s has neither a body nor a skip reason", r.name)
			}
			if r.entry == "" || r.death == "" {
				t.Errorf("the row %s names no entry path or death point", r.name)
			}
			names[r.name] = true
		}
		// the C9 scenes of the 1209 field trial
		for _, name := range []string{"HeldDeliveredCrashBeforeEnd", "TerminalReplayedAfterDeath/null_model", "TerminalReplayedAfterDeath/refused",
			"OrphanBeforeStepRow/superseded", "OrphanBeforeStepRow/preparedAgain", "ResumedFromAnotherCwd/superseded",
			"ResumedFromAnotherCwd/preparedAgain", "ResumedFromAnotherCwd/sameStep", "HostCopyBeforeThreadStarted/superseded",
			"HostCopyBeforeThreadStarted/preparedAgain"} {
			if !names[name] {
				t.Errorf("the C9 row %s is missing", name)
			}
		}
	})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.body == nil {
				if strings.TrimSpace(r.skip) == "" {
					t.Fatalf("the row has neither a body nor a skip reason")
				}
				t.Skip(r.skip)
			}
			k := newEvidenceKit(t, true, ".crw/\n")
			others := r.body(t, k)
			noOpenLeft(t, "EvidenceCwd", k.evCwd)
			for _, cwd := range others {
				noOpenLeft(t, cwd, cwd)
			}
		})
	}
}
