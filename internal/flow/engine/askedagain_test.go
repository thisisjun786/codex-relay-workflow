package engine

import (
	"encoding/json"
	"testing"
)

// crw.deferredAsks(node) counts the standing completed crw.release rows of the node that the scheduler held back for an edit
// overlap and that the lane asked again after on its path (CRW-1467): the rows a resume replays. The last held-back answer, with no
// ask after it, is not counted; an ask a resume superseded is no ask; a refused ask (op_failed, which names no node) is one. A run
// that does not resume counts nothing.
func TestDeferredAsksCountsTheAnswersAskedAgain(t *testing.T) {
	const body = `return { b: crw.deferredAsks('B'), c: crw.deferredAsks('C') }`
	type counts struct{ B, C int }
	release := func(position int, data map[string]any) Event {
		return Event{RunID: "run-test", Kind: KindCall, CallID: "c_B_" + string(rune('0'+position)), Path: "root/9:B", Position: position, Op: opRelease, Label: opRelease, Event: EvCompleted, Data: data}
	}
	deferred := func(node string) map[string]any {
		return map[string]any{"node": node, "plan": "p", "released": false, "kind": "executor", "reason": ReleaseDeferEditOverlap, "result": "{}"}
	}
	planRead := func(position int) Event {
		return Event{RunID: "run-test", Kind: KindCall, CallID: "c_B_" + string(rune('0'+position)), Path: "root/9:B", Position: position, Op: "crw.planRead", Label: "crw.planRead", Event: EvCompleted, Data: map[string]any{"result": "{}"}}
	}
	// three defers of B, a plan read between each two: the first two were asked again after, the third was not
	rows := func(h *harness, fifth func(Event) Event) {
		h.journal.Append(release(1, deferred("B")))
		h.journal.Append(planRead(2))
		h.journal.Append(release(3, deferred("B")))
		h.journal.Append(planRead(4))
		h.journal.Append(fifth(release(5, deferred("B"))))
		// C's own lane: one defer asked again by a release on C's path, and a held-back answer of another reason that was asked again
		c := func(position int, data map[string]any) Event {
			ev := release(position, data)
			ev.Path, ev.CallID = "root/9:C", "c_C_"+string(rune('0'+position))
			return ev
		}
		h.journal.Append(c(1, deferred("C")))
		h.journal.Append(c(2, map[string]any{"node": "C", "released": false, "kind": "executor", "reason": "defer:no_capacity", "result": "{}"}))
		h.journal.Append(c(3, map[string]any{"node": "C", "released": true, "kind": "executor", "result": "{}"}))
	}
	same := func(ev Event) Event { return ev }
	for _, tc := range []struct {
		name   string
		fifth  func(Event) Event
		after  func(h *harness)
		resume bool
		want   counts
	}{
		{name: "two asked again, the last not", fifth: same, resume: true, want: counts{2, 1}},
		{name: "the second ask superseded", fifth: same, after: func(h *harness) {
			h.journal.Append(Event{RunID: "run-test", Kind: KindCall, CallID: "c_B_5", Path: "root/9:B", Position: 5, Op: opRelease, Event: EvSuperseded})
		}, resume: true, want: counts{1, 1}},
		{name: "the second follow-up refused", fifth: func(ev Event) Event {
			ev.Event, ev.Data = EvRefused, map[string]any{"reason": "op_failed", "detail": "relay down"}
			return ev
		}, resume: true, want: counts{2, 1}},
		{name: "no resume", fifth: same, resume: false, want: counts{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.run(`return 0`)
			rows(h, tc.fifth)
			if tc.after != nil {
				tc.after(h)
			}
			before := len(h.journal.Events())
			if tc.resume {
				h.cfg.Resume = &ResumeOptions{}
			}
			res := h.run(body)
			var got struct{ B, C int }
			if res.Status != RunCompleted {
				t.Fatalf("run %s: %s", res.Status, res.Error)
			}
			if err := json.Unmarshal(res.Value, &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("crw.deferredAsks answered B %d, C %d, want %+v", got.B, got.C, tc.want)
			}
			// it takes no position and writes no row
			for _, ev := range h.journal.Events()[before:] {
				if ev.Kind == KindCall {
					t.Errorf("a call row was written: %+v", ev)
				}
			}
		})
	}

	// AskedAgain, which the count and releaseOp.Proven share, is defined on the rows that are written: a later completed or refused
	// crw.release of the same run and path that stands and, when it names a node, names the row's
	t.Run("AskedAgain", func(t *testing.T) {
		row := Event{RunID: "r", Kind: KindCall, CallID: "c1", Path: "root/9:B", Position: 1, Op: opRelease, Event: EvCompleted, Seq: 1, Data: map[string]any{"node": "B"}}
		later := func(mut func(*Event)) Event {
			ev := Event{RunID: "r", Kind: KindCall, CallID: "c3", Path: "root/9:B", Position: 3, Op: opRelease, Event: EvCompleted, Seq: 4, Data: map[string]any{"node": "B"}}
			if mut != nil {
				mut(&ev)
			}
			return ev
		}
		for _, tc := range []struct {
			name string
			ev   []Event
			want bool
		}{
			{"completed", []Event{row, later(nil)}, true},
			{"refused without node", []Event{row, later(func(ev *Event) { ev.Event, ev.Data = EvRefused, map[string]any{"reason": "op_failed"} })}, true},
			{"a prepared row", []Event{row, later(func(ev *Event) { ev.Event = EvPrepared })}, false},
			{"another op", []Event{row, later(func(ev *Event) { ev.Op = "crw.planRead" })}, false},
			{"another node", []Event{row, later(func(ev *Event) { ev.Data["node"] = "C" })}, false},
			{"another path", []Event{row, later(func(ev *Event) { ev.Path = "root/9:C" })}, false},
			{"the same position", []Event{row, later(func(ev *Event) { ev.Position = 1 })}, false},
			// an earlier process's refusal at a later position, which the resume that ran row's position again did not supersede
			{"refused before the answer", []Event{later(func(ev *Event) { ev.Event, ev.Data, ev.Seq = EvRefused, map[string]any{"reason": "op_failed"}, 0 }), row}, false},
			{"superseded", []Event{row, later(nil), {Kind: KindCall, CallID: "c3", Event: EvSuperseded, Seq: 5}}, false},
			{"superseded before it was written again", []Event{row, {Kind: KindCall, CallID: "c3", Event: EvSuperseded, Seq: 2}, later(nil)}, true},
		} {
			if got := AskedAgain(tc.ev, row); got != tc.want {
				t.Errorf("%s: AskedAgain %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}
