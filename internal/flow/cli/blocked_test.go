package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// itemFailed appends the item_error row a join item that threw leaves (engine/scriptvm.go): the path of the item under the dag join.
func (r *run) itemFailed(key, text string) {
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindFlow, Path: engine.RootPath + "/7:" + key, Event: "item_error", Data: map[string]any{"error": text}})
}

// startedNone is a run whose design was approved and whose two releases both threw: the lanes are null and the script returned
// incomplete, so the engine finished it completed.
func startedNone(t *testing.T, w *world, id string, extra func(r *run)) {
	t.Helper()
	r := w.newRun(id, "thread-owner")
	r.phase("design")
	r.completed("c_arch", "architect", "", "thr-arch", 700)
	r.phase("build")
	r.itemFailed("A", "Error: crw.release: the relay refused the release of A: stale_coordinator_epoch")
	r.itemFailed("B", "Error: crw.release: the relay refused the release of B: stale_coordinator_epoch")
	if extra != nil {
		extra(r)
	}
	r.finish(engine.RunCompleted, "")
	r.close()
}

// TestCRW1320_RunThatStartedNoNodeIsBlockedWithItsReasons: a run that finished completed with no successful crw.release and nothing
// landed, whose join items failed, is shown blocked by show, ps and their JSON, with the reason of every item that failed; the engine's
// status of the run stays completed.
func TestCRW1320_RunThatStartedNoNodeIsBlockedWithItsReasons(t *testing.T) {
	w := newWorld(t)
	startedNone(t, w, "run-b", nil)
	td := w.deps()
	code, out, errOut := td.do("show", "run-b", "--state", w.state)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"run run-b  project-lane  blocked",
		"no node started: no crw.release released a node and nothing landed",
		"item A failed: Error: crw.release: the relay refused the release of A: stale_coordinator_epoch",
		"item B failed: Error: crw.release: the relay refused the release of B: stale_coordinator_epoch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = td.do("show", "run-b", "--state", w.state, "--json")
	var d RunDetail
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("show --json: %d %s", code, out)
	}
	if d.State != StateBlocked || d.Status != engine.RunCompleted || len(d.ItemErrors) != 2 || d.ItemErrors[0].Item != "A" || d.ItemErrors[1].Item != "B" ||
		!strings.Contains(d.ItemErrors[0].Error, "stale_coordinator_epoch") {
		t.Errorf("show --json: state %q status %q item errors %+v", d.State, d.Status, d.ItemErrors)
	}
	_, out, _ = td.do("ps", "--all", "--state", w.state)
	if !strings.Contains(out, "run-b  blocked  project-lane") {
		t.Errorf("ps --all:\n%s", out)
	}
	_, out, _ = td.do("ps", "--all", "--json", "--state", w.state)
	var ps struct {
		Runs []RunInfo `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &ps); err != nil || len(ps.Runs) != 1 || ps.Runs[0].State != StateBlocked || len(ps.Runs[0].ItemErrors) != 2 {
		t.Errorf("ps --all --json: %v %s", err, out)
	}
	// the reading report.RunHook asks at the finish is the same one
	events, _, err := loadRun(w, "run-b")
	if err != nil {
		t.Fatal(err)
	}
	reasons, blocked := Blocked(events)
	if !blocked || len(reasons) != 2 || !strings.HasPrefix(reasons[0], "A: ") {
		t.Errorf("Blocked: %v %q", blocked, reasons)
	}
}

// TestCRW1320_StartedOrWaitingRunKeepsItsState: a run where a node was released, a child was released or a node landed keeps today's
// state though items failed, and so does a run whose nodes the scheduler held back unreleased (capacity, CRW-1201): a resume asks again.
// A run whose join items did not fail is not blocked either.
func TestCRW1320_StartedOrWaitingRunKeepsItsState(t *testing.T) {
	cases := map[string]func(r *run){
		"released": func(r *run) {
			r.op("c_rel_c", ops.OpRelease, map[string]any{"node": "C", "released": true, "kind": "executor", "reason": ""})
		},
		"child": func(r *run) {
			r.op("c_child", ops.OpReleaseChild, map[string]any{"node": "C", "child": "child-C"})
		},
		"landed": func(r *run) {
			r.op("c_int", ops.OpIntegrate, map[string]any{"node": "C", "batch_commit": "abcdef0123456789"})
		},
		"capacity": func(r *run) {
			r.op("c_rel_c", ops.OpRelease, map[string]any{"node": "C", "released": false, "kind": "executor", "reason": "capacity_exhausted"})
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			startedNone(t, w, "run-k", extra)
			events, _, err := loadRun(w, "run-k")
			if err != nil {
				t.Fatal(err)
			}
			info := Summarize(RunRecord{RunID: "run-k"}, events, false, 0)
			if info.State != StateCompleted || len(info.ItemErrors) != 2 {
				t.Errorf("state %q item errors %+v", info.State, info.ItemErrors)
			}
			if _, blocked := Blocked(events); blocked {
				t.Errorf("Blocked says blocked")
			}
		})
	}
	t.Run("no_item_failed", func(t *testing.T) {
		w := newWorld(t)
		r := w.newRun("run-q", "thread-owner")
		r.completed("c_arch", "architect", "", "thr-arch", 700)
		r.finish(engine.RunCompleted, "")
		r.close()
		events, _, err := loadRun(w, "run-q")
		if err != nil {
			t.Fatal(err)
		}
		if info := Summarize(RunRecord{RunID: "run-q"}, events, false, 0); info.State != StateCompleted || len(info.ItemErrors) != 0 {
			t.Errorf("state %q item errors %+v", info.State, info.ItemErrors)
		}
	})
}

// TestCRW1320_ShowGivesNoResumeHintWhileTheRunLockIsHeld: while a process holds the run lock, show names the process and gives no
// `crw flow resume` hint for the calls the run goes on with (resume refuses a locked run); without the lock the hint is unchanged.
func TestCRW1320_ShowGivesNoResumeHintWhileTheRunLockIsHeld(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-h", "thread-owner")
	r.phase("build")
	r.call("c_open", "executor", "n1", 1, []string{engine.EvPrepared, engine.EvThreadRequested}, nil)
	r.close()
	td := w.deps()
	_, out, _ := td.do("show", "run-h", "--state", w.state)
	for _, want := range []string{"run run-h  project-lane  dead", "1 call(s) to go on with: crw flow resume run-h", "no process holds the run: crw flow resume run-h"} {
		if !strings.Contains(out, want) {
			t.Errorf("show without the lock lacks %q:\n%s", want, out)
		}
	}
	lock, err := TakeLock(w.state, "run-h", "test", td.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, out, _ = td.do("show", "run-h", "--state", w.state)
	if !strings.Contains(out, "run run-h  project-lane  running") || !strings.Contains(out, "holds the run lock") {
		t.Errorf("show with the lock:\n%s", out)
	}
	if strings.Contains(out, "crw flow resume") {
		t.Errorf("show with the lock gives a resume hint:\n%s", out)
	}
}

// TestCRW1320_DigestSaysABlockedRunStartedNoNode: the digest of a night batch says why a blocked run stopped, with the items that failed,
// and not that every selected issue was worked on.
func TestCRW1320_DigestSaysABlockedRunStartedNoNode(t *testing.T) {
	info := RunInfo{State: StateBlocked, ItemErrors: []ItemError{{Item: "A", Error: "Error: refused"}, {Item: "B", Error: "Error: refused"}}}
	if got := stoppedWhy(info, DigestInput{}); got != "blocked: no node started (A: Error: refused; B: Error: refused)" {
		t.Errorf("stopped %q", got)
	}
}
