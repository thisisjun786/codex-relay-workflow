package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
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
// A row is written once for its key: (report_id, result) and (message_id, result). The flow side
// reads them as it reads the rows a command writes (internal/flow/report).

// FlowForeignSeqBase is the first sequence number of a row a process that is not the run writes
// into flow_events (engine.ForeignSeqBase, which the flow report package checks this against).
const FlowForeignSeqBase int64 = 1 << 40

// The flow_events kinds the settle writes, and the results they carry.
const (
	FlowReportDeliveryKind  = "report_delivery"
	FlowMessageDeliveryKind = "message_delivery"
	FlowDeliveryAccepted    = "accepted"
	FlowDeliveryRefused     = "refused"
)

// flowReportKinds are the direct kinds a flow report row goes up as.
var flowReportKinds = map[string]bool{KindFlowMilestoneReport: true, KindFlowProjectBlocked: true, KindFlowProjectDecisionRequest: true,
	KindFlowJunDecisionRequest: true, KindFlowNoNodeStarted: true}

// IsFlowKind reports whether kind is one a flow run stages (the project kinds and the run's wake).
func IsFlowKind(kind string) bool {
	k, ok := directKinds[kind]
	return ok && (k.prefix == prefixProject || k.prefix == prefixRun)
}

// FlowSubject is the subject of a flow message: the run and what the message is about in it.
func FlowSubject(run, what string) string { return run + "/" + what }

// flowDeliveryResult is the result a settle records for a flow message, "" when it records none:
// accepted for a message the host took (dispatched), refused for one the settle holds at a cap.
func flowDeliveryResult(state string, hold sql.NullString) string {
	switch {
	case state == delivery.Dispatched:
		return FlowDeliveryAccepted
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
	var n int
	if err := q.QueryRowContext(tx, "SELECT COUNT(*) FROM flow_events WHERE kind = ? AND json_extract(payload_json, '$.'||?) = ? AND json_extract(payload_json, '$.result') = ?",
		kind, key, value, d.result).Scan(&n); err != nil {
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
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, "", err
	}
	if _, err := q.ExecContext(tx, "INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
		run, seq, kind, "", "", "", string(raw), d.at); err != nil {
		return false, "", err
	}
	return true, "", nil
}
