package cli

import (
	"context"
	"flag"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var statusCommand = Command{
	Name:  "status",
	Flags: func(f *flag.FlagSet) { f.String("relationship", "", "") },
	Run: func(ctx context.Context, services Services, args Args) (any, error) {
		relationship, _ := args.String("relationship")
		opened, err := openStore(ctx, services)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		payload, err := snapshot(ctx, opened, relationship)
		if err != nil {
			return nil, err
		}
		now := clockNow()
		health, err := observationHealth(ctx, opened, relationship, now)
		if err != nil {
			return nil, err
		}
		payload = append(payload, contract.Field{Key: "observation", Value: health})
		if len(opened.UnenforcedIndexes) > 0 {
			unenforced := []any{}
			for _, index := range opened.UnenforcedIndexes {
				unenforced = append(unenforced, contract.OrderedObject{{Key: "index", Value: index.Index}, {Key: "detail", Value: index.Detail}})
			}
			payload = append(payload, contract.Field{Key: "unenforcedIndexes", Value: unenforced})
		}
		faults, err := faultAttention(ctx, opened, now)
		if err != nil {
			return nil, err
		}
		return append(payload, contract.Field{Key: "faults", Value: faults}), nil
	},
}

var showCommand = Command{
	Name:     "show",
	Required: []string{"event"},
	Flags: func(f *flag.FlagSet) {
		f.String("event", "", "")
		f.Bool("message", false, "include the exact text a recipient was or would be sent")
	},
	Run: func(ctx context.Context, services Services, args Args) (any, error) {
		event, _ := args.String("event")
		opened, err := openStore(ctx, services)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		row, err := opened.One(ctx, "SELECT * FROM events WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, &UsageError{Detail: "no event " + store.PythonRepr(event), Code: contract.ExitUsage}
		}
		queued, err := opened.One(ctx, "SELECT * FROM deliveries WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		attemptRows, err := opened.All(ctx, "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no", event)
		if err != nil {
			return nil, err
		}
		attempts := []any{}
		for _, a := range attemptRows {
			attempts = append(attempts, rowRecord(a))
		}
		ack, err := opened.One(ctx, "SELECT * FROM acks WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		verdict, err := opened.One(ctx, "SELECT * FROM verdicts WHERE event_id = ?", event)
		if err != nil {
			return nil, err
		}
		var deliveryValue, ackRecord, ackVerified, verdictRecord any
		if queued != nil {
			deliveryValue = rowRecord(queued)
		}
		if ack != nil {
			ackRecord, ackVerified = mustLoads(colText(ack, "record")), col(ack, "verified")
		}
		if verdict != nil {
			verdictRecord = mustLoads(colText(verdict, "record"))
		}
		reports, err := workReports(ctx, opened, event)
		if err != nil {
			return nil, err
		}
		var current any
		if len(reports) > 0 {
			current = reports[len(reports)-1]
		}
		restoration, err := restorationEntries(ctx, opened, event)
		if err != nil {
			return nil, err
		}
		payload := contract.OrderedObject{
			{Key: "event", Value: event},
			{Key: "stage", Value: col(row, "stage")},
			{Key: "receipt", Value: mustLoads(colText(row, "receipt"))},
			{Key: "delivery", Value: deliveryValue},
			{Key: "attempts", Value: attempts},
			{Key: "acknowledgement", Value: ackRecord},
			{Key: "acknowledgementVerified", Value: ackVerified},
			{Key: "verdict", Value: verdictRecord},
			{Key: "workReport", Value: current},
			{Key: "workReportSubmissions", Value: reports},
			{Key: "restoration", Value: restoration},
		}
		if queued != nil && args.Bool("message") {
			prepared, err := attemptMessages(ctx, opened, event)
			if err != nil {
				return nil, err
			}
			payload = append(payload, contract.Field{Key: "attemptMessages", Value: prepared})
			if len(prepared) == 0 {
				preview, err := delivery.Preview(ctx, opened, event)
				if err != nil {
					return nil, err
				}
				payload = append(payload, contract.Field{Key: "previewMessage", Value: preview})
			}
		}
		return payload, nil
	},
}

// mustLoads is json.loads on a column this package wrote; unreadable JSON is a host error in
// Python, and a nil here is rendered as null rather than guessed at.
func mustLoads(text string) any { return loads(text) }

// workReports is report.read_all: every submission, oldest first, with its handoff attached.
func workReports(ctx context.Context, s *store.Store, event string) ([]any, error) {
	rows, err := s.All(ctx, "SELECT * FROM work_reports WHERE event_id = ? ORDER BY submission_no", event)
	if err != nil || len(rows) == 0 {
		return []any{}, err
	}
	handoffRows, err := s.All(ctx, "SELECT * FROM work_report_handoffs WHERE event_id = ?", event)
	if err != nil {
		return nil, err
	}
	handoffs := map[int64]contract.OrderedObject{}
	for _, row := range handoffRows {
		number, _ := col(row, "submission_no").(int64)
		orEmptyList := func(name string) any {
			if text := colText(row, name); text != "" {
				return loads(text)
			}
			return []any{}
		}
		handoffs[number] = contract.OrderedObject{
			{Key: "isDraft", Value: truthy(col(row, "is_draft"))},
			{Key: "baseVerifiedAt", Value: col(row, "base_verified_at")},
			{Key: "requiredDeclared", Value: loads(colText(row, "required_declared"))},
			{Key: "checks", Value: loads(colText(row, "checks"))},
			{Key: "reviewCoverage", Value: loads(colText(row, "review_coverage"))},
			{Key: "threadDispositions", Value: loads(colText(row, "thread_dispositions"))},
			{Key: "criterionEvidence", Value: orEmptyList("criterion_evidence")},
			{Key: "limitations", Value: orEmptyList("limitations")},
		}
	}
	jsonOr := func(row store.Row, name string, empty any) any {
		if text := colText(row, name); text != "" {
			return loads(text)
		}
		return empty
	}
	out := []any{}
	for _, row := range rows {
		record := contract.OrderedObject{
			{Key: "eventId", Value: col(row, "event_id")}, {Key: "relationshipId", Value: col(row, "relationship_id")},
			{Key: "executionGeneration", Value: col(row, "execution_generation")}, {Key: "revisionHash", Value: col(row, "revision_hash")},
			{Key: "submissionNo", Value: col(row, "submission_no")}, {Key: "repository", Value: col(row, "repository")},
			{Key: "prNumber", Value: col(row, "pr_number")}, {Key: "prUrl", Value: col(row, "pr_url")},
			{Key: "prState", Value: col(row, "pr_state")}, {Key: "baseRef", Value: col(row, "base_ref")},
			{Key: "baseSha", Value: col(row, "base_sha")}, {Key: "headSha", Value: col(row, "head_sha")},
			{Key: "criteriaDigest", Value: col(row, "criteria_digest")}, {Key: "cxcStatus", Value: col(row, "cxc_status")},
			{Key: "cxcReason", Value: col(row, "cxc_reason")}, {Key: "contractVersion", Value: col(row, "contract_version")},
			{Key: "summary", Value: col(row, "summary")},
			{Key: "evidence", Value: jsonOr(row, "evidence", []any{})},
			{Key: "unresolved", Value: jsonOr(row, "unresolved", []any{})},
			{Key: "nextAction", Value: col(row, "next_action")},
			{Key: "review", Value: jsonOr(row, "review", nil)},
			{Key: "restore", Value: jsonOr(row, "restore", contract.OrderedObject{})},
			{Key: "recordedAt", Value: col(row, "recorded_at")},
		}
		number, _ := col(row, "submission_no").(int64)
		if handoff, ok := handoffs[number]; ok {
			record = append(record, contract.Field{Key: "handoff", Value: handoff})
		}
		out = append(out, record)
	}
	return out, nil
}

// restorationEntries is _restoration_entries.
func restorationEntries(ctx context.Context, s *store.Store, event string) ([]any, error) {
	rows, err := s.All(ctx, "SELECT kind, at, detail FROM journal WHERE subject = ? AND kind IN (?,?,?) ORDER BY seq",
		event, "restoration_projected", "restoration_rendered", "restoration_attempted")
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		if !truthy(col(row, "detail")) {
			continue
		}
		entry, _ := loads(colText(row, "detail")).(contract.OrderedObject)
		entry = append(contract.OrderedObject{}, entry...)
		for _, key := range []string{"kind", "at"} {
			if at := fieldIndex(entry, key); at >= 0 {
				entry[at].Value = col(row, key)
			} else {
				entry = append(entry, contract.Field{Key: key, Value: col(row, key)})
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// attemptMessages is DeliveryService.attempt_messages.
func attemptMessages(ctx context.Context, s *store.Store, event string) ([]any, error) {
	rows, err := s.All(ctx, "SELECT a.request_id, a.attempt_no, a.internal_state, a.state, a.record,"+
		"       a.sent_at, m.message"+
		"  FROM attempts a"+
		"  LEFT JOIN attempt_messages m ON m.request_id = a.request_id"+
		" WHERE a.event_id = ? ORDER BY a.attempt_no", event)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		var record contract.OrderedObject
		if truthy(col(row, "record")) {
			record, _ = loads(colText(row, "record")).(contract.OrderedObject)
		}
		status := "uncertain"
		switch state := colText(row, "state"); {
		case col(row, "message") == nil:
			status = "unavailable"
		case colText(row, "internal_state") != "settled":
			status = "prepared"
		case state == stDispatched || state == "acknowledged":
			status = "dispatched"
		case state == hostLostTurn:
			status = hostLostTurn
		case record != nil && get(record, "sendAttempted") == "no":
			status = "confirmed_unsent"
		}
		out = append(out, contract.OrderedObject{
			{Key: "requestId", Value: col(row, "request_id")}, {Key: "attemptNo", Value: col(row, "attempt_no")},
			{Key: "status", Value: status}, {Key: "deliveryState", Value: col(row, "state")},
			{Key: "sentAt", Value: col(row, "sent_at")}, {Key: "message", Value: col(row, "message")},
		})
	}
	return out, nil
}
