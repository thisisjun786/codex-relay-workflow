package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// NodeProof is what the live worktree of a node says about the executor calls of its current attempt.
type NodeProof interface {
	// Proven reports whether the effect of an executor call still holds: its recorded state (the pending tree P of a
	// completed call, P_held of a held one) is the state of the files, or a recorded commit carries it.
	Proven(callID string) bool
}

// Prover proves the effect of executor calls against live state (internal/flow/ops implements it). A resumed run replays a
// completed executor call only when its effect holds, and resumes a held one in place only when the worktree is still the
// snapshot the hold recorded. A Backend that is a Prover is used when Config.Prover is nil; a run without either has
// nothing to prove an effect against, so every effect holds.
type Prover interface {
	ProveNode(ctx context.Context, events []Event, node string) (NodeProof, error)
}

// Op is a crw.<name> call whose effect lies outside the engine: the commit of a pending tree, a verification, a review, an
// acceptance, an integration, a hold, a report to the relay (F08 registers the real ones in Config.Ops). The engine gives the
// call a position of its path, journals its completed row and, when it resumes, replays the row instead of running the op
// again - but only when Proven says the effect is still there. An op that ran without its row being written is run again
// under the same call id and the same RequestID, so the world sees one request.
type Op interface {
	// Run does the op. args are the call's arguments as a JSON array.
	Run(ctx context.Context, req OpRequest) (OpResult, error)
	// Proven reports whether the effect of a completed row of the op still holds, and why not when it does not.
	Proven(ctx context.Context, events []Event, row Event) (bool, string, error)
}

// OpRequest is one run of an op.
type OpRequest struct {
	RunID, CallID string
	// RequestID is the idempotent request id of the attempt: the same for the same call of the same run while no row ended an
	// attempt of it, and another one (AttemptRequestID) for an attempt that supersedes an ended one. The completed row names
	// it (request_id), so an op's Proven can find the request a row made.
	RequestID string
	Args      json.RawMessage
	// Journal is the run's journal.
	Journal Journal
}

// OpResult is what an op did: Result is the value the script gets (JSON), Data is added to the completed row (the commit
// and tree of a commit, the acceptance id of an acceptance) where the effect proofs read it.
type OpResult struct {
	Result json.RawMessage
	Data   map[string]any
}

// RequestID is the idempotent request id a recording call sends with: the same for the same call of the same run, so a call
// sent again after a crash is recognised by whoever recorded it the first time (internal/flow/ops.RequestID is this).
func RequestID(runID, callID string) string {
	s := sha256.Sum256([]byte("crw-flow-record\x00" + runID + "\x00" + callID))
	return "req_" + hex.EncodeToString(s[:])[:24]
}

// AttemptRequestID is the request id of the nth attempt of a call's operation (n counts the attempts the journal already ended
// with a row, from 0): the first is RequestID, each attempt that supersedes an ended one has an id of its own, so a receiver that
// deduplicates by request id sees a new request and not the old one again.
func AttemptRequestID(runID, callID string, n int) string {
	if n <= 0 {
		return RequestID(runID, callID)
	}
	s := sha256.Sum256([]byte(fmt.Sprintf("crw-flow-record\x00%s\x00%s\x00attempt\x00%d", runID, callID, n)))
	return "req_" + hex.EncodeToString(s[:])[:24]
}

// opRequestID is the request id the op of a position runs with now. An attempt that left no row (the process died before the
// op's row was written) is run again under the id it had, so the world sees one request. An attempt the journal ended -
// a completed row, or a refusal of other arguments - is superseded by this one (the arguments changed, or a position before
// this one did not replay), and this one is a request of its own.
func (e *Engine) opRequestID(callID, op, digest string) string {
	ended := 0
	for _, ev := range e.j.Events() {
		if ev.Kind != KindCall || ev.CallID != callID || ev.Op != op {
			continue
		}
		if ev.Event == EvCompleted || (ev.Event == EvRefused && rowStr(ev.Data, "input_digest") != digest) {
			ended++
		}
	}
	return AttemptRequestID(e.runID, callID, ended)
}

// builtinOps are the crw members the engine defines itself.
var builtinOps = map[string]bool{"classify": true, "clock": true, "tierStep": true, "callFacts": true, "recordedSignal": true, "priorRelease": true}

