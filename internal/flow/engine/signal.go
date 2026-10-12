package engine

import (
	"sync"

	"github.com/dop251/goja"
)

// This file is what the engine gives the signal path of crw.dag (engine.resume.signals, CRW-1202).
//
// A signal is made of the terminal events of lane calls (their RECEIPTs). A resumed run replays a signal only while every call it
// was made of replays and answers the same result; the script cannot tell a replayed call from one that ran again by what the
// call returned (a result carries nothing of how it came about, and a result that did would differ between a run and its
// resume), so the engine says it: crw.callFacts answers the digest of a call's result, its place in the journal and whether the
// resume replayed it. crw.recordedSignal and crw.priorRelease read what the earlier processes recorded. None of the three takes a
// position of its path or writes a row: the answer is a function of the journal and of how this process came to the result, and
// everything the script decides from it is recorded by an operation of its own.

// The ops whose rows the engine reads for the signal path (internal/flow/ops owns them).
const (
	opSignalBind  = "crw.signalBind"
	opRelease     = "crw.release"
	opDesignLedge = "crw.designLedger"
	// releasePlanChanged is the reason of a release that compared another plan than the one the audited DESIGN stands on
	releasePlanChanged = "plan_changed"
)

// signalFacts are the calls this process replayed from the journal of an earlier one.
type signalFacts struct {
	m        sync.Mutex
	replayed map[string]bool
}

func (f *signalFacts) noteReplayed(callID string) {
	f.m.Lock()
	defer f.m.Unlock()
	if f.replayed == nil {
		f.replayed = map[string]bool{}
	}
	f.replayed[callID] = true
}

func (f *signalFacts) isReplayed(callID string) bool {
	f.m.Lock()
	defer f.m.Unlock()
	return f.replayed[callID]
}

// CallFacts are the facts crw.callFacts answers for the result of a call.
type CallFacts struct {
	CallID string `json:"callId"`
	// Status is the status of the call's result.
	Status string `json:"status"`
	// Digest is the canonical digest of the call's result: of its output when it completed, else of its status and reason. A
	// replayed call answers the digest it answered before.
	Digest string `json:"digest"`
	// Live is whether the result did not come from the journal of an earlier process: false for a call a resume replayed, true for
	// every call of a run that does not resume.
	Live bool `json:"live"`
	// Seq is the sequence number of the call's terminal row: the journal's order, which does not depend on the order the host
	// finished concurrent calls in this process.
	Seq int `json:"seq"`
}

// CanonicalDigest is the digest crw.callFacts answers for the output of a completed call: the digest of its JSON in canonical form.
func CanonicalDigest(raw []byte) string { return canonicalDigest(raw) }

// callFactsOf reads the facts of a call from the journal.
func (e *Engine) callFactsOf(res CallResult) CallFacts {
	digest := ""
	if res.Status == StatusCompleted {
		digest = canonicalDigest(res.Output)
	} else {
		digest = digestJSON(mustJSON(map[string]string{"status": res.Status, "reason": res.Reason}))
	}
	seq := 0
	for _, ev := range e.j.Events() {
		if ev.Kind == KindCall && ev.CallID == res.CallID && ev.Op == "agent" && isTerminal(ev.Event) {
			seq = ev.Seq
		}
	}
	return CallFacts{CallID: res.CallID, Status: res.Status, Digest: digest, Live: !e.facts.isReplayed(res.CallID), Seq: seq}
}

// opFactsOf reads the facts of the completed row of an operation's position: the digest of its result, the place of its row in the
// journal and whether this process replayed it (an operation that ran again at its position is live).
func (e *Engine) opFactsOf(callID string) (CallFacts, bool) {
	var row Event
	found := false
	for _, ev := range e.j.Events() {
		if ev.Kind == KindCall && ev.CallID == callID && ev.Op != "agent" && ev.Event == EvCompleted {
			row, found = ev, true
		}
	}
	if !found {
		return CallFacts{}, false
	}
	return CallFacts{CallID: callID, Status: StatusCompleted, Digest: canonicalDigest([]byte(rowStr(row.Data, "result"))), Live: !e.facts.isReplayed(callID), Seq: row.Seq}, true
}

// callFacts is crw.callFacts(result): the facts the signal path rests on, for the CallResult agent.call returned or its handle, or
// for the result of an operation that names the position it ran at (a plain object with a call_id).
func (s *scriptVM) callFacts(call goja.FunctionCall) goja.Value {
	v := call.Argument(0)
	var res CallResult
	found := false
	if obj, ok := v.(*goja.Object); ok {
		if ref := s.results[obj]; ref != nil {
			res, found = ref.res, true
		} else if cv := obj.Get("call_id"); cv != nil {
			if id, ok := cv.Export().(string); ok && id != "" {
				facts, ok := s.e.opFactsOf(id)
				if !ok {
					s.throwType("crw.callFacts: the operation %s has no completed row", id)
				}
				return s.fromJSON(mustJSON(facts))
			}
		}
	} else if str, ok := v.Export().(string); ok {
		res, found = s.e.byHandle[str]
	}
	if !found {
		s.throwType("crw.callFacts takes the CallResult agent.call returned, its handle, or the result of an operation that names its call_id")
	}
	return s.fromJSON(mustJSON(s.e.callFactsOf(res)))
}

