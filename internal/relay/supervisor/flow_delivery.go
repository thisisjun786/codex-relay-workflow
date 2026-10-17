package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The delivery of a flow run's messages (CRW-1380, design CRW-1314 §3). A flow run stages its
// reports, escalations, completion digest and step wakes as direct messages whose subject is
// '<run>/<what it is>'; staging is the run's (or its command's) and writes only queued. What the
// transport came to is the daemon's: the settle that moves such a message to dispatched (accepted)
// or holds it at a cap (refused: the channel stopped retrying on its own), and the busy deferral that
// holds it at busy_cap before any transport call (deferBusy), append one row to the run's
// flow_events in the same transaction, so the row and the message's state commit together
// or not at all. A daemon that dies before the settle commits leaves neither; one that dies after
// it leaves both, and a later settle or a resume of the run finds the row and writes no second.
//
//	report_delivery: the delivery of a report row of the run (its report_id is the subject's rest)
//	message_delivery: the delivery of any other flow message (an escalation, a completion, a wake)
//
// A send whose outcome is unknown (the settle or the recovery of a stranded send moves the message to
// held_uncertain) is recorded too, as unknown, so the report does not read queued for ever; the
// verified readback that settles such a send appends accepted (CRW-1390).
//
// A row is written once for its key: (report_id, result) and (message_id, result). Its subject (the
// column and the payload's _label, as a command's row has them) is the message's subject, and its
// node is the node of the run's row the message is about (an escalation's node; a report row and a
// wake have none). The flow side reads them as it reads the rows a command writes (internal/flow/report).

// FlowForeignSeqBase is the first sequence number of a row a process that is not the run writes
// into flow_events (engine.ForeignSeqBase, which the flow report package checks this against).
const FlowForeignSeqBase int64 = 1 << 40

// The flow_events kinds the settle writes, and the results they carry.
const (
	FlowReportDeliveryKind  = "report_delivery"
	FlowMessageDeliveryKind = "message_delivery"
	FlowDeliveryAccepted    = "accepted"
	FlowDeliveryRefused     = "refused"
	FlowDeliveryUnknown     = "unknown"
)

// flowReportKinds are the direct kinds a flow report row goes up as.
var flowReportKinds = map[string]bool{KindFlowMilestoneReport: true, KindFlowProjectBlocked: true, KindFlowProjectDecisionRequest: true,
	KindFlowJunDecisionRequest: true, KindFlowNoNodeStarted: true, KindFlowMergeRequest: true}

// IsFlowKind reports whether kind is one a flow run stages (the project kinds and the run's wake).
func IsFlowKind(kind string) bool {
	k, ok := directKinds[kind]
	return ok && (k.prefix == prefixProject || k.prefix == prefixRun)
}

// FlowSubject is the subject of a flow message: the run and what the message is about in it.
func FlowSubject(run, what string) string { return run + "/" + what }

// flowDeliveryResult is the result a settle records for a flow message, "" when it records none:
// accepted for a message the host took (dispatched), refused for one the settle holds at a cap,
// unknown for one whose send nobody heard back from (held_uncertain: not repeated, settled only by a
// verified readback).
func flowDeliveryResult(state string, hold sql.NullString) string {
	switch {
	case state == delivery.Dispatched:
		return FlowDeliveryAccepted
	case state == delivery.HeldUncertain:
		return FlowDeliveryUnknown
	case hold.Valid && hold.String != "":
		return FlowDeliveryRefused
	}
	return ""
}

// flowDelivery is what one settle of a flow message came to.
type flowDelivery struct {
	result, requestID, state, hold, at string
	turnID                             any
}

// recordFlowDelivery appends the delivery row of a flow message inside the settle's transaction.
// It fails open: a row it cannot write is journaled (flow_delivery_unrecorded) and the settle
// commits, because the message did go, and a store with no flow journal is not a reason to hold it.
func (c *Channel) recordFlowDelivery(tx context.Context, row store.SupervisorMessagesRow, d flowDelivery) error {
	if d.result == "" || !IsFlowKind(row.ObligationKind) {
		return nil
	}
	run, what, ok := strings.Cut(row.Subject, "/")
	if !ok || run == "" || what == "" {
		return nil
	}
	written, why, err := c.appendFlowDelivery(tx, row, run, what, d)
	if err == nil && (written || why == "") {
		return nil
	}
	reason := why
	if err != nil {
		reason = err.Error()
	}
	detail := pyjson.Dumps(contract.OrderedObject{{Key: "run", Value: run}, {Key: "kind", Value: row.ObligationKind}, {Key: "result", Value: d.result},
		{Key: "requestId", Value: d.requestID}, {Key: "reason", Value: reason}}, pyjson.Options{})
	_, jerr := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'flow_delivery_unrecorded',?,?)", d.at, row.MessageID, detail)
	return jerr
}

// flowDeliveryHeldQuery counts the delivery rows of a key and result among the rows of the message's run, with the arguments
// flowDeliveryHeldArgs gives: the run narrows the read to the run's rows by the (run_id, seq) primary key, inside the settle's write
// transaction, and a key is the run's alone (its message id and report id are derived from the run).
const flowDeliveryHeldQuery = "SELECT COUNT(*) FROM flow_events WHERE run_id = ? AND kind = ? AND json_extract(payload_json, '$.'||?) = ? AND json_extract(payload_json, '$.result') = ?"

func flowDeliveryHeldArgs(run, kind, key, value, result string) []any {
	return []any{run, kind, key, value, result}
}

