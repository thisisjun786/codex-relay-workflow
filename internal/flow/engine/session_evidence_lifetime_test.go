package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
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

// beforeRowJournal kills the process right before it would store the first row stop matches: that row is never stored.
type beforeRowJournal struct {
	Journal
	sw     *deadSwitch
	before func(Event) bool
	onDie  func()
}

func (j *beforeRowJournal) Append(e Event) Event {
	if j.sw.isDead() {
		return e
	}
	if j.before(e) {
		j.onDie()
		j.sw.kill()
		return e
	}
	return j.Journal.Append(e)
}

func (j *beforeRowJournal) Err() error { return nil }

// dieBefore is die for a process that dies before it stores the row before matches (the record directories of EvidenceCwd and of
// every cwd in frozen made read-only then, so the dying process removes nothing anywhere).
func (k *evidenceKit) dieBefore(t *testing.T, before func(Event) bool, body string, frozen ...string) {
	t.Helper()
	cwds := append([]string{k.evCwd}, frozen...)
	sw := &deadSwitch{}
	mem := NewMemoryJournal()
	k.h.cfg.Journal = &beforeRowJournal{Journal: mem, sw: sw, before: before, onDie: func() {
		for _, cwd := range cwds {
			freezeRecords(t, cwd, true)
		}
	}}
	k.jr = mem
	k.dieRun(t, sw, body, cwds)
	k.h.cfg.Resume = &ResumeOptions{}
}

// resumeDyingAt resumes the run with body in a process that dies right after it stored the row stop matches (EvidenceCwd's record
// directories frozen then).
func (k *evidenceKit) resumeDyingAt(t *testing.T, stop func(Event) bool, body string) {
	t.Helper()
	sw := &deadSwitch{}
	prior := k.jr
	k.h.cfg.Journal = &dyingJournal{Journal: prior, sw: sw, stop: func(e Event) bool {
		if stop(e) {
			freezeRecords(t, k.evCwd, true)
			return true
		}
		return false
	}}
	k.dieRun(t, sw, body, []string{k.evCwd})
}

// dieRun runs body in a process whose journal, bus and host die with sw, and readies the kit for the process that resumes the run
// over the same journal.
func (k *evidenceKit) dieRun(t *testing.T, sw *deadSwitch, body string, frozen []string) {
	t.Helper()
	k.cfg.Bus = &dyingBus{FileStepBus: k.bus, sw: sw}
	k.cfg.Host = &dyingHost{fakeSessionHost: k.host, sw: sw}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx1, cancel1 := context.WithCancel(ctx)
	sw.mu.Lock()
	sw.cancel = cancel1
	sw.mu.Unlock()
	if _, err := Run(ctx1, k.h.load(body), k.h.cfg); err != nil {
		t.Fatal(err)
	}
	if !sw.isDead() {
		t.Fatalf("the process never died")
	}
	for _, cwd := range frozen {
		freezeRecords(t, cwd, false)
	}
	k.h.cfg.Journal = k.jr
	k.cfg.Bus, k.cfg.Host = k.bus, k.host
	k.h.cfg.SpentBefore = RunSpent(k.jr.Events(), k.h.cfg.RunID).Total
}

