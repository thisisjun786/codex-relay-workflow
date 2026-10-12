package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The audit and the archive of a milestone close (CRW-1206): the reviewer threads of the acceptances are archived only when the
// audit of their acceptance passed.

// threadHost is the host of the reviewer threads: what it lists for a thread, and the archives it was asked for.
type threadHost struct {
	mu       sync.Mutex
	readings map[string][]appserver.TurnReading
	archived []string
	refuse   map[string]bool
}

func (h *threadHost) ReadTurns(_ context.Context, thread string, turns []string) ([]appserver.TurnReading, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list, ok := h.readings[thread]
	if !ok {
		return nil, errors.New("the host knows no such thread")
	}
	var out []appserver.TurnReading
	for _, id := range turns {
		found := appserver.TurnReading{TurnID: id}
		for _, r := range list {
			if r.TurnID == id {
				found = r
			}
		}
		out = append(out, found)
	}
	return out, nil
}

func (h *threadHost) ArchiveThread(_ context.Context, thread string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refuse[thread] {
		return errors.New("the host refused")
	}
	h.archived = append(h.archived, thread)
	return nil
}

func (h *threadHost) archives() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.archived)
}

const (
	auditHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	auditCrit  = "sha256:criteria"
)

func rawItem(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// reading is the turn of a reviewer thread on the host: the packet that was sent and the message it ended with.
func reading(turn, head, criteria, last string) appserver.TurnReading {
	packet := "ROLE: reviewer.\n\nHEAD: " + head + "\nCRITERIA DIGEST: " + criteria + "\n"
	return appserver.TurnReading{TurnID: turn, Found: true, Readable: true, Status: "completed", Items: []json.RawMessage{
		rawItem(map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": packet}}}),
		rawItem(map[string]any{"type": "agentMessage", "text": last, "phase": "final_answer"}),
	}}
}

func digestOfText(text string) string {
	var v any
	_ = json.Unmarshal([]byte(text), &v)
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// closedMilestone puts the rows of three accepted nodes in a journal: A whose reviewer thread the host shows as recorded, B whose
// thread says something else than the journal recorded, and C whose thread the host cannot show. Each node has an executor whose
// pair was read back.
func closedMilestone(t *testing.T) (*world, *threadHost) {
	t.Helper()
	w := newWorld(t)
	host := &threadHost{readings: map[string][]appserver.TurnReading{}, refuse: map[string]bool{}}
	w.lane = NewLane(Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Decisions: &MemoryDecisions{}, Actor: "parent", Threads: host,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	verdict := `{"verdict":"verified"}`
	add := func(callID, op, event string, data map[string]any) {
		kind := engine.KindCall
		w.j.Append(engine.Event{RunID: "run-1", Kind: kind, CallID: callID, Op: op, Event: event, Data: data})
	}
	for _, n := range []string{"A", "B", "C"} {
		head := auditHeadA[:39] + map[string]string{"A": "a", "B": "b", "C": "c"}[n]
		ex, rv, thread, turn := "c_ex_"+n, "c_rv_"+n, "thr-rv-"+n, "turn-rv-"+n
		add("c_rel_"+n, "crw.release", engine.EvCompleted, map[string]any{"node": n, "milestone": "M", "released": true, "revision": "r0"})
		add(ex, "agent", engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": n})
		add(ex, "agent", engine.EvThreadStarted, map[string]any{"thread_id": "thr-ex-" + n, "model": "claude-fake-haiku", "effort": "xhigh"})
		add(ex, "agent", engine.EvCompleted, map[string]any{"result": "{}", "result_digest": "x"})
		add("c_commit_"+n, "crw.commit", engine.EvCompleted, map[string]any{"node": n, "commit": head, "call": ex})
		add(rv, "agent", engine.EvPrepared, map[string]any{"role": "reviewer", "family": "reviewer", "node": n, "head": head})
		add(rv, "agent", engine.EvThreadStarted, map[string]any{"thread_id": thread, "model": "gpt-fake-sol", "effort": "high"})
		add(rv, "agent", engine.EvTurnStarted, map[string]any{"turn_id": turn, "k": 1})
		add(rv, "agent", engine.EvCompleted, map[string]any{"result": verdict, "result_digest": digestOfText(verdict)})
		add("c_review_"+n, "crw.review", engine.EvCompleted, map[string]any{"node": n, "head": head, "criteria_digest": auditCrit, "state": "verdict", "verdict": "verified", "call": rv})
		add("c_accept_"+n, "crw.accept", engine.EvCompleted, map[string]any{"node": n, "head": head, "acceptance_id": "acc-" + n, "plan": "plan-1"})
		switch n {
		case "A":
			host.readings[thread] = []appserver.TurnReading{reading(turn, head, auditCrit, verdict)}
		case "B":
			host.readings[thread] = []appserver.TurnReading{reading(turn, head, auditCrit, `{"verdict":"needs_changes"}`)}
		}
	}
	w.must("milestone", "plan-1", "M")
	return w, host
}

// A thread is archived after the audit of its acceptance passed and not before: with no audit row nothing is archived; after the
// audit exactly the thread of the acceptance that passed is, the others are kept with the reason (a mismatch is not a pass, and
// neither is an acceptance the host could not show), and a second archive archives nothing again.
func TestArchiveOnlyAfterAuditPass(t *testing.T) {
	w, host := closedMilestone(t)
	var arch ArchiveOutcome
	_ = json.Unmarshal(w.must("archive", "M"), &arch)
	if len(arch.Archived) != 0 || len(host.archives()) != 0 || len(arch.Kept) != 3 {
		t.Fatalf("before the audit: %+v, archives %v", arch, host.archives())
	}

	var aud AuditOutcome
	_ = json.Unmarshal(w.must("audit", "M"), &aud)
	if aud.OK != 1 || aud.Mismatch != 1 || aud.Unobserved != 1 || len(aud.Results) != 3 {
		t.Fatalf("audit %+v", aud)
	}
	states := map[string]string{}
	for _, r := range aud.Results {
		states[r.Node] = r.State
	}
	if states["A"] != audit.OK || states["B"] != audit.Mismatch || states["C"] != audit.Unobserved {
		t.Fatalf("states %v", states)
	}
	if rows := w.j.Events(); len(rows) == 0 || len(audit.Passed(rows)) != 1 || audit.Passed(rows)["acc-A"].Thread != "thr-rv-A" {
		t.Fatalf("passed %+v", audit.Passed(rows))
	}

	arch = ArchiveOutcome{}
	_ = json.Unmarshal(w.must("archive", "M"), &arch)
	if got := host.archives(); !slices.Equal(got, []string{"thr-rv-A"}) {
		t.Fatalf("archives %v, want the thread of the acceptance that passed only", got)
	}
	kept := map[string]string{}
	for _, k := range arch.Kept {
		kept[k.Node] = k.Reason
	}
	if len(arch.Archived) != 1 || arch.Archived[0].Node != "A" || kept["B"] != "no audit pass row" || kept["C"] != "no audit pass row" {
		t.Errorf("archive %+v", arch)
	}

	// a thread that is archived is not archived again
	arch = ArchiveOutcome{}
	_ = json.Unmarshal(w.must("archive", "M"), &arch)
	if got := host.archives(); len(got) != 1 || len(arch.Archived) != 0 {
		t.Errorf("archives %v, outcome %+v", got, arch)
	}
}

// An audit that ran again and found the acceptance unobserved takes its pass away: the newest row about an acceptance decides, so
// a thread is not archived on the word of an audit that the host can no longer confirm.
func TestArchiveFollowsTheNewestAudit(t *testing.T) {
	w, host := closedMilestone(t)
	w.must("audit", "M")
	host.mu.Lock()
	delete(host.readings, "thr-rv-A")
	host.mu.Unlock()
	w.must("audit", "M")
	var arch ArchiveOutcome
	_ = json.Unmarshal(w.must("archive", "M"), &arch)
	if len(host.archives()) != 0 || len(arch.Archived) != 0 {
		t.Errorf("archives %v after an audit that could not observe the thread", host.archives())
	}
}

// A host that refuses the archive, or no host at all, keeps the thread and the close goes on; an audit with no host reads every
// acceptance as unobserved.
func TestArchiveRefusedOrNoHostKeepsTheThread(t *testing.T) {
	w, host := closedMilestone(t)
	host.refuse["thr-rv-A"] = true
	w.must("audit", "M")
	var arch ArchiveOutcome
	if err := json.Unmarshal(w.must("archive", "M"), &arch); err != nil {
		t.Fatal(err)
	}
	if len(arch.Archived) != 0 || len(host.archives()) != 0 {
		t.Errorf("archive %+v", arch)
	}
	found := false
	for _, k := range arch.Kept {
		if k.Node == "A" && k.Reason == "the host refused: the host refused" {
			found = true
		}
	}
	if !found {
		t.Errorf("kept %+v", arch.Kept)
	}

	// no host wired
	w2 := newWorld(t)
	for _, ev := range w.j.Events() {
		w2.j.Append(ev)
	}
	var aud AuditOutcome
	_ = json.Unmarshal(w2.must("audit", "M"), &aud)
	if aud.OK != 0 || aud.Unobserved+aud.Mismatch != 3 {
		t.Errorf("an audit with no host: %+v", aud)
	}
	_ = json.Unmarshal(w2.must("archive", "M"), &arch)
	if len(arch.Archived) != 0 {
		t.Errorf("archived with no host: %+v", arch)
	}
}