// opPrefix is the op name of a registered op's rows.
const opPrefix = "crw."

// installOps defines the registered ops on the crw object.
func (s *scriptVM) installOps(crw *goja.Object) error {
	for name := range s.e.cfg.Ops {
		if builtinOps[name] || strings.ContainsAny(name, ". ") || name == "" {
			return fmt.Errorf("engine: %q cannot be registered as an op", name)
		}
		_ = crw.Set(name, func(call goja.FunctionCall) goja.Value { return s.opCall(name, call) })
	}
	return nil
}

// opCall is a registered op: a position of its path whose completed row is replayed when the previous process recorded it
// with the same arguments and its effect still holds.
func (s *scriptVM) opCall(name string, call goja.FunctionCall) goja.Value {
	s.live()
	label := opPrefix + name
	args := make([]json.RawMessage, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		raw, err := s.jsonOfErr(a)
		if err != nil {
			s.throwType("%s: the arguments cannot be serialised: %v", label, err)
		}
		args = append(args, raw)
	}
	argsJSON := mustJSON(args)
	path, position := s.open(label)
	e := s.e
	id := e.callID(path.id, position)
	digest := digestJSON(mustJSON(map[string]any{"op": label, "args": canonicalDigest(argsJSON), "joins": path.joins}))
	promise, resolve, reject := s.vm.NewPromise()
	complete := e.loop.Begin(s.rt)
	op := e.cfg.Ops[name]

	if prior, ok := e.replayableOp(path, id, label, digest); ok {
		if proven, _, err := op.Proven(e.ctx, e.j.Events(), prior); err == nil && proven {
			e.noteReplayed(prior)
			complete(func() {
				path.settle(position)
				if err := resolve(s.fromJSON([]byte(rowStr(prior.Data, "result")))); err != nil {
					e.failResolve(err)
				}
			})
			return s.vm.ToValue(promise)
		}
	}
	e.markLive(path, position)
	requestID := e.opRequestID(id, label, digest)
	e.inflight.Add(1)
	go func() {
		defer e.inflight.Done()
		res, err := op.Run(e.ctx, OpRequest{RunID: e.runID, CallID: id, RequestID: requestID, Args: argsJSON, Journal: e.j})
		if err == nil && len(res.Result) == 0 {
			res.Result = json.RawMessage("null")
		}
		data := map[string]any{"input_digest": digest, "request_id": requestID}
		for k, v := range res.Data {
			data[k] = v
		}
		if err != nil {
			data["reason"], data["detail"] = "op_failed", err.Error()
			e.j.Append(Event{RunID: e.runID, Kind: KindCall, CallID: id, Path: path.id, Position: position, Op: label, Label: label, Event: EvRefused, Data: data})
		} else {
			data["result"] = string(res.Result)
			// a commit this process made moves the boundary of its node as a replayed one does
			e.noteOp(e.j.Append(Event{RunID: e.runID, Kind: KindCall, CallID: id, Path: path.id, Position: position, Op: label, Label: label, Event: EvCompleted, Data: data}))
		}
		if jerr := journalErr(e.j); jerr != nil {
			e.journalFailed(jerr)
		}
		complete(func() {
			path.settle(position)
			var rerr error
			if err != nil {
				rerr = reject(s.vm.NewGoError(fmt.Errorf("%s: %w", label, err)))
			} else {
				rerr = resolve(s.fromJSON(res.Result))
			}
			if rerr != nil {
				e.failResolve(rerr)
			}
		})
	}()
	return s.vm.ToValue(promise)
}

// replayableOp is the completed row a resumed run may replay at a position of an op: the path replayed up to it, and the
// last row the previous processes wrote for the call is a completed row of the same op and the same input.
func (e *Engine) replayableOp(path *pathState, callID, op, digest string) (Event, bool) {
	if e.cfg.Resume == nil || path.suffix {
		return Event{}, false
	}
	rows := e.prior.rowsOf(callID, op)
	if len(rows) == 0 {
		return Event{}, false
	}
	last := rows[len(rows)-1]
	if last.Event != EvCompleted || rowStr(last.Data, "input_digest") != digest {
		return Event{}, false
	}
	return last, true
}
