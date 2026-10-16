package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"golang.org/x/sys/unix"
)

// CRW-1496: one record-lifetime invariant for a flow evidence assignment. The engine writes a record under a directory only once a
// durable row of the call's newest generation names that directory (evidencePlace, the evidence_placed row), and every release and
// restore reads the directories from the rows. TestSessionEvidenceLifetimeTable runs the invariant over its entry paths (prepare,
// publish, bind, held resume, supersede, re-prepare, other-cwd resume) and death points (before and after evidence_placed, after
// evidence_placed with no file, before and after step_published, thread_started and turn_started): after the recovery no open
// record is left under EvidenceCwd, the owner's cwd or the host cwd, and on a path that goes on the bound child's receipt is
// accepted at its host cwd. The WorktreeRecreated rows recreate the call's directory at the same path before the resume: every copy of
// the record is placed again rooted at the new tree (ensureEvidence, evidenceRefresh), and WorktreeKept checks that a recovery of a
// tree that did not change rewrites no record. Every row has a body: a scene that cannot be built is not a row.

// evidenceLifetimeRow is one scene of the table: entry path, death point, and a body that sets the scene up, recovers it and returns
// the directories besides EvidenceCwd that must hold no open record.
type evidenceLifetimeRow struct {
	name, entry, death string
	body               func(t *testing.T, k *evidenceKit) []string
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

// inoOf is the inode of the directory at path.
func inoOf(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Errorf("stat %s: %v", path, err)
		return 0
	}
	return st.Ino
}

// copiesAt checks that the record id is under each of cwds and roots at the tree whose inode is ino.
func copiesAt(t *testing.T, id string, ino uint64, cwds ...string) {
	t.Helper()
	for _, cwd := range cwds {
		rec, ok := evidence.RecordedAssignment(cwd, sessionOwner, id)
		switch {
		case !ok:
			t.Errorf("no copy of the record %s under %s", id, cwd)
		case rec.RootIno != ino:
			t.Errorf("the copy of the record %s under %s roots at inode %d, want the tree's inode %d now", id, cwd, rec.RootIno, ino)
		}
	}
}

// recordBytes are the bytes and the modification time of the record id under cwd.
func recordBytes(t *testing.T, cwd, id string) ([]byte, time.Time) {
	t.Helper()
	paths := evidenceRecords(t, cwd)
	for _, path := range paths {
		if strings.TrimSuffix(filepath.Base(path), ".json") != id {
			continue
		}
		b, err := os.ReadFile(path)
		fi, serr := os.Stat(path)
		if err != nil || serr != nil {
			t.Fatalf("the record %s under %s: %v %v", id, cwd, err, serr)
		}
		return b, fi.ModTime()
	}
	t.Fatalf("no record %s under %s (%v)", id, cwd, paths)
	return nil, time.Time{}
}

// turnsSignal is the host of a resumed process that tells, once, when the process first reads the turns of agent: a call that goes
// on with its bound subagent reads them only once its recovery (sessionRecover) is done.
type turnsSignal struct {
	*fakeSessionHost
	agent string
	once  sync.Once
	read  chan struct{}
}

func (h *turnsSignal) Turns(ctx context.Context, id string, limit int, full bool) ([]SessionTurn, error) {
	if id == h.agent {
		h.once.Do(func() { close(h.read) })
	}
	return h.fakeSessionHost.Turns(ctx, id, limit, full)
}

