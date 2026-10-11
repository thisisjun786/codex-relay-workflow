package ops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the run's import of the native spawns the hook decided on its bound threads (F14, CRW-1207). The hook records,
// for each spawn of a bound thread, the pair the caller sent, the pair it applied, the decision and the reason, the call and the
// tool use id (bound.Decision, the thread's ledger). After an engine turn the run joins that ledger with what the host says about
// the same turn:
//
//   - the turn's CollabAgentToolCall items (tool spawnAgent): the item whose id is the tool use id gives the model and effort the
//     spawn was *requested* with (that is what the schema calls them) and receiverThreadIds, the threads the call created;
//   - thread/read of each receiver: Thread.model and Thread.reasoningEffort, the pair the receiver is *configured* with.
//
// Three values stay apart in every observation: requested (what the caller sent, from the ledger; the item's own request is kept
// beside it), applied (what the hook set) and configured (what the receiver reads back). A receiver that is missing, cannot be
// read or reads a null model or effort is unobserved, and an unobserved pair is never evidence that the role pair held. A
// configured pair that differs from the applied one is pair_mismatch. A CollabAgentToolCall of a bound thread with no applied
// record is unbound_spawn: the hook did not decide that spawn (its binding file may have been removed, or the hook and the run
// read different homes). Both of those escalate as configuration faults.

// NativeHost is what the import reads from the host.
type NativeHost interface {
	// TurnItems returns the items of the turn read by its recorded id, in full.
	TurnItems(ctx context.Context, threadID, turnID string) ([]json.RawMessage, error)
	// ThreadConfig is thread/read of a thread: Thread.model and Thread.reasoningEffort, nil for a null.
	ThreadConfig(ctx context.Context, threadID string) (model, effort *string, err error)
}

// NativeTurn names the engine turn whose spawns are imported.
type NativeTurn struct {
	RunID, ThreadID, CallID, TurnID string
	// Node is the node of the call ("" for none).
	Node string
}

// The kinds and decisions of the rows an import produces.
const (
	// NativeKindSpawn is a row of the hook's decision about one spawn: decision allowed or refused, reason the refusal.
	NativeKindSpawn = "native_spawn"
	// NativeKindObservation is a row of what the host showed about one allowed spawn, or about a spawn nobody decided.
	NativeKindObservation = "native_observation"
	// The decisions of an observation row.
	ObservationObserved     = "observed"
	ObservationUnobserved   = "unobserved"
	ObservationPairMismatch = "pair_mismatch"
	ObservationUnbound      = "unbound_spawn"
	// NativeRule is the rule id of the rows (the native helper of a flow-bound thread, SP-NAT-1).
	NativeRule = "SP-NAT-1"
)

// The reasons of an unobserved row.
const (
	UnobservedNoReceiver = "no_receiver"
	UnobservedReadFailed = "read_failed"
	UnobservedNullModel  = "null_model"
	UnobservedNullEffort = "null_effort"
)

// SpawnRow is one row of flow_spawn_decisions.
type SpawnRow struct {
	DecisionID, RunID, Kind, CallID, NodeID, RuleID, Decision, Reason string
	RequestedModel, RequestedEffort, AppliedModel, AppliedEffort      string
	ToolUseID                                                         string
	// Evidence is evidence_json: what the row rests on.
	Evidence  map[string]any
	DecidedAt time.Time
}

// NativeEscalation is a configuration fault the run escalates: reason is unbound_spawn or pair_mismatch.
type NativeEscalation struct {
	Reason, CallID, ToolUseID, Node string
	Detail                          map[string]any
}

// NativeImport is what an import found.
type NativeImport struct {
	Rows        []SpawnRow
	Escalations []NativeEscalation
}

// collabItem is the CollabAgentToolCall item of a turn.
type collabItem struct {
	Type            string   `json:"type"`
	ID              string   `json:"id"`
	Tool            string   `json:"tool"`
	Status          string   `json:"status"`
	SenderThreadID  string   `json:"senderThreadId"`
	ReceiverThreads []string `json:"receiverThreadIds"`
	Model           *string  `json:"model"`
	ReasoningEffort *string  `json:"reasoningEffort"`
}

