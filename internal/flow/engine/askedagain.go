package engine

import "github.com/dop251/goja"

// This file is the proof of a release the scheduler held back for an edit overlap (CRW-1467).
//
// A crw.release the relay answered defer:edit_overlap did not release the node, and the lane may ask again for it. Whether it asked
// again is a fact of the journal: an op writes its row after it ran (one completed row, or one refused row op_failed), so a later
// crw.release row on the node's path is an ask the lane made after it had the answer. A release that was asked again after is the
// decision of its moment, and a resume replays it; the last held-back answer, with no ask after it (a kill inside the relay call
// leaves no row), runs again and asks the relay.

// ReleaseDeferEditOverlap is the reason of a release the scheduler held back because its edit regions overlap a node that holds
// them (dagsched.DeferEditOverlap), as the crw.release row records it.
const ReleaseDeferEditOverlap = "defer:edit_overlap"

// supersededSeqs are the newest sequence numbers at which each call's rows were superseded: a row whose Seq is below its call's
// entry stands for nothing (the filter of crw.priorRelease).
func supersededSeqs(events []Event) map[string]int {
	superseded := map[string]int{}
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Event == EvSuperseded && ev.Seq > superseded[ev.CallID] {
			superseded[ev.CallID] = ev.Seq
		}
	}
	return superseded
}

// AskedAgain reports whether the lane asked for the release of row's node again after row: some crw.release row, completed or
// refused, of the same run and path at a later position, written after row (a later Seq), that no resume superseded and that, when it
// names a node, names row's. A dag thunk's path is its own node's, so a refused row that names no node is still an ask of that node
// only. The Seq matters for refused rows: a resume does not supersede them, so an earlier process's refusal at a later position stays
// in the journal after a resume ran row's position again, and it is no ask after row's answer.
func AskedAgain(events []Event, row Event) bool {
	return askedAgain(events, supersededSeqs(events), row)
}

func askedAgain(events []Event, superseded map[string]int, row Event) bool {
	node := rowStr(row.Data, "node")
	for _, ev := range events {
		if ev.Kind != KindCall || ev.Op != opRelease || (ev.Event != EvCompleted && ev.Event != EvRefused) {
			continue
		}
		if ev.RunID != row.RunID || ev.Path != row.Path || ev.Position <= row.Position || ev.Seq <= row.Seq || superseded[ev.CallID] > ev.Seq {
			continue
		}
		if n := rowStr(ev.Data, "node"); n != "" && n != node {
			continue
		}
		return true
	}
	return false
}

// deferredAsks is crw.deferredAsks(node): the number of standing completed crw.release rows of the node that the scheduler held back
// for an edit overlap (released false, reason defer:edit_overlap) and that the lane asked again after (AskedAgain), or 0 for a run
// that does not resume. These are the rows a resume replays (releaseOp.Proven proves exactly them): the lane reads the count to tell
// a replayed held-back answer, which it had asked again after, from a live one. It takes no position and writes no row.
func (s *scriptVM) deferredAsks(call goja.FunctionCall) goja.Value {
	node := call.Argument(0).String()
	if s.e.cfg.Resume == nil {
		return s.vm.ToValue(0)
	}
	events := s.e.j.Events()
	superseded := supersededSeqs(events)
	n := 0
	for _, ev := range events {
		if ev.Kind != KindCall || ev.Op != opRelease || ev.Event != EvCompleted || ev.RunID != s.e.runID || superseded[ev.CallID] > ev.Seq {
			continue
		}
		if rowStr(ev.Data, "node") != node || rowBoolOf(ev.Data, "released") || rowStr(ev.Data, "reason") != ReleaseDeferEditOverlap {
			continue
		}
		if askedAgain(events, superseded, ev) {
			n++
		}
	}
	return s.vm.ToValue(n)
}