// afterRecovery runs f once the resumed process has read the turns of its bound subagent agent (its recovery is done), or after a
// deadline when it never does.
func (k *evidenceKit) afterRecovery(t *testing.T, agent string, f func()) {
	h := &turnsSignal{fakeSessionHost: k.host, agent: agent, read: make(chan struct{})}
	k.cfg.Host = h
	t.Cleanup(func() { k.cfg.Host = k.host })
	go func() {
		select {
		case <-h.read:
		case <-time.After(10 * time.Second):
			t.Errorf("the resumed process never read the turns of %s", agent)
		}
		f()
	}()
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
	// preparedOnly: the process dies right after the prepared row, before any record is written (EvidenceCwd cannot take one from
	// then on, so the dying process writes nothing). The resumed call publishes its step, and the child's receipt is accepted.
	preparedOnly := func(t *testing.T, k *evidenceKit) []string {
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
		restore := func() {
			if err := os.Chmod(k.evCwd, 0o755); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(restore)
		k.dieAt(t, func(e Event) bool {
			if !isRow(EvPrepared)(e) {
				return false
			}
			if err := os.Chmod(k.evCwd, 0o555); err != nil {
				t.Error(err)
			}
			return true
		}, evidenceExe)
		restore()
		if got := eventsOf(k.rowsOf("exe")); got != EvPrepared {
			t.Fatalf("the dead process left the rows %s, want the prepared row alone", got)
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
			t.Fatalf("records the dead process left before its step %v", got)
		}
		accepted := k.claimAtHost(t, k.evCwd)
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		if accepted.Load() != 1 {
			t.Errorf("%d receipts accepted, want 1", accepted.Load())
		}
		return nil
	}
	// publishedSuperseded: the step is published with the host cwd at EvidenceCwd and never taken, the process dies after the
	// step_published row, and the resumed run supersedes the call: its record is released.
	publishedSuperseded := func(t *testing.T, k *evidenceKit) []string {
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
		k.dieAt(t, isRow(EvStepPublished), evidenceExe)
		if got := evidenceRecords(t, k.evCwd); len(got) != 1 {
			t.Fatalf("records the dead process left %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
		}
		k.setOrch(nil, nil)
		if got := value[[]any](t, k.h.run(evidenceExploreOnly)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		if got := rowsNamed(k.rowsOf("exe"), EvSuperseded); len(got) != 1 {
			t.Errorf("superseded rows %v (rows %s)", got, eventsOf(k.rowsOf("exe")))
		}
		return nil
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
	// turnStarted: the host cwd is apart, the subagent is bound (thread_started), and the process dies before its turn_started row is
	// stored or right after it. The resumed run adopts the subagent, whose receipt is accepted at its host cwd and recorded on the
	// completed row.
	turnStarted := func(after bool) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			owner, host := hostApart(t, k)
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
			want := 0
			if after {
				want = 1
				k.dieAt(t, isRow(EvTurnStarted), evidenceExe, owner, host)
			} else {
				k.dieBefore(t, isRow(EvTurnStarted), evidenceExe, owner, host)
			}
			rows := k.rowsOf("exe")
			if len(rowsNamed(rows, EvThreadStarted)) != 1 || len(rowsNamed(rows, EvTurnStarted)) != want {
				t.Fatalf("the dead process left the rows %s, want thread_started and %d turn_started", eventsOf(rows), want)
			}
			st := k.orch.spawned()[0]
			if v, _ := claim(t, st, host, "agent-1", "agent-1-turn1", []byte("red then green\n")); v != evidence.AssignedAccepted {
				t.Fatalf("the adopted subagent's claim under its host cwd: %v", v)
			}
			k.complete("agent-1", st)
			if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			rows = k.rowsOf("exe")
			if r := receiptOf(t, rows[len(rows)-1]); r["state"] != "accepted" {
				t.Errorf("evidence_receipt %v (rows %s)", r, eventsOf(rows))
			}
			return []string{owner, host}
		}
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
	// recreated is the C9 scene of a worktree recreated while the call can still go on in place (CRW-1496): the owner's cwd is apart
	// from EvidenceCwd and is the host cwd, the process dies at stage, and the call's directory P is unregistered and established
	// again at the same path before the resume (dropWorktree), so every copy of the record names a tree that is gone. The resumed
	// call places the record again under every directory with the new tree (ensureEvidence for a spawn step published again,
	// evidenceRefresh for a call that goes on with its bound subagent or its repair step): every copy roots at the new inode, and
	// the child's receipt in the new P is accepted at the owner's cwd and recorded on the completed row. With keep the tree is not
	// recreated, and the records are not rewritten at all.
	recreated := func(stage string, keep bool) func(t *testing.T, k *evidenceKit) []string {
		return func(t *testing.T, k *evidenceKit) []string {
			owner := t.TempDir()
			k.setOwnerCwd(owner)
			k.setHostCwd(owner)
			hang := func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }
			switch stage {
			case "spawnPublished":
				k.setOrch(hang, nil)
				k.dieAt(t, isRow(EvStepPublished), evidenceExe, owner)
			case "bound":
				k.setOrch(hang, nil)
				k.dieAt(t, isRow(EvTurnStarted), evidenceExe, owner)
			case "sendInputPublished":
				k.setOrch(func(st Step, _ string, _ int) orchAnswer {
					if st.K == 1 {
						return orchAnswer{text: "this is not JSON"}
					}
					return orchAnswer{hang: true}
				}, nil)
				k.dieAt(t, deathAtK(EvStepPublished, 2), evidenceExe, owner)
			}
			rows := k.rowsOf("exe")
			steps := rowsNamed(rows, EvStepPublished)
			if len(steps) == 0 {
				t.Fatalf("no step was published (rows %s)", eventsOf(rows))
			}
			id, p := rowStr(steps[0].Data, "evidence_assignment"), rowStr(steps[0].Data, "workspace")
			if id == "" || p == "" {
				t.Fatalf("the spawn step names no assignment or workspace: %v", steps[0].Data)
			}
			old := inoOf(t, p)
			copiesAt(t, id, old, k.evCwd, owner)
			if !keep {
				k.placer.dropWorktree(t, p)
			}
			var accepted atomic.Int32
			// check is what the child sees once the resumed call has placed its record again: every copy roots at the tree now, and
			// its receipt is accepted at the owner's cwd
			check := func(spawn Step, agent, turn string) {
				now := inoOf(t, p)
				if !keep && now == old {
					t.Errorf("the recreated worktree %s kept the inode %d", p, old)
				}
				copiesAt(t, id, now, k.evCwd, owner)
				if v, _ := claim(t, spawn, owner, agent, turn, []byte("red then green\n")); v != evidence.AssignedAccepted {
					t.Errorf("the claim of %s under the owner's cwd %s: %v", agent, owner, v)
				} else {
					accepted.Add(1)
				}
			}
			switch stage {
			case "spawnPublished":
				k.setOrch(hang, func(st Step, agent string) {
					go func() {
						check(st, agent, agent+"-turn1")
						k.complete(agent, st)
					}()
				})
			case "bound":
				agent := rowStr(rowsNamed(rows, EvThreadStarted)[0].Data, "thread_id")
				spawn := k.orch.spawned()[0]
				type snap struct {
					b  []byte
					at time.Time
				}
				before := map[string]snap{}
				for _, cwd := range []string{k.evCwd, owner} {
					b, at := recordBytes(t, cwd, id)
					before[cwd] = snap{b, at}
				}
				k.afterRecovery(t, agent, func() {
					if keep {
						for cwd, was := range before {
							if b, at := recordBytes(t, cwd, id); !bytes.Equal(b, was.b) || !at.Equal(was.at) {
								t.Errorf("the record %s under %s was rewritten by the recovery of a kept tree:\n%s\nwant\n%s", id, cwd, b, was.b)
							}
						}
					}
					check(spawn, agent, agent+"-turn1")
					k.complete(agent, spawn)
				})
			case "sendInputPublished":
				spawn := k.orch.spawned()[0]
				k.setOrch(func(st Step, _ string, _ int) orchAnswer {
					if st.Action == StepSendInput {
						check(spawn, st.AgentID, st.AgentID+"-turn2")
						return orchAnswer{}
					}
					return orchAnswer{hang: true}
				}, nil)
			}
			if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			if accepted.Load() != 1 {
				t.Errorf("%d receipts accepted under the owner's cwd, want 1", accepted.Load())
			}
			rows = k.rowsOf("exe")
			if r := receiptOf(t, rows[len(rows)-1]); r["state"] != "accepted" {
				t.Errorf("evidence_receipt %v (rows %s)", r, eventsOf(rows))
			}
			return []string{owner}
		}
	}
	return []evidenceLifetimeRow{
		{name: "PreparedOnly", entry: "prepare", death: "after prepared", body: preparedOnly},
		{name: "OrphanBeforeStepRow/superseded", entry: "supersede", death: "before step_published", body: orphan(evidenceExploreOnly)},
		{name: "OrphanBeforeStepRow/preparedAgain", entry: "re-prepare", death: "before step_published", body: orphan(evidenceExeAgain)},
		{name: "PublishedSuperseded", entry: "publish", death: "after step_published", body: publishedSuperseded},
		{name: "ResumedFromAnotherCwd/superseded", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExploreOnly, false)},
		{name: "ResumedFromAnotherCwd/preparedAgain", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExeAgain, true)},
		{name: "ResumedFromAnotherCwd/sameStep", entry: "other-cwd resume", death: "after step_published", body: otherCwd(evidenceExe, true)},
		{name: "BindBeforePlaced", entry: "bind", death: "before evidence_placed", body: beforePlaced},
		{name: "BindAfterPlaced", entry: "bind", death: "after evidence_placed", body: placed},
		{name: "BindPlacedNoFile", entry: "bind", death: "after evidence_placed, no file", body: placedNoFile},
		{name: "HostCopyBeforeThreadStarted/superseded", entry: "supersede", death: "before thread_started", body: hostCopy("superseded")},
		{name: "HostCopyBeforeThreadStarted/preparedAgain", entry: "re-prepare", death: "before thread_started", body: hostCopy("preparedAgain")},
		{name: "BoundAfterThreadStarted", entry: "bind", death: "after thread_started", body: bound},
		{name: "TurnStarted/before", entry: "bind", death: "before turn_started", body: turnStarted(false)},
		{name: "TurnStarted/after", entry: "bind", death: "after turn_started", body: turnStarted(true)},
		{name: "HeldDeliveredCrashBeforeEnd", entry: "held resume", death: "after held", body: heldDelivered},
		{name: "TerminalReplayedAfterDeath/null_model", entry: "prepare", death: "after null_model", body: terminal("contextWindowExceeded", EvNullModel, "null_model")},
		{name: "TerminalReplayedAfterDeath/refused", entry: "prepare", death: "after refused", body: terminal("badRequest", EvRefused, "refused")},
		{name: "WorktreeRecreated/spawnPublished", entry: "publish", death: "after step_published, worktree recreated", body: recreated("spawnPublished", false)},
		{name: "WorktreeRecreated/bound", entry: "bind", death: "after turn_started, worktree recreated", body: recreated("bound", false)},
		{name: "WorktreeRecreated/sendInputPublished", entry: "publish", death: "after the repair's step_published, worktree recreated", body: recreated("sendInputPublished", false)},
		{name: "WorktreeKept/bound", entry: "bind", death: "after turn_started", body: recreated("bound", true)},
	}
}