// setOwnerCwd makes the fake host know the owner thread with cwd, or no owner thread when cwd is "".
func (k *evidenceKit) setOwnerCwd(cwd string) {
	k.host.mu.Lock()
	defer k.host.mu.Unlock()
	if cwd == "" {
		delete(k.host.agents, sessionOwner)
		return
	}
	k.host.agents[sessionOwner] = &fakeAgent{SessionAgent: SessionAgent{ThreadID: sessionOwner, Cwd: cwd, Status: "active"}}
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

	// TerminalReplayedAfterDeath: the same for the other terminal rows a resumed run replays, a null_model (the context window was
	// exceeded) and a refused call (a bad request): each releases the open record its attempt left.
	t.Run("TerminalReplayedAfterDeath", func(t *testing.T) {
		for _, tc := range []struct{ name, fail, row, status string }{
			{"null_model", "contextWindowExceeded", EvNullModel, "null_model"},
			{"refused", "badRequest", EvRefused, "refused"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				k := newEvidenceKit(t, true, ".crw/\n")
				k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{fail: tc.fail} }, nil)
				k.die(t, deathAt(tc.row), evidenceExe)
				if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
					t.Fatalf("records the dead process left %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
				}
				k.setOrch(nil, nil)
				res := k.h.run(evidenceExe)
				if got := value[[]any](t, res); got[0] != tc.status {
					t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
				}
				if n := len(rowsNamed(k.rowsOf("exe"), EvPrepared)); n != 1 {
					t.Fatalf("%d prepared rows: the %s call was not replayed (rows %s)", n, tc.status, eventsOf(k.rowsOf("exe")))
				}
				noOpenRecord(t, "after the "+tc.status+" call was replayed", k.evCwd)
			})
		}
	})

	// OrphanBeforeStepRow: a process that wrote a step's record and died before the step_published row that names it leaves a record
	// no row names. A resumed run that supersedes the call (its position is an explorer now) or prepares it again (another input)
	// releases it all the same: the id is derived from the owner, the run, the call and the step's marker.
	t.Run("OrphanBeforeStepRow", func(t *testing.T) {
		for _, tc := range []struct{ name, body string }{
			{"superseded", evidenceExploreOnly},
			{"prepared again", `const r = await agent.call('build node n1 once more', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				k := newEvidenceKit(t, true, ".crw/\n")
				k.dieBefore(t, func(e Event) bool { return e.Kind == KindCall && e.Event == EvStepPublished }, evidenceExe)
				if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
					t.Fatalf("records the dead process left %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
				}
				k.setOrch(nil, nil)
				res := k.h.run(tc.body)
				if got := value[[]any](t, res); got[0] != "completed" {
					t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
				}
				noOpenRecord(t, "after the call was "+tc.name, k.evCwd)
			})
		}
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

	// DoubleFault: process 1 writes a step's record and dies before its step_published row (an orphan of generation 1); process 2
	// resumes with another input and dies right after generation 2's prepared row, before it superseded generation 1; process 3
	// resumes with a third input. The orphan of generation 1 is released all the same: every generation's next step is derived.
	t.Run("DoubleFault", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		isStep := func(e Event) bool { return e.Kind == KindCall && e.Event == EvStepPublished }
		k.dieBefore(t, isStep, evidenceExe)
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("the orphan of generation 1: %v", got)
		}
		k.resumeDyingAt(t, func(e Event) bool { return e.Kind == KindCall && e.Event == EvPrepared }, `const r = await agent.call('build node n1 once more', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`)
		if got := eventsOf(k.rowsOf("exe")); got != "prepared,prepared" {
			t.Fatalf("rows after the two deaths %s", got)
		}
		k.setOrch(nil, nil)
		res := k.h.run(`const r = await agent.call('build node n1 a third time', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		noOpenRecord(t, "after the third generation", k.evCwd)
	})

	// OwnerCopyOwnerGone: the owner thread runs in another cwd than EvidenceCwd, so a step's record is written under both before its
	// step_published row, and the process dies before that row. The resumed process's host no longer knows the owner thread (its read
	// fails), and the resume supersedes the call: both copies are released, the owner's cwd being named by the prepared row.
	t.Run("OwnerCopyOwnerGone", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		owner := t.TempDir()
		k.setHostCwd(owner)
		k.setOwnerCwd(owner)
		k.dieBefore(t, func(e Event) bool { return e.Kind == KindCall && e.Event == EvStepPublished }, evidenceExe, owner)
		if got := len(evidenceRecords(t, owner)) + len(evidenceRecords(t, k.evCwd)); got != 2 {
			t.Fatalf("want the step's record under EvidenceCwd and the owner's cwd, got %d", got)
		}
		k.setOwnerCwd("")
		k.setOrch(nil, nil)
		if got := value[[]any](t, k.h.run(evidenceExploreOnly)); got[0] != "completed" {
			t.Fatalf("resumed %v", got)
		}
		noOpenRecord(t, "EvidenceCwd", k.evCwd)
		noOpenRecord(t, "the owner's cwd", owner)
	})

	// ResumedFromAnotherCwd: the run is resumed from another directory (a new EvidenceCwd) after its executor step was published and
	// never taken. The record stays where it was written: a resumed run that supersedes the call or prepares it again releases it
	// there, and one that goes on with the same step reuses it there and writes none under the new directory.
	t.Run("ResumedFromAnotherCwd", func(t *testing.T) {
		for _, tc := range []struct{ name, body, want string }{
			{"superseded", evidenceExploreOnly, "completed"},
			{"prepared again", `const r = await agent.call('build node n1 once more', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`, "completed"},
			{"same step", evidenceExe, "completed"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				k := newEvidenceKit(t, true, ".crw/\n")
				first := k.evCwd
				k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
				k.die(t, deathAt(EvStepPublished), evidenceExe)
				if got := evidenceRecords(t, first); len(got) != 1 {
					t.Fatalf("records of the published step %v", got)
				}
				second := t.TempDir()
				k.cfg.EvidenceCwd, k.evCwd = second, second
				k.setHostCwd(second)
				k.setOrch(nil, nil)
				res := k.h.run(tc.body)
				if got := value[[]any](t, res); got[0] != tc.want {
					t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
				}
				noOpenRecord(t, "the first directory after the resume", first)
				noOpenRecord(t, "the second directory after the resume", second)
			})
		}
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

// The ids the release derives for a step no row names are bounded by the steps a process could have been about to publish: in each
// generation, the turns up to one past its largest k, at its largest attempt and the next one. A call with three generations of
// five attempts each derives at most 3*2*(k+1) ids, every generation's next step among them, whatever the attempts were.
func TestSessionEvidenceDerivedIDsBounded(t *testing.T) {
	e := &Engine{cfg: Config{Session: &SessionConfig{Owner: sessionOwner, EvidenceCwd: t.TempDir()}}, runID: "run-bounded"}
	const call = "c_bounded"
	var rows []Event
	seq := 0
	row := func(ev string, data map[string]any) {
		seq++
		rows = append(rows, Event{Seq: seq, CallID: call, Event: ev, Data: data})
	}
	var keys []string
	for gen := 0; gen < 3; gen++ {
		row(EvPrepared, map[string]any{})
		if gen == 0 {
			keys = append(keys, call)
		} else {
			keys = append(keys, fmt.Sprintf("%s@%d", call, seq))
		}
		for a := 1; a <= 5; a++ {
			for k := 1; k <= 2; k++ {
				row(EvStepPublished, map[string]any{"k": k, "attempt": a, "action": StepSpawn})
			}
			row(EvSessionAttemptLost, map[string]any{"k": 2, "attempt": a})
		}
	}
	ids := e.evidenceDerivedIDs(rows)
	if len(ids) > 3*2*(2+1) {
		t.Fatalf("%d derived ids for three generations with k up to 2: want at most %d", len(ids), 3*2*(2+1))
	}
	for _, key := range keys {
		next := evidence.FlowAssignmentID(sessionOwner, "run-bounded", key, sessionOp(call+"/t1", 6))
		if !slices.Contains(ids, next) {
			t.Fatalf("the next step's id %s of generation %s is not among the derived %v", next, key, ids)
		}
	}
}

// realEvidenceDir writes nothing through a link: with .crw or .crw/evidence a symbolic link it fails with crwdir.ErrLink and the
// link's target stays empty, and a .gitignore that is a link is left as it is (its target is not written).
func TestRealEvidenceDirRefusesLinks(t *testing.T) {
	for _, at := range []string{".crw", filepath.Join(".crw", "evidence")} {
		t.Run(at, func(t *testing.T) {
			p, target := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(p, at)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(p, at)); err != nil {
				t.Fatal(err)
			}
			if err := realEvidenceDir(p); !errors.Is(err, crwdir.ErrLink) {
				t.Fatalf("realEvidenceDir with %s a link: %v, want ErrLink", at, err)
			}
			if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Fatalf("written through the link: %v", entries)
			}
		})
	}
	t.Run(".gitignore", func(t *testing.T) {
		p, target := t.TempDir(), filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(filepath.Join(p, ".crw", "evidence"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(p, ".crw", "evidence", ".gitignore")); err != nil {
			t.Fatal(err)
		}
		if err := realEvidenceDir(p); err != nil {
			t.Fatalf("realEvidenceDir with a .gitignore link: %v", err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("the link's target was written: %v", err)
		}
	})
}
