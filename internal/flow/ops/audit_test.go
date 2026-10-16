package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The audit of a milestone's acceptances and the archive of their reviewer threads (CRW-1206): a thread is archived only when the audit
// of its acceptance passed. The project lane's close audits the acceptances and archives nothing (CRW-1447); crw.archive stays an op of the
// lane that a script may call, and these tests call it directly.

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

// noHost is a host of reviewer threads that fails the test when it is asked anything: an acceptance on a unit check has no
// reviewer thread, so its audit reads none and its close archives none.
type noHost struct{ t *testing.T }

func (h noHost) ReadTurns(context.Context, string, []string) ([]appserver.TurnReading, error) {
	h.t.Errorf("the audit of a unit-check acceptance read a thread from the host")
	return nil, errors.New("no host")
}

func (h noHost) ArchiveThread(context.Context, string) error {
	h.t.Errorf("the close of a unit-check acceptance archived a thread")
	return errors.New("no host")
}

// A node lane accepts a head on a unit check (CRW-1465): crw.accept names the basis it rested on and the call of the check, and the
// audit vouches for the acceptance on that row - a completed unit check of role unit-check, result ok, on the head, under the
// criteria digest of the node's release - in place of a reviewer thread it would read: the state is ok when the executor's pair was
// observed, the first check is unit_check_row (no recorded reviewer call), no host is read, no thread is named, and the close
// archives and keeps nothing. A check off the head or under another criteria digest is a mismatch; a land-head acceptance (no
// executor call) is unobserved, not a mismatch, and archives nothing either; an old acceptance row without a basis still takes the
// crw.review path, and an acceptance with neither a basis nor a review is a mismatch as before.
func TestAuditVouchesForAUnitCheckAcceptance(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	appendRow := func(w *world, op, callID, event string, data map[string]any) {
		b, _ := json.Marshal(data)
		var round map[string]any
		_ = json.Unmarshal(b, &round)
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: op, Event: event, Data: round})
	}
	unitCheckRow := func(w *world, callID string, extra map[string]any) {
		res, _ := json.Marshal(map[string]any{"state": "ok", "detail": "", "verdict": "verified", "findings": []any{}, "engine_family": "grok", "model": "grok-4.7-medium",
			"effort": "medium", "guide_rev": "g1", "prompt_digest": "pd-unit", "fingerprint_before": "t-head", "fingerprint_after": "t-head", "elapsed_ms": 1})
		data := map[string]any{"role": "unit-check", "node": "A", "mode": "uncommitted", "call": "c_ex_A", "state": "ok", "verdict": "verified", "findings": 0, "result": string(res)}
		for k, v := range extra {
			data[k] = v
		}
		appendRow(w, OpUnitCheck, callID, engine.EvCompleted, data)
	}
	// nodeLane is the journal of a node lane that built A, checked the uncommitted tree, committed it, verified the head and accepted it.
	nodeLane := func(t *testing.T) (*world, string) {
		w := newWorld(t)
		w.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
		w.relay.plan.Nodes[0].CriteriaDigest = strings.Repeat("c", 64)
		w.ledgered(true, designOf("A"))
		w.must("release", "plan-1", "A", "M")
		rel, ok := releaseRowOf(rows(w.j.Events()), "A")
		criteria := dataStr(rel, "criteria_digest")
		if !ok || criteria == "" {
			t.Fatalf("the release row names no criteria digest: %+v", rel.Data)
		}
		w.lane.d.Relay = &acceptCapture{stubRelay: w.relay}
		w.lane.d.Threads = noHost{t}
		w.lane.d.Git = func(context.Context, string, []string, ...string) (string, error) {
			return "", errors.New("this test runs no git")
		}
		appendRow(w, "agent", "c_ex_A", engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "A"})
		appendRow(w, "agent", "c_ex_A", engine.EvThreadStarted, map[string]any{"thread_id": "thr-ex-A", "model": "claude-fake-haiku", "effort": "xhigh"})
		appendRow(w, "agent", "c_ex_A", engine.EvCompleted, map[string]any{"result": "{}", "result_digest": "x", "pending_tree": "t-head"})
		unitCheckRow(w, "c_uc", map[string]any{"tree": "t-head", "criteria_digest": criteria, "execution_id": dataStr(rel, "execution_id")})
		appendRow(w, OpCommit, "c_commit", engine.EvCompleted, map[string]any{"node": "A", "commit": head, "parent": base, "tree": "t-head", "call": "c_ex_A", "result": "{}"})
		appendRow(w, OpVerify, "c_verify", engine.EvCompleted, map[string]any{"node": "A", "head": head, "ok": true, "base": base, "digest": strings.Repeat("d", 64), "result": "{}"})
		return w, criteria
	}
	acceptRow := func(t *testing.T, w *world) engine.Event {
		t.Helper()
		row, ok := rows(w.j.Events()).opRows(OpAccept).last(func(engine.Event) bool { return true })
		if !ok {
			t.Fatal("no crw.accept row")
		}
		return row
	}
	auditOf := func(t *testing.T, w *world) audit.Result {
		t.Helper()
		var aud AuditOutcome
		if err := json.Unmarshal(w.must("audit", "M"), &aud); err != nil || len(aud.Results) != 1 {
			t.Fatalf("audit %+v %v", aud, err)
		}
		return aud.Results[0]
	}
	checkOf := func(r audit.Result, name string) audit.Check {
		for _, c := range r.Checks {
			if c.Name == name {
				return c
			}
		}
		return audit.Check{}
	}
	archiveOf := func(t *testing.T, w *world) ArchiveOutcome {
		t.Helper()
		var arch ArchiveOutcome
		if err := json.Unmarshal(w.must("archive", "M"), &arch); err != nil {
			t.Fatal(err)
		}
		return arch
	}

	t.Run("a node-lane acceptance on its unit check", func(t *testing.T) {
		w, criteria := nodeLane(t)
		w.must("accept", "A", head)
		acc := acceptRow(t, w)
		if dataStr(acc, "basis") != "unit_check" || dataStr(acc, "reviewer_call") != "c_uc" {
			t.Fatalf("the accept row names basis %q and reviewer call %q, want unit_check and c_uc: %+v", dataStr(acc, "basis"), dataStr(acc, "reviewer_call"), acc.Data)
		}
		subjects := audit.Subjects(w.j.Events())
		if len(subjects) != 1 || subjects[0].Basis != audit.BasisUnitCheck || subjects[0].ReviewerCall != "c_uc" || subjects[0].CriteriaDigest != criteria || subjects[0].Milestone != "M" {
			t.Fatalf("subjects %+v", subjects)
		}
		r := auditOf(t, w)
		if r.State != audit.OK || r.Reason != "" || r.Thread != "" || r.Turn != "" {
			t.Fatalf("the audit of a unit-check acceptance: %+v", r)
		}
		if len(r.Checks) == 0 || r.Checks[0].Name != audit.CheckUnitCheck || r.Checks[0].Result != audit.OK {
			t.Errorf("checks %+v, want unit_check_row ok first", r.Checks)
		}
		if c := checkOf(r, audit.CheckRecorded); c.Name != "" {
			t.Errorf("a recorded-reviewer check was made: %+v", c)
		}
		if c := checkOf(r, audit.CheckHost); c.Name != "" {
			t.Errorf("a host check was made: %+v", c)
		}
		if c := checkOf(r, audit.CheckExecutor); c.Result != audit.OK {
			t.Errorf("executor pair %+v", c)
		}
		if arch := archiveOf(t, w); len(arch.Archived) != 0 || len(arch.Kept) != 0 {
			t.Errorf("the close archived or kept a thread for a unit-check acceptance: %+v", arch)
		}
	})

	t.Run("a unit check off the head or under another criteria digest", func(t *testing.T) {
		for name, edit := range map[string]func(d map[string]any){
			"another tree":            func(d map[string]any) { d["tree"] = "t-other" },
			"another criteria digest": func(d map[string]any) { d["criteria_digest"] = strings.Repeat("e", 64) },
			"no judgment": func(d map[string]any) {
				d["state"], d["result"] = "quota", strings.Replace(d["result"].(string), `"state":"ok"`, `"state":"quota"`, 1)
			},
			"no role": func(d map[string]any) { delete(d, "role") },
			"a needs_changes verdict": func(d map[string]any) {
				d["result"] = strings.Replace(d["result"].(string), `"verdict":"verified"`, `"verdict":"needs_changes"`, 1)
			},
			"a blocking finding": func(d map[string]any) {
				d["result"] = strings.Replace(d["result"].(string), `"findings":[]`, `"findings":[{"id":"0123456789abcdef","severity":"P1","impact":"core_function","file":"x.go","line":1,"text":"breaks"}]`, 1)
			},
		} {
			t.Run(name, func(t *testing.T) {
				w, _ := nodeLane(t)
				w.must("accept", "A", head)
				events := w.j.Events()
				w2 := newWorld(t)
				w2.lane.d.Threads = noHost{t}
				for _, ev := range events {
					if ev.Op == OpUnitCheck {
						d := map[string]any{}
						for k, v := range ev.Data {
							d[k] = v
						}
						edit(d)
						ev.Data = d
					}
					w2.j.Append(ev)
				}
				r := auditOf(t, w2)
				if r.State != audit.Mismatch || len(r.Checks) == 0 || r.Checks[0].Name != audit.CheckUnitCheck || r.Checks[0].Result != audit.Mismatch {
					t.Fatalf("%+v", r)
				}
				if arch := archiveOf(t, w2); len(arch.Archived) != 0 {
					t.Errorf("archived %+v", arch)
				}
			})
		}
	})

	t.Run("a land-head acceptance has no executor call", func(t *testing.T) {
		w := newWorld(t)
		w.lane.d.Threads = noHost{t}
		appendRow(w, OpRelease, "c_rel", engine.EvCompleted, map[string]any{"node": "A", "milestone": "M", "released": true, "kind": "land", "head": head, "tree": "t-head", "criteria_digest": "cd-A"})
		unitCheckRow(w, "c_uc", map[string]any{"mode": "committed", "call": "", "commit": head, "base": base, "tree": "t-head", "criteria_digest": "cd-A", "receipt": map[string]any{"state": "no_executor"}})
		appendRow(w, OpAccept, "c_acc", engine.EvCompleted, map[string]any{"node": "A", "head": head, "acceptance_id": "acc-A", "plan": "plan-1", "basis": "unit_check", "reviewer_call": "c_uc"})
		r := auditOf(t, w)
		if r.State != audit.Unobserved || r.Checks[0].Name != audit.CheckUnitCheck || r.Checks[0].Result != audit.OK || checkOf(r, audit.CheckExecutor).Result != audit.Unobserved || r.Thread != "" {
			t.Fatalf("a land-head acceptance: %+v", r)
		}
		if arch := archiveOf(t, w); len(arch.Archived) != 0 || len(arch.Kept) != 0 {
			t.Errorf("the close archived or kept a thread for a land-head acceptance: %+v", arch)
		}
	})

	t.Run("an old acceptance row without a basis takes the review path", func(t *testing.T) {
		w, _ := closedMilestone(t)
		for _, s := range audit.Subjects(w.j.Events()) {
			if s.Basis != "" || s.ReviewerCall != "c_rv_"+s.Node {
				t.Errorf("subject %+v", s)
			}
		}
		var aud AuditOutcome
		_ = json.Unmarshal(w.must("audit", "M"), &aud)
		for _, r := range aud.Results {
			if r.Checks[0].Name != audit.CheckRecorded {
				t.Errorf("node %s: checks %+v, want the recorded reviewer check first", r.Node, r.Checks)
			}
		}
		// with neither a basis nor a review the acceptance rests on nothing the audit can vouch for
		w2 := newWorld(t)
		appendRow(w2, OpRelease, "c_rel", engine.EvCompleted, map[string]any{"node": "A", "milestone": "M", "released": true})
		appendRow(w2, OpAccept, "c_acc", engine.EvCompleted, map[string]any{"node": "A", "head": head, "acceptance_id": "acc-A", "plan": "plan-1"})
		if r := auditOf(t, w2); r.State != audit.Mismatch || r.Checks[0].Name != audit.CheckRecorded || r.Checks[0].Result != audit.Mismatch {
			t.Errorf("an acceptance with no basis and no review: %+v", r)
		}
	})
}