func TestSessionEvidenceLifetimeTable(t *testing.T) {
	rows := evidenceLifetimeRows()
	t.Run("Meta", func(t *testing.T) {
		names := map[string]bool{}
		for _, r := range rows {
			if r.body == nil {
				t.Errorf("the row %s has no body", r.name)
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
			"HostCopyBeforeThreadStarted/preparedAgain", "WorktreeRecreated/spawnPublished", "WorktreeRecreated/bound",
			"WorktreeRecreated/sendInputPublished"} {
			if !names[name] {
				t.Errorf("the C9 row %s is missing", name)
			}
		}
	})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.body == nil {
				t.Fatal("the row has no body")
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

// failGitOnce puts a git on PATH that fails the first `git rev-parse --verify -q HEAD^{commit}` (the HEAD a flow record is made with)
// once arm is called, as a git that timed out, and runs the real git otherwise; the returned func reports whether the failure was
// spent.
func failGitOnce(t *testing.T) (arm func(), spent func() bool) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	armed := filepath.Join(dir, "armed")
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = 'rev-parse --verify -q HEAD^{commit}' ] && rm \"" + armed + "\" 2>/dev/null; then\n" +
		"  echo 'error: git timed out' >&2\n  exit 3\nfi\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	arm = func() {
		if err := os.WriteFile(armed, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spent = func() bool {
		_, err := os.Lstat(armed)
		return errors.Is(err, fs.ErrNotExist)
	}
	return arm, spent
}

// TestSessionEvidenceRefreshGitFailure: the resumed call goes on with its bound subagent, and git fails once while the record is
// placed again (evidenceRefresh). A git that fails is no answer: the call holds evidence_assignment_failed, and no record is
// rewritten (none loses its HEAD or is replaced by one made from no answer); the hold may release the attempt's open record as any
// hold of a bound call does (CRW-1496).
func TestSessionEvidenceRefreshGitFailure(t *testing.T) {
	arm, spent := failGitOnce(t)
	k := newEvidenceKit(t, true, ".crw/\n")
	owner := t.TempDir()
	k.setOwnerCwd(owner)
	k.setHostCwd(owner)
	k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
	k.dieAt(t, func(e Event) bool { return e.Kind == KindCall && e.Event == EvTurnStarted }, evidenceExe, owner)
	rows := k.rowsOf("exe")
	id := rowStr(rowsNamed(rows, EvStepPublished)[0].Data, "evidence_assignment")
	agent := rowStr(rowsNamed(rows, EvThreadStarted)[0].Data, "thread_id")
	spawn := k.orch.spawned()[0]
	before := map[string][]byte{}
	for _, cwd := range []string{k.evCwd, owner} {
		before[cwd], _ = recordBytes(t, cwd, id)
	}
	// a call that goes on reads its subagent's turns: that subagent then finishes, so a run that does not hold ends
	h := &turnsSignal{fakeSessionHost: k.host, agent: agent, read: make(chan struct{})}
	k.cfg.Host = h
	done := make(chan struct{})
	go func() {
		select {
		case <-h.read:
			k.complete(agent, spawn)
		case <-done:
		}
	}()
	arm()
	res := k.h.run(evidenceExe)
	close(done)
	k.cfg.Host = k.host
	if !spent() {
		t.Fatal("the resumed process never asked git for the HEAD of the call's directory")
	}
	if got := value[[]any](t, res); got[0] != StatusHeld || got[1] != ReasonEvidenceAssignmentFailed {
		t.Fatalf("resumed %v, want held %s (rows %s)", got, ReasonEvidenceAssignmentFailed, eventsOf(k.rowsOf("exe")))
	}
	// the hold ends the attempt, so its open record may be released; one that is left is the record as it was
	for cwd, was := range before {
		for _, path := range evidenceRecords(t, cwd) {
			if strings.TrimSuffix(filepath.Base(path), ".json") != id {
				continue
			}
			if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, was) {
				t.Errorf("the record %s under %s was rewritten on a git failure (%v):\n%s\nwant\n%s", id, cwd, err, now, was)
			}
		}
	}
	if held := rowsNamed(k.rowsOf("exe"), EvHeld); len(held) != 1 || !strings.Contains(rowStr(held[0].Data, "detail"), "git") ||
		rowStr(held[0].Data, "input_state") != "delivered" {
		t.Errorf("held rows %v, want the one that names git's failure, with the bound turn's input delivered", held)
	}
}

// TestSessionEvidenceRefreshGitFailureRepair: the task turn was delivered and answered with no JSON, the repair step (send_input) is
// published, and the process dies. The resumed call goes on with that repair, and git fails once while its record is placed again
// (evidenceRefresh): the call holds evidence_assignment_failed with its input recorded as delivered (the task turn was), never as
// not_delivered. The next resume then goes on as a held delivered call does: the closed subagent is never sent the repair again, a
// new attempt takes it, and its answer is accepted (CRW-1496).
func TestSessionEvidenceRefreshGitFailureRepair(t *testing.T) {
	arm, spent := failGitOnce(t)
	k := newEvidenceKit(t, true, ".crw/\n")
	k.setOrch(func(st Step, _ string, _ int) orchAnswer {
		if st.K == 1 {
			return orchAnswer{text: "this is not JSON"}
		}
		return orchAnswer{hang: true}
	}, nil)
	k.dieAt(t, deathAtK(EvStepPublished, 2), evidenceExe)
	rows := k.rowsOf("exe")
	if steps := rowsNamed(rows, EvStepPublished); len(steps) != 2 || rowStr(steps[1].Data, "action") != StepSendInput {
		t.Fatalf("the dead process left the rows %s, want the repair step published", eventsOf(rows))
	}
	first := rowStr(rowsNamed(rows, EvThreadStarted)[0].Data, "thread_id")
	arm()
	if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != StatusHeld || got[1] != ReasonEvidenceAssignmentFailed {
		t.Fatalf("the first resume %v, want held %s (rows %s)", got, ReasonEvidenceAssignmentFailed, eventsOf(k.rowsOf("exe")))
	}
	if !spent() {
		t.Fatal("the resumed process never asked git for the HEAD of the call's directory")
	}
	held := rowsNamed(k.rowsOf("exe"), EvHeld)
	if len(held) != 1 || rowStr(held[0].Data, "input_state") != "delivered" || held[0].Data["repair_used"] != true {
		t.Errorf("held rows %v, want one with input_state delivered and repair_used", held)
	}
	// the second resume: every step is answered at once with the step's own answer
	var mu sync.Mutex
	var sentTo []string
	k.setOrch(func(st Step, _ string, _ int) orchAnswer {
		if st.Action == StepSendInput {
			mu.Lock()
			sentTo = append(sentTo, st.AgentID)
			mu.Unlock()
		}
		return orchAnswer{}
	}, nil)
	if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
		t.Fatalf("the second resume %v, want completed (rows %s)", got, eventsOf(k.rowsOf("exe")))
	}
	steps := rowsNamed(k.rowsOf("exe"), EvStepPublished)
	if last := steps[len(steps)-1]; rowInt(last.Data, "attempt") < 2 || last.Data["repair"] != true || rowStr(last.Data, "action") != StepSpawn {
		t.Errorf("the step the second resume published %v, want the repair spawned on a new attempt (rows %s)", last.Data, eventsOf(k.rowsOf("exe")))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, agent := range sentTo {
		if agent == first {
			t.Errorf("the closed subagent %s was sent the repair again (rows %s)", first, eventsOf(k.rowsOf("exe")))
		}
	}
}

// heldOnDir is the directory a held executor call worked in, as the next resume reads it (heldCwd): the allocation path of its
// prepared row.
func heldOnDir(t *testing.T, rows []Event) string {
	t.Helper()
	prep := rowsNamed(rows, EvPrepared)
	if len(prep) == 0 {
		t.Fatalf("no prepared row: %s", eventsOf(rows))
	}
	m, _ := prep[0].Data["allocation"].(map[string]any)
	dir := rowStr(m, "path")
	if dir == "" {
		t.Fatalf("the prepared row %v names no allocation", prep[0].Data)
	}
	return dir
}

// resumedInPlace checks how the resume after a hold went on: in place (one hold_cleared row, the call's one prepared row) or live (no
// hold_cleared row, the call prepared again).
func (k *evidenceKit) resumedInPlace(t *testing.T, inPlace bool) {
	t.Helper()
	var j Journal = k.h.journal
	if k.jr != nil {
		j = k.jr
	}
	rows := k.rowsOf("exe")
	cleared, prepared := len(flowRows(j, EvHoldCleared)), len(rowsNamed(rows, EvPrepared))
	switch {
	case inPlace && (cleared != 1 || prepared != 1):
		t.Errorf("%d hold_cleared rows and %d prepared rows, want the held call to go on in place (rows %s)", cleared, prepared, eventsOf(rows))
	case !inPlace && (cleared != 0 || prepared != 2):
		t.Errorf("%d hold_cleared rows and %d prepared rows, want the held call to run live over a tree the hold did not leave (rows %s)",
			cleared, prepared, eventsOf(rows))
	}
}

// TestSessionEvidenceRefreshGitFailureLeavesTheHeldSnapshot: the call held when its record could not be placed again
// (evidenceRefreshHold) may have had its input delivered, so its held row records P_held, the working snapshot of the call's
// directory, as every other hold of an executor does. A resume whose directory no longer holds that tree runs the call live; one
// whose tree is unchanged goes on in place (CRW-1561).
func TestSessionEvidenceRefreshGitFailureLeavesTheHeldSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tree    string
		inPlace bool
	}{{"TreeChanged", "T1", false}, {"TreeUnchanged", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			arm, spent := failGitOnce(t)
			k := newEvidenceKit(t, true, ".crw/\n")
			owner := t.TempDir()
			k.setOwnerCwd(owner)
			k.setHostCwd(owner)
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, nil)
			k.dieAt(t, func(e Event) bool { return e.Kind == KindCall && e.Event == EvTurnStarted }, evidenceExe, owner)
			dir := heldOnDir(t, k.rowsOf("exe"))
			k.placer.setTree(dir, "TH")
			arm()
			if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != StatusHeld || got[1] != ReasonEvidenceAssignmentFailed {
				t.Fatalf("resumed %v, want held %s (rows %s)", got, ReasonEvidenceAssignmentFailed, eventsOf(k.rowsOf("exe")))
			}
			if !spent() {
				t.Fatal("the resumed process never asked git for the HEAD of the call's directory")
			}
			held := rowsNamed(k.rowsOf("exe"), EvHeld)
			if len(held) != 1 {
				t.Fatalf("held rows %v, want one", held)
			}
			if rowStr(held[0].Data, "p_held") != "TH" || rowStr(held[0].Data, "p_held_error") != "" {
				t.Errorf("held %v, want p_held the tree TH of %s", held[0].Data, dir)
			}
			if tc.tree != "" {
				k.placer.setTree(dir, tc.tree)
			}
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{} }, nil)
			if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
				t.Fatalf("the next resume %v, want completed (rows %s)", got, eventsOf(k.rowsOf("exe")))
			}
			k.resumedInPlace(t, tc.inPlace)
		})
	}
}