// ImportNativeSpawns joins the hook's ledger of the engine thread (under the flow-bound directory dir) with the items of the turn
// and the configuration of the threads it created. The rows are deterministic (their ids depend on the facts only), so an import
// repeated for the same turn produces the rows again and StoreSpawnRows skips the stored ones. An error means the ledger or the
// turn could not be read; nothing is imported then.
func ImportNativeSpawns(ctx context.Context, host NativeHost, dir string, turn NativeTurn, now time.Time) (NativeImport, error) {
	ledger, err := bound.ReadDecisions(dir, turn.ThreadID)
	if err != nil {
		return NativeImport{}, fmt.Errorf("ops: the spawn ledger of thread %s: %w", turn.ThreadID, err)
	}
	raws, err := host.TurnItems(ctx, turn.ThreadID, turn.TurnID)
	if err != nil {
		return NativeImport{}, fmt.Errorf("ops: the items of turn %s: %w", turn.TurnID, err)
	}
	var out NativeImport
	applied := map[string]bound.Decision{} // tool use id → the allowed decision
	for _, d := range ledger {
		if d.CallID != turn.CallID {
			continue
		}
		out.Rows = append(out.Rows, decisionRow(turn, d, now))
		if _, ok := applied[d.ToolUseID]; !ok && d.Decision == bound.DecisionAllowed && d.ToolUseID != "" {
			applied[d.ToolUseID] = d
		}
	}
	seen := map[string]bool{}
	for _, raw := range raws {
		var item collabItem
		if json.Unmarshal(raw, &item) != nil || item.Type != "collabAgentToolCall" || item.Tool != "spawnAgent" || item.ID == "" ||
			(item.SenderThreadID != "" && item.SenderThreadID != turn.ThreadID) || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		d, ok := applied[item.ID]
		if !ok {
			out.unbound(turn, item, ledger, now)
			continue
		}
		out.observe(ctx, host, turn, item, d, now)
	}
	return out, nil
}

// rowID is the stable id of a row: the hash of the facts that make it.
func rowID(parts ...string) string {
	h := sha256.New()
	h.Write([]byte("crw-native-spawn"))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "ns_" + hex.EncodeToString(h.Sum(nil))[:32]
}

func decisionRow(turn NativeTurn, d bound.Decision, now time.Time) SpawnRow {
	return SpawnRow{
		DecisionID: rowID(turn.RunID, turn.ThreadID, turn.CallID, d.ToolUseID, NativeKindSpawn, d.Decision, d.Reason),
		RunID:      turn.RunID, Kind: NativeKindSpawn, CallID: turn.CallID, NodeID: turn.Node, RuleID: NativeRule, Decision: d.Decision, Reason: d.Reason,
		RequestedModel: d.RequestedModel, RequestedEffort: d.RequestedEffort, AppliedModel: d.AppliedModel, AppliedEffort: d.AppliedEffort, ToolUseID: d.ToolUseID,
		Evidence:  map[string]any{"thread_id": turn.ThreadID, "thread_kind": d.ThreadKind, "helper_role": d.HelperRole, "detail": d.Detail},
		DecidedAt: now,
	}
}

// unbound records a spawn the hook did not allow: no applied record at all, or only a refusal (the host ran a spawn the hook
// refused).
func (out *NativeImport) unbound(turn NativeTurn, item collabItem, ledger []bound.Decision, now time.Time) {
	why := "no_applied_record"
	for _, d := range ledger {
		if d.ToolUseID == item.ID && d.CallID == turn.CallID {
			why = "only_a_refusal_recorded"
		}
	}
	out.Rows = append(out.Rows, SpawnRow{
		DecisionID: rowID(turn.RunID, turn.ThreadID, turn.CallID, item.ID, NativeKindObservation, ObservationUnbound),
		RunID:      turn.RunID, Kind: NativeKindObservation, CallID: turn.CallID, NodeID: turn.Node, RuleID: NativeRule, Decision: ObservationUnbound, Reason: why,
		RequestedModel: strOf(item.Model), RequestedEffort: strOf(item.ReasoningEffort), ToolUseID: item.ID,
		Evidence:  map[string]any{"thread_id": turn.ThreadID, "item": itemEvidence(item)},
		DecidedAt: now,
	})
	out.Escalations = append(out.Escalations, NativeEscalation{Reason: ObservationUnbound, CallID: turn.CallID, ToolUseID: item.ID, Node: turn.Node,
		Detail: map[string]any{"thread_id": turn.ThreadID, "why": why, "receivers": item.ReceiverThreads}})
}

func strOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func itemEvidence(item collabItem) map[string]any {
	return map[string]any{"status": item.Status, "model": item.Model, "reasoningEffort": item.ReasoningEffort, "receiverThreadIds": item.ReceiverThreads}
}

