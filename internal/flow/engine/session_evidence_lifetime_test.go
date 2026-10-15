package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// CRW-1482 crosscheck fix round 1: a flow evidence assignment lives and ends with its call attempt. It is closed or released
// whenever that attempt ends, is superseded or is prepared again, so an open record no child will claim never outlives its attempt
// and never refuses a later child of the owner session that shows no packet; and an attempt held because the subagent that took its
// step could not be bound with its record is never bound again: the held call goes on with a new attempt and a new subagent.

// evidenceExploreFirst is an explorer call before the executor call, so the executor is at position 1 of the path.
const evidenceExploreFirst = `const x = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [x.status, r.status, r.reason]`

// evidenceExploreOnly is an explorer call at position 0 whose input differs from every script above it.
const evidenceExploreOnly = `const x = await agent.call('look around again', { agentType: 'explorer', label: 'exp' })
return [x.status]`

// plainChildVerdict is the stop hook's judgement of a child of the owner session whose transcript shows no packet and that cites no
// assignment: an open record of the session refuses it.
func plainChildVerdict(cwd string) evidence.AssignedVerdict {
	return evidence.JudgeAssignedReceipt(cwd, sessionOwner, "agent-plain", "", `{"status":"done"}`, "")
}

// pauseAtStep pauses the run once a step_published row of the call labelled label is in the journal.
func (k *evidenceKit) pauseAtStep(t *testing.T, ctl *Control, label string) {
	go func() {
		for i := 0; i < 5000; i++ {
			for _, e := range k.h.journal.Events() {
				if e.Kind == KindCall && e.Label == label && e.Event == EvStepPublished {
					ctl.Pause("operator", "test")
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// noOpenRecord checks that no record is left under cwd and that the owner's plain child is not refused there.
func noOpenRecord(t *testing.T, what, cwd string) {
	t.Helper()
	if got := evidenceRecords(t, cwd); len(got) != 0 {
		t.Errorf("%s: records left under %s: %v", what, cwd, got)
	}
	if v := plainChildVerdict(cwd); v != evidence.NoContract {
		t.Errorf("%s: a child of the owner that shows no packet is judged %v under %s, want NoContract", what, v, cwd)
	}
}

func TestSessionEvidenceAttemptLifetime(t *testing.T) {
	// HeldUnboundNeverRebound: the subagent that took the step runs in a host cwd that cannot hold the record, so it is closed and the
	// call held evidence_assignment_failed. Once that cwd can hold records, the resumed held call goes on with a new attempt: a new
	// step and a new subagent, never the rejected, closed one the marker search still finds for the old step.
	t.Run("HeldUnboundNeverRebound", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		host := t.TempDir()
		link := filepath.Join(host, ".crw")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		k.setHostCwd(host)
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "held" || got[1] != ReasonEvidenceAssignmentFailed {
			t.Fatalf("result %v", got)
		}
		waitFor(t, "the unbound subagent closed", func() bool { c := k.orch.closedAgents(); return len(c) == 1 && c[0] == "agent-1" })
		first := k.orch.spawned()[0]
		firstID := messageAssignment(first.Message)
		// the host cwd can hold records now
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		k.h.cfg.Resume = &ResumeOptions{}
		res = k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		rows := k.rowsOf("exe")
		bound := rowsNamed(rows, EvThreadStarted)
		if len(bound) != 1 || rowStr(bound[0].Data, "thread_id") == "agent-1" || rowInt(bound[0].Data, "attempt") != 2 {
			t.Errorf("thread_started %v (rows %s): the rejected agent-1 was bound again, or no new attempt was bound", bound, eventsOf(rows))
		}
		spawns := k.orch.spawned()
		if len(spawns) != 2 || spawns[1].Attempt != 2 || messageAssignment(spawns[1].Message) == firstID {
			t.Errorf("spawns %v: want a new subagent for attempt 2 with its own assignment", spawns)
		}
		for _, cwd := range []string{k.evCwd, host} {
			if _, ok := evidence.RecordedAssignment(cwd, sessionOwner, firstID); ok {
				t.Errorf("the held-unbound attempt's record %s is still under %s", firstID, cwd)
			}
		}
	})

	// HeldDeliveredCrashBeforeEnd: a process that wrote the held row of a delivered turn and died before it ended the call leaves its
	// attempt's open record; the resumed held call goes on with a new attempt and releases it.
	t.Run("HeldDeliveredCrashBeforeEnd", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		ctl := &Control{}
		k.h.cfg.Control = ctl
		var once sync.Once
		heldID := ""
		k.die(t, func(e Event) bool {
			if e.Kind == KindCall && e.Event == EvStepPublished {
				heldID = rowStr(e.Data, "evidence_assignment")
			}
			if e.Kind == KindCall && e.Event == EvTurnStarted {
				once.Do(func() { go ctl.Pause("operator", "test") })
			}
			return e.Kind == KindCall && e.Event == EvHeld
		}, evidenceExe)
		if _, ok := evidence.RecordedAssignment(k.evCwd, sessionOwner, heldID); !ok {
			t.Fatalf("the dead process left no open record of the held attempt")
		}
		k.h.cfg.Control = &Control{}
		k.setOrch(nil, nil)
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		noOpenRecord(t, "after the resumed call completed", k.evCwd)
	})

	// CompletedReplayedAfterDeath: a process that wrote the completed row of an attempt whose subagent never claimed its record (the
	// row says missing) and died before it ended the call leaves that record open; the resumed run replays the completed call and
	// releases it, so the owner's later children that show no packet are not refused by it.
	t.Run("CompletedReplayedAfterDeath", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.die(t, deathAt(EvCompleted), evidenceExe)
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records the dead process left %v, want the open record of the completed attempt", got)
		}
		k.setOrch(nil, nil)
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		if n := len(rowsNamed(k.rowsOf("exe"), EvPrepared)); n != 1 {
			t.Fatalf("%d prepared rows: the completed call was not replayed (rows %s)", n, eventsOf(k.rowsOf("exe")))
		}
		noOpenRecord(t, "after the completed call was replayed", k.evCwd)
	})

	// OpenSupersededByResume: a call the previous process left open with its step published is superseded by the resumed call of its
	// position (an explorer now): its open record goes with it.
	t.Run("OpenSupersededByResume", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
		k.die(t, deathAt(EvStepPublished), evidenceExe)
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records of the open call %v", got)
		}
		k.setOrch(nil, nil)
		res := k.h.run(evidenceExploreOnly)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v", got)
		}
		if got := eventsOf(k.rowsOf("exe")); got != "prepared,step_published,superseded" {
			t.Errorf("rows of the open call %s", got)
		}
		noOpenRecord(t, "after the open call was superseded", k.evCwd)
	})

	// HeldSupersededInSuffix: a held call whose step was published and never taken, in the suffix of a resumed run that does not run
	// it again, is superseded: its record goes with it.
	t.Run("HeldSupersededInSuffix", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(st Step, _ string, _ int) orchAnswer {
			if st.Role == "executor" {
				return orchAnswer{skip: true}
			}
			return orchAnswer{}
		}, nil)
		ctl := &Control{}
		k.h.cfg.Control = ctl
		k.pauseAtStep(t, ctl, "exe")
		res := k.h.run(evidenceExploreFirst)
		if got := value[[]any](t, res); got[1] != "held" {
			t.Fatalf("result %v", got)
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records of the held call %v", got)
		}
		k.h.cfg.Control = &Control{}
		k.h.cfg.Resume = &ResumeOptions{}
		k.setOrch(nil, nil)
		res = k.h.run(evidenceExploreOnly)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v", got)
		}
		if rows := k.rowsOf("exe"); rows[len(rows)-1].Event != EvSuperseded {
			t.Errorf("rows of the held call %s", eventsOf(rows))
		}
		noOpenRecord(t, "after the held call was superseded", k.evCwd)
	})

	// HeldPreparedAgain: a held call whose step was published and never taken is prepared again by the resumed run (its position
	// runs live with another input, which is no executor): the held attempt's record is released before the new prepared row.
	t.Run("HeldPreparedAgain", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
		ctl := &Control{}
		k.h.cfg.Control = ctl
		k.pauseAtStep(t, ctl, "exe")
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "held" {
			t.Fatalf("result %v", got)
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records of the held call %v", got)
		}
		k.h.cfg.Control = &Control{}
		k.h.cfg.Resume = &ResumeOptions{}
		k.setOrch(nil, nil)
		res = k.h.run(evidenceExploreOnly)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v", got)
		}
		if n := len(rowsNamed(k.h.journal.Events(), EvPrepared)); n != 2 {
			t.Errorf("%d prepared rows, want the held call's and its new one's", n)
		}
		noOpenRecord(t, "after the held call was prepared again", k.evCwd)
	})

	// ClaimedNotReusedAcrossPrepare: an executor call whose subagent claimed its receipt is held and then prepared again with another
	// input: the new attempt 1 has its own assignment, so its new subagent's receipt is accepted, never refused as another agent's.
	t.Run("ClaimedNotReusedAcrossPrepare", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		ctl := &Control{}
		k.h.cfg.Control = ctl
		claimed := make(chan struct{})
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
			go func() {
				defer close(claimed)
				if v, _ := claim(t, st, k.evCwd, agent, agent+"-turn1", []byte("first generation\n")); v != evidence.AssignedAccepted {
					t.Errorf("claim %v", v)
				}
			}()
		})
		go func() {
			<-claimed
			for i := 0; i < 5000; i++ {
				if len(rowsNamed(k.h.journal.Events(), EvTurnStarted)) > 0 {
					ctl.Pause("operator", "test")
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "held" {
			t.Fatalf("result %v", got)
		}
		firstID := messageAssignment(k.orch.spawned()[0].Message)
		k.h.cfg.Control = &Control{}
		k.h.cfg.Resume = &ResumeOptions{}
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
			go func() {
				if v, _ := claim(t, st, k.evCwd, agent, agent+"-turn1", []byte("second generation\n")); v != evidence.AssignedAccepted {
					t.Errorf("the new subagent's claim: %v", v)
				}
				k.complete(agent, st)
			}()
		})
		res = k.h.run(`const r = await agent.call('build node n1 once more', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		spawns := k.orch.spawned()
		if len(spawns) != 2 || messageAssignment(spawns[1].Message) == firstID {
			t.Errorf("spawns %v: the new attempt reused the first generation's assignment %s", spawns, firstID)
		}
		rows := k.rowsOf("exe")
		if r := receiptOf(t, rows[len(rows)-1]); r["state"] != "accepted" || r["agent_id"] != "agent-2" {
			t.Errorf("evidence_receipt %v, want accepted for agent-2", r)
		}
	})
}
