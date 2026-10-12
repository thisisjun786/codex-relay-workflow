package engine

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// This file is the walk of a resumed run (engine.resume.rule, F07). A resumed run first reconciles the calls the previous
// process left open (recon.go), then the script runs again from its top and every position it opens is looked up at its own
// (path, position) in the journal the previous processes wrote. A position replays only when every earlier position of its
// path replayed, the positions of the parent path before the call that created the path replayed, its input digest (the
// context boundary and the digests of the joins before it included) and its input snapshot are the ones recorded, and the
// effect proof of what it produced holds. The first position that fails, every later position of the path and every path a
// later position opened are the suffix: they run live from the boundary state suffix.go restores. Other paths, the sibling
// items of a join included, are untouched. There is no reuse across paths: a result belongs to the (path, position) it was
// recorded at.

// priorIndex is what the previous processes journaled, by call: every row written before this run's first row. It is built
// once, after the reconcile pass, and never changes.
type priorIndex struct {
	calls map[string][]Event
	order []string
	// settled is the last settled row of a join or workflow position, by op, path and position.
	settled map[settledKey]Event
	// threads are the threads the previous processes started or forked.
	threads map[string]bool
}

type settledKey struct {
	op, path string
	position int
}

func newPriorIndex(j Journal, startSeq int) *priorIndex {
	ix := &priorIndex{calls: map[string][]Event{}, settled: map[settledKey]Event{}, threads: map[string]bool{}}
	for _, ev := range j.Events() {
		if ev.Seq >= startSeq {
			break
		}
		switch {
		case ev.Kind == KindCall:
			if _, seen := ix.calls[ev.CallID]; !seen {
				ix.order = append(ix.order, ev.CallID)
			}
			ix.calls[ev.CallID] = append(ix.calls[ev.CallID], ev)
			if ev.Event == EvThreadStarted || ev.Event == EvForkStarted {
				ix.threads[rowStr(ev.Data, "thread_id")] = true
			}
		case ev.Kind == KindFlow && (ev.Op == "join" || ev.Op == "workflow") && ev.Event == ev.Op+"_settled":
			ix.settled[settledKey{ev.Op, ev.Path, ev.Position}] = ev
		}
	}
	return ix
}

// rowsOf are the rows of a call the previous processes wrote, of one op ("" for any).
func (ix *priorIndex) rowsOf(callID, op string) []Event {
	if ix == nil {
		return nil
	}
	var out []Event
	for _, ev := range ix.calls[callID] {
		if op == "" || ev.Op == op {
			out = append(out, ev)
		}
	}
	return out
}

// canonicalDigest is the digest of a JSON document in canonical form (object members sorted, numbers as written).
func canonicalDigest(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return digestJSON(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return digestJSON(raw)
	}
	return digestJSON(out)
}

// joinDigestOf is the join digest: the digest of the item paths' final results ordered by path key.
func joinDigestOf(children []map[string]any) string {
	sorted := append([]map[string]any(nil), children...)
	sort.SliceStable(sorted, func(i, k int) bool { return rowStr(sorted[i], "key") < rowStr(sorted[k], "key") })
	return digestJSON(mustJSON(sorted))
}

// joinReplays reports whether a resumed run replays a join or workflow position: nothing under it ran live, the parent path
// replayed up to it, and the digest of its items' results is the one the previous process settled.
func (e *Engine) joinReplays(j *joinState, op, digest string) bool {
	if e.cfg.Resume == nil || j.live || j.parent.suffix {
		return false
	}
	prior, ok := e.prior.settled[settledKey{op, j.parent.id, j.position}]
	return ok && rowStr(prior.Data, "digest") == digest
}

// settleJoin settles a join or workflow position of the parent path: the path is free for its next position, the digest joins
// the chain every later input digest of the path carries, a join that did not replay makes the rest of the path the suffix,
// and the settled row is journaled.
func (e *Engine) settleJoin(parent *pathState, position int, label, op, digest string, children []map[string]any, replayed bool) {
	parent.settle(position)
	if !replayed {
		e.markLive(parent, position+1)
	}
	parent.joins = digestJSON([]byte(parent.joins + "\x00" + digest))
	data := map[string]any{"digest": digest, "replayed": replayed}
	if children != nil {
		data["children"] = children
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Path: parent.id, Position: position, Op: op, Label: label, Event: op + "_settled", Data: data})
}

// markLive records that a position of the path ran live (or that the join at the position before from did not replay): the
// rest of the path is the suffix, the join the path is an item of cannot replay, and the calls the previous processes ended
// in the suffix are ended superseded.
func (e *Engine) markLive(p *pathState, from int) {
	if e.cfg.Resume == nil {
		return
	}
	if p.join != nil {
		p.join.live = true
	}
	if p.suffix {
		return
	}
	p.suffix = true
	e.supersedeSuffix(p, from)
}

// inSuffix reports whether a journaled call at (path, position) is in the suffix that begins at position from of path p: a
// later position of p, or any position of a path a later position of p opened.
func inSuffix(p *pathState, from int, path string, position int) bool {
	if path == p.id {
		return position >= from
	}
	rest, ok := strings.CutPrefix(path, p.id+"/")
	if !ok {
		return false
	}
	num, _, _ := strings.Cut(rest, ":")
	n, err := strconv.Atoi(num)
	return err == nil && n >= from
}

// supersedeSuffix ends, as superseded by the resume, the calls the previous processes ended in the suffix that begins at
// position from of path p. A call whose outcome is unknown stays: it keeps its node in recovery_hold. A call the previous
// process left open is the reconcile pass's and the new attempt's (supersedePrior), which interrupts its turn first.
func (e *Engine) supersedeSuffix(p *pathState, from int) {
	for _, id := range e.prior.order {
		rows := e.prior.calls[id]
		first := rows[0]
		if !inSuffix(p, from, first.Path, first.Position) || e.suffixClaimed(id) {
			continue
		}
		last := ""
		if first.Op == "agent" {
			st := stateOf(id, rowsOfOp(rows, "agent"))
			if st.terminal == "" || st.terminal == EvSuperseded || st.terminal == EvOutcomeUnknown {
				continue
			}
			last = st.terminal
		} else {
			last = rows[len(rows)-1].Event
			if last == EvSuperseded || last == EvRefused {
				continue
			}
		}
		data := map[string]any{"reason": ReasonSupersededByResume, "lineage": e.lineage, "last_event": last}
		if first.Op == "agent" {
			e.supersededRow(id, rowsOfOp(rows, "agent"), first, data)
			continue
		}
		e.j.Append(Event{RunID: e.runID, Kind: KindCall, CallID: id, Path: first.Path, Position: first.Position, Op: first.Op, Label: first.Label, Event: EvSuperseded, Data: data})
	}
}

func rowsOfOp(rows []Event, op string) []Event {
	var out []Event
	for _, ev := range rows {
		if ev.Op == op {
			out = append(out, ev)
		}
	}
	return out
}

// suffixClaimed is whether the resumed script took the call over (it goes on in this process).
func (e *Engine) suffixClaimed(id string) bool {
	e.claimedM.Lock()
	defer e.claimedM.Unlock()
	return e.claimed[id]
}