// observe reads the configured pair of each receiver of an allowed spawn and records it next to the requested and the applied
// pair.
func (out *NativeImport) observe(ctx context.Context, host NativeHost, turn NativeTurn, item collabItem, d bound.Decision, now time.Time) {
	row := func(receiver, decision, reason string, observed map[string]any) SpawnRow {
		return SpawnRow{
			DecisionID: rowID(turn.RunID, turn.ThreadID, turn.CallID, item.ID, NativeKindObservation, receiver),
			RunID:      turn.RunID, Kind: NativeKindObservation, CallID: turn.CallID, NodeID: turn.Node, RuleID: NativeRule, Decision: decision, Reason: reason,
			RequestedModel: d.RequestedModel, RequestedEffort: d.RequestedEffort, AppliedModel: d.AppliedModel, AppliedEffort: d.AppliedEffort, ToolUseID: item.ID,
			Evidence: map[string]any{"thread_id": turn.ThreadID, "receiver_thread_id": receiver, "observed": observed, "item": itemEvidence(item),
				"requested": map[string]any{"model": d.RequestedModel, "effort": d.RequestedEffort}, "applied": map[string]any{"model": d.AppliedModel, "effort": d.AppliedEffort}},
			DecidedAt: now,
		}
	}
	if len(item.ReceiverThreads) == 0 {
		out.Rows = append(out.Rows, row("", ObservationUnobserved, UnobservedNoReceiver, nil))
		return
	}
	for _, receiver := range slices.Compact(slices.Sorted(slices.Values(item.ReceiverThreads))) {
		model, effort, err := host.ThreadConfig(ctx, receiver)
		if err != nil {
			out.Rows = append(out.Rows, row(receiver, ObservationUnobserved, UnobservedReadFailed, map[string]any{"error": err.Error()}))
			continue
		}
		observed := map[string]any{"model": model, "effort": effort}
		differs := model != nil && *model != d.AppliedModel || effort != nil && *effort != d.AppliedEffort
		switch {
		case differs:
			out.Rows = append(out.Rows, row(receiver, ObservationPairMismatch, "", observed))
			out.Escalations = append(out.Escalations, NativeEscalation{Reason: ObservationPairMismatch, CallID: turn.CallID, ToolUseID: item.ID, Node: turn.Node,
				Detail: map[string]any{"thread_id": turn.ThreadID, "receiver_thread_id": receiver, "applied": map[string]any{"model": d.AppliedModel, "effort": d.AppliedEffort}, "configured": observed}})
		case model == nil:
			out.Rows = append(out.Rows, row(receiver, ObservationUnobserved, UnobservedNullModel, observed))
		case effort == nil:
			out.Rows = append(out.Rows, row(receiver, ObservationUnobserved, UnobservedNullEffort, observed))
		default:
			out.Rows = append(out.Rows, row(receiver, ObservationObserved, "", observed))
		}
	}
}

// SQL is the part of *sql.DB (or *sql.Tx) StoreSpawnRows writes through.
type SQL interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// StoreSpawnRows appends the rows to flow_spawn_decisions in the relay store, those whose id is not stored yet. The run's row in
// flow_runs must exist. It returns how many rows it wrote.
func StoreSpawnRows(ctx context.Context, db SQL, rows []SpawnRow) (int, error) {
	n := 0
	for _, r := range rows {
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM flow_spawn_decisions WHERE decision_id = ?`, r.DecisionID).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return n, err
		}
		evidence, err := json.Marshal(r.Evidence)
		if err != nil {
			return n, err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO flow_spawn_decisions (decision_id, run_id, kind, call_id, node_id, rule_id, decision, reason, requested_model, requested_effort,
 applied_model, applied_effort, tool_use_id, evidence_json, decided_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.DecisionID, r.RunID, r.Kind, r.CallID, r.NodeID, r.RuleID, r.Decision, r.Reason, r.RequestedModel, r.RequestedEffort,
			r.AppliedModel, r.AppliedEffort, r.ToolUseID, string(evidence), r.DecidedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// EscalateNative appends the escalations of an import to the run's journal as escalation rows, class configuration_fault, once
// for each reason and tool use: an import repeated for the same turn escalates nothing more.
func EscalateNative(j engine.Journal, runID string, escalations []NativeEscalation) {
	done := map[string]bool{}
	for _, e := range j.Events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
			r, _ := e.Data["reason"].(string)
			id, _ := e.Data["tool_use_id"].(string)
			done[r+"\x00"+id] = true
		}
	}
	for _, e := range escalations {
		key := e.Reason + "\x00" + e.ToolUseID
		if done[key] {
			continue
		}
		done[key] = true
		data := map[string]any{"reason": e.Reason, "class": "configuration_fault", "node": e.Node, "call_id": e.CallID, "tool_use_id": e.ToolUseID}
		for k, v := range e.Detail {
			data[k] = v
		}
		j.Append(engine.Event{RunID: runID, Kind: engine.KindFlow, CallID: e.CallID, Op: "agent", Event: engine.EvEscalation, Data: data})
	}
}