// standingRow is the newest completed row of an op that satisfies keep and that nothing superseded afterwards.
func (e *Engine) standingRow(op string, keep func(map[string]any) bool) (Event, bool) {
	events := e.j.Events()
	superseded := map[string]int{}
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Event == EvSuperseded && ev.Seq > superseded[ev.CallID] {
			superseded[ev.CallID] = ev.Seq
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind != KindCall || ev.Op != op || ev.Event != EvCompleted || superseded[ev.CallID] > ev.Seq {
			continue
		}
		if keep(ev.Data) {
			return ev, true
		}
	}
	return Event{}, false
}

// recordedSignal is crw.recordedSignal(milestone, k): the signal k of the milestone as an earlier process recorded it and as the
// journal still stands on it, or null. A signal stands while its row is not superseded: the first signal a resume does not
// replay, and every signal after it, are the suffix of the resume, and the rows of a suffix stand for nothing. attempt is what the
// row was bound with ("live:<n>" for a signal that an earlier resume ran again, "" otherwise): replaying the row means handing
// crw.signalBind the arguments it was recorded with, that one included.
func (s *scriptVM) recordedSignal(call goja.FunctionCall) goja.Value {
	ms, k := call.Argument(0).String(), int(call.Argument(1).ToInteger())
	if s.e.cfg.Resume == nil {
		return goja.Null()
	}
	row, ok := s.e.standingRow(opSignalBind, func(d map[string]any) bool { return rowStr(d, "milestone") == ms && rowInt(d, "k") == k })
	if !ok {
		return goja.Null()
	}
	return s.fromJSON(mustJSON(map[string]any{"k": k, "prev": rowStr(row.Data, "prev"), "members": row.Data["members"], "attempt": rowStr(row.Data, "attempt"), "call": row.CallID}))
}

// priorRelease is crw.priorRelease(node): the release of the node that an earlier process recorded and the journal still stands
// on, with the revision it was made under and the signal that made that revision (0 for a revision the first design made), or
// null. A lane released under a revision a signal made waits for that signal to be decided before it goes on (crw:lib/lane).
// refused counts the releases of the lane's path before it that compared another plan (plan_changed): the lane asked again after
// each of them, and only the ask the release answered waits for that signal - the signal may be the one a later plan read of the
// lane brought.
func (s *scriptVM) priorRelease(call goja.FunctionCall) goja.Value {
	node := call.Argument(0).String()
	if s.e.cfg.Resume == nil {
		return goja.Null()
	}
	rel, ok := s.e.standingRow(opRelease, func(d map[string]any) bool { return rowStr(d, "node") == node && rowBoolOf(d, "released") })
	if !ok {
		return goja.Null()
	}
	rev := rowStr(rel.Data, "revision")
	signal := 0
	if led, ok := s.e.standingOrAnyRow(opDesignLedge, func(d map[string]any) bool {
		return rowStr(d, "milestone") == rowStr(rel.Data, "milestone") && rowStr(d, "revision") == rev
	}); ok {
		signal = rowInt(led.Data, "signal")
	}
	refused := 0
	superseded := map[string]int{}
	events := s.e.j.Events()
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Event == EvSuperseded && ev.Seq > superseded[ev.CallID] {
			superseded[ev.CallID] = ev.Seq
		}
	}
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Op == opRelease && ev.Event == EvCompleted && superseded[ev.CallID] <= ev.Seq && ev.Path == rel.Path &&
			ev.Position < rel.Position && rowStr(ev.Data, "node") == node && rowStr(ev.Data, "reason") == releasePlanChanged {
			refused++
		}
	}
	return s.fromJSON(mustJSON(map[string]any{"node": node, "revision": rev, "signal": signal, "call": rel.CallID, "refused": refused}))
}

// standingOrAnyRow is the newest completed row of an op that satisfies keep, whether or not a resume superseded it: what a
// revision was, as a record, does not depend on it standing.
func (e *Engine) standingOrAnyRow(op string, keep func(map[string]any) bool) (Event, bool) {
	events := e.j.Events()
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind == KindCall && ev.Op == op && ev.Event == EvCompleted && keep(ev.Data) {
			return ev, true
		}
	}
	return Event{}, false
}

func rowBoolOf(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}