// appendFlowDelivery writes the row unless one with its key is there. why says why nothing was
// written when that is not because the row exists ("" when it exists).
func (c *Channel) appendFlowDelivery(tx context.Context, row store.SupervisorMessagesRow, run, what string, d flowDelivery) (bool, string, error) {
	q := c.Store.Q(tx)
	var known int
	if err := q.QueryRowContext(tx, "SELECT COUNT(*) FROM flow_runs WHERE run_id = ?", run).Scan(&known); err != nil {
		return false, "", err
	}
	if known == 0 {
		return false, "the store holds no run " + run + ", so the delivery has no journal to go into", nil
	}
	kind, key, value := FlowMessageDeliveryKind, "message_id", row.MessageID
	payload := map[string]any{"message_id": row.MessageID, "kind": row.ObligationKind, "subject": row.Subject, "result": d.result,
		"request_id": d.requestID, "turn_id": d.turnID, "observation": d.state, "reason": d.hold}
	if flowReportKinds[row.ObligationKind] {
		var reports int
		if err := q.QueryRowContext(tx, "SELECT COUNT(*) FROM flow_events WHERE run_id = ? AND kind = 'report' AND json_extract(payload_json, '$.report_id') = ?", run, what).Scan(&reports); err != nil {
			return false, "", err
		}
		if reports > 0 {
			kind, key, value = FlowReportDeliveryKind, "report_id", what
			payload["report_id"] = what
		}
	}
	if row.ObligationKind == KindSessionStepWaiting {
		payload["wake_id"] = what
	}
	node, err := c.flowDeliveryNode(tx, row, run, what, kind)
	if err != nil {
		return false, "", err
	}
	var n int
	if err := q.QueryRowContext(tx, flowDeliveryHeldQuery, flowDeliveryHeldArgs(run, kind, key, value, d.result)...).Scan(&n); err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "", nil
	}
	var top sql.NullInt64
	if err := q.QueryRowContext(tx, "SELECT MAX(seq) FROM flow_events WHERE run_id = ? AND seq >= ?", run, FlowForeignSeqBase).Scan(&top); err != nil {
		return false, "", err
	}
	seq := FlowForeignSeqBase
	if top.Valid {
		seq = top.Int64 + 1
	}
	payload["_kind"] = "flow"
	payload["_label"] = row.Subject
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, "", err
	}
	if _, err := q.ExecContext(tx, "INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
		run, seq, kind, "", node, row.Subject, string(raw), d.at); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// flowDeliveryNode is the node of the run's row a flow message is about: the report row of a report's
// delivery, the escalation row of an escalation (its logical id is esc-<escalation id>), "" for any
// other message (a completion, a wake) and for a row that names none.
func (c *Channel) flowDeliveryNode(tx context.Context, row store.SupervisorMessagesRow, run, what, deliveryKind string) (string, error) {
	kind, key, value := "", "", ""
	switch {
	case deliveryKind == FlowReportDeliveryKind:
		kind, key, value = "report", "report_id", what
	case row.ObligationKind == KindFlowEscalation && strings.HasPrefix(what, "esc-"):
		kind, key, value = "escalation", "escalation_id", strings.TrimPrefix(what, "esc-")
	default:
		return "", nil
	}
	var node sql.NullString
	err := c.Store.Q(tx).QueryRowContext(tx, "SELECT node_id FROM flow_events WHERE run_id = ? AND kind = ? AND json_extract(payload_json, '$.'||?) = ? ORDER BY seq LIMIT 1",
		run, kind, key, value).Scan(&node)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return node.String, err
}

func (a *attemptRun) fenceFlowReportAck(tx context.Context, current store.SupervisorMessagesRow) (bool, error) {
	if !flowReportKinds[current.ObligationKind] {
		return false, nil
	}
	run, reportID, ok := strings.Cut(current.Subject, "/")
	if !ok || run == "" || reportID == "" {
		return false, nil
	}
	var turn sql.NullString
	err := a.c.Store.Q(tx).QueryRowContext(tx, `SELECT json_extract(ack.payload_json,'$.turn') FROM flow_events AS ack
		WHERE ack.run_id=? AND ack.kind='report_read' AND json_extract(ack.payload_json,'$.report_id')=?
		AND EXISTS (SELECT 1 FROM flow_events AS report WHERE report.run_id=ack.run_id AND report.kind='report' AND json_extract(report.payload_json,'$.report_id')=?)
		ORDER BY ack.seq LIMIT 1`, run, reportID, reportID).Scan(&turn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	at := delivery.ISOOf(a.now)
	if a.c.clockISO != nil {
		at = a.c.clockISO()
	}
	record := withheldRecord(a.claimed.requestID, a.id, a.claimed.attemptNo, map[string]any{"reason": "report_ack", "messageState": "read"})
	if err := a.c.Store.SettleSupervisorAttempt(tx, a.claimed.requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
		return true, err
	}
	moved, err := a.c.Store.SettleSupervisorMessage(tx, a.id, "read", sql.NullFloat64{}, sql.NullString{}, at, "sending", a.claimed.attemptNo, sql.NullString{String: a.owner, Valid: true})
	if err != nil {
		return true, err
	}
	if !moved {
		return true, Refusal{"not_claimable", "the report acknowledgement no longer holds this message's claim"}
	}
	_, why, err := a.c.appendFlowDelivery(tx, current, run, reportID, flowDelivery{result: FlowDeliveryAccepted, requestID: a.claimed.requestID, state: "read", hold: "report_ack", at: at, turnID: optionalText(turn)})
	if err != nil {
		return true, err
	}
	if why != "" {
		return true, errors.New(why)
	}
	return true, a.c.directJournal(tx, at, "supervisor_message_read", a.id, map[string]any{"by": "report_ack", "runId": run, "reportId": reportID, "readTurnId": optionalText(turn), "requestId": a.claimed.requestID, "sendAttempted": "no"})
}