// TestSessionEvidenceHoldsLeaveTheHeldSnapshot: the two other holds of a call whose flow evidence assignment could not be recorded
// record P_held as well: the hold before anything was dispatched (evidenceHold, not_delivered: the directory may still be edited
// from outside) and the hold of a subagent that took the step and was closed unbound (evidenceUnbound: it may have edited files).
// The unbound call whose directory changed runs live on the next resume (CRW-1561).
func TestSessionEvidenceHoldsLeaveTheHeldSnapshot(t *testing.T) {
	t.Run("NotDelivered", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		if err := os.Symlink(t.TempDir(), filepath.Join(k.evCwd, ".crw")); err != nil {
			t.Fatal(err)
		}
		dir := k.placer.realPath("/w/n1/a1")
		k.placer.setTree(dir, "TH")
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != StatusHeld || got[1] != ReasonEvidenceAssignmentFailed {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := heldOnDir(t, rows); got != dir {
			t.Fatalf("the call worked in %s, want %s", got, dir)
		}
		held := rowsNamed(rows, EvHeld)
		if len(held) != 1 || rowStr(held[0].Data, "input_state") != "not_delivered" || rowStr(held[0].Data, "p_held") != "TH" {
			t.Errorf("held rows %v, want one not_delivered whose p_held is the tree TH of %s", held, dir)
		}
	})
	t.Run("Unbound", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		host := t.TempDir()
		link := filepath.Join(host, ".crw")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		k.setHostCwd(host)
		dir := k.placer.realPath("/w/n1/a1")
		k.placer.setTree(dir, "TH")
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != StatusHeld || got[1] != ReasonEvidenceAssignmentFailed {
			t.Fatalf("result %v", got)
		}
		waitFor(t, "the unbound subagent closed", func() bool { c := k.orch.closedAgents(); return len(c) == 1 && c[0] == "agent-1" })
		rows := k.rowsOf("exe")
		if got := heldOnDir(t, rows); got != dir {
			t.Fatalf("the call worked in %s, want %s", got, dir)
		}
		held := rowsNamed(rows, EvHeld)
		if len(held) != 1 || rowStr(held[0].Data, "evidence_unbound") != "agent-1" {
			t.Fatalf("held rows %v, want one naming agent-1 unbound", held)
		}
		if rowStr(held[0].Data, "p_held") != "TH" || rowStr(held[0].Data, "p_held_error") != "" {
			t.Errorf("held %v, want p_held the tree TH of %s", held[0].Data, dir)
		}
		// the host cwd can hold records now, and the closed subagent's directory is not the tree the hold left
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		k.placer.setTree(dir, "T1")
		k.h.cfg.Resume = &ResumeOptions{}
		if got := value[[]any](t, k.h.run(evidenceExe)); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		k.resumedInPlace(t, false)
	})
}
