package registry

import (
	"context"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// dispositions.py: what each live child's current turn did, and whether the store observed it
// being sent. Read through store.ReadOnlyRows, so no store is created or migrated.

// executionOnly is dispositions.EXECUTION_ONLY (delivery.EXECUTION_ONLY_OUTCOMES).
var executionOnly = []string{"failed", "interrupted", "blocked_needs_input"}

// Send-axis and receiving-axis words (dispositions.py).
const (
	obsNotSent         = "not_sent"
	obsSendUncertain   = "send_uncertain"
	obsStoredNotWoken  = "stored_not_woken"
	obsSent            = "dispatched"
	obsSuperseded      = "superseded"
	obsUnrecognised    = "state_unrecognised"
	obsRefusedPreQueue = "refused_pre_queue"
	obsSuppressed      = "suppressed"
	obsUnmeasured      = "unmeasured"
	obsDisagree        = "records_disagree"
	obsHostRead        = "observed_host_read"
	obsClaimed         = "observed_claimed"
)

// observationByState is dispositions.OBSERVATION_BY_STATE, keyed by every delivery state.
var observationByState = map[string]string{
	"queued": obsNotSent, "deferred_busy": obsNotSent, "withheld_pre_send": obsNotSent,
	"sending": obsSendUncertain, "held_uncertain": obsSendUncertain,
	"inbox_only": obsStoredNotWoken, "dispatched": obsSent, "acknowledged": obsSent, "superseded": obsSuperseded,
}

// observationDetail is dispositions.OBSERVATION_DETAIL; SENT has no detail (None).
var observationDetail = map[string]any{
	obsNotSent:        "the delivery exists and nothing has been sent yet",
	obsSendUncertain:  "a send is in flight or answered unusably, which is not evidence of non-delivery",
	obsStoredNotWoken: "stored where the recipient reads it, with no turn woken; a durable inbox item is not a successful wake",
	obsSent:           nil,
	obsSuperseded:     "this delivery is no longer what the assignment stands on; its state is left alone so reconciliation can still settle it",
}

// Caller-visible prose, byte-identical to dispositions.py.
const (
	UnmeasuredDetail = "nothing in this store establishes whether a delivery was ever attempted or even wanted for " +
		"this event: absence of a delivery row covers an event emitted with --no-enqueue, an event " +
		"stranded by an old generation, and a refusal recorded before delivery intents existed, and " +
		"those are not merged into 'not delivered'"
	RecipientUnmeasuredDetail = "no acknowledgement and no acknowledgement evidence is recorded, so whether the recipient " +
		"observed this event is not measured here; a dispatched delivery is not an acknowledgement and " +
		"not a verification"
	DispositionReadingLimits = "A reading of what this scope's children last reported and what the store observed about " +
		"sending it, not a completion verdict and not an assignment state. The send axis is derived " +
		"from the relay's own records and the receiving axis from acknowledgement evidence; unmeasured " +
		"means nobody established the fact, never that the fact is false. Which reviewable event is " +
		"the current head is not decided here - read it with assignment-show - and neither is " +
		"integration or verification. unreadable, an empty list and 'nothing is wrong' are three " +
		"different answers and none of them means finished."
	RefusedPreQueueDetail = "delivery was wanted for this event and refused for a reason that may not last"
	ClaimedDetail         = "an acknowledgement is recorded, but its own turn has not been established by a host " +
		"read, so it is the parent's claim rather than an observation of it"
	EvidenceWithoutAckDetail = "acknowledgement evidence is recorded for this event with no acknowledgement beside " +
		"it, so whether the recipient observed it has no supportable answer"
	NoDispositionDetail = "no final execution-only disposition in this generation, which is not the same as nothing being wrong"
	SameOutcomeDetail   = "several events report the same outcome in this generation; the newest anchors it and every candidate is listed"
	NoIdentityDetail    = "the store answered without its own identity, so these rows cannot be attributed to a relay database"
	UnopenedDetail      = "the store could not be opened for reading"
)

// dispositionsSQL is dispositions._SQL.
var dispositionsSQL = "SELECT 'meta' AS kind," +
	"       (SELECT value FROM schema_meta WHERE key = 'store_id') AS store_id," +
	"       NULL AS relationship_id, NULL AS issue_key, NULL AS relationship_status," +
	"       NULL AS parent_task_id, NULL AS child_task_id, NULL AS execution_generation," +
	"       NULL AS project_key, NULL AS reviewable_count, NULL AS earlier_events," +
	"       NULL AS event_id, NULL AS outcome, NULL AS producer, NULL AS stage," +
	"       NULL AS suppressed_reason, NULL AS turn_id, NULL AS attempt," +
	"       NULL AS became_final_at, NULL AS ordering_at," +
	"       NULL AS report_submission, NULL AS cxc_status, NULL AS cxc_reason," +
	"       NULL AS next_action," +
	"       NULL AS delivery_event, NULL AS delivery_state, NULL AS delivery_kind," +
	"       NULL AS recipient_task_id, NULL AS attempt_count, NULL AS hold_reason," +
	"       NULL AS dispatch_turn_id, NULL AS dispatch_evidence," +
	"       NULL AS intent_event, NULL AS intent_attempts, NULL AS intent_error," +
	"       NULL AS intent_next_retry_at," +
	"       NULL AS supersession_reason, NULL AS supersession_applied," +
	"       NULL AS request_id, NULL AS attempt_no, NULL AS attempt_internal_state," +
	"       NULL AS attempt_state, NULL AS attempt_sent_at," +
	"       NULL AS ack_event, NULL AS ack_accepted, NULL AS ack_rejection," +
	"       NULL AS ack_verified," +
	"       NULL AS ack_evidence_event, NULL AS ack_tier," +
	"       NULL AS superseded_by, NULL AS correction_event, NULL AS correction_state," +
	"       NULL AS correction_attempts, NULL AS correction_hold," +
	"       NULL AS correction_next_eligible_at, NULL AS correction_lifecycle_withhold," +
	"       NULL AS correction_lifecycle_recorded_at," +
	"       NULL AS correction_lifecycle_next_retry_at," +
	"       NULL AS correction_supersession_reason, NULL AS correction_supersession_applied" +
	" UNION ALL" +
	" SELECT 'child' AS kind, NULL AS store_id," +
	"        r.relationship_id AS relationship_id, r.issue_key AS issue_key," +
	"        r.status AS relationship_status, r.parent_task_id AS parent_task_id," +
	"        r.child_task_id AS child_task_id," +
	"        r.execution_generation AS execution_generation, s.project_key AS project_key," +
	"        (SELECT COUNT(*) FROM events rv" +
	"          WHERE rv.relationship_id = r.relationship_id" +
	"            AND rv.execution_generation = r.execution_generation" +
	"            AND rv.outcome = 'ready_for_review' AND rv.stage = 'final'" +
	"            AND rv.suppressed_reason IS NULL) AS reviewable_count," +
	"        (SELECT COUNT(*) FROM events eg" +
	"          WHERE eg.relationship_id = r.relationship_id" +
	"            AND eg.execution_generation <> r.execution_generation" +
	"            AND eg.outcome IN ('failed','interrupted','blocked_needs_input'))" +
	"            AS earlier_events," +
	"        e.event_id AS event_id, e.outcome AS outcome, e.producer AS producer," +
	"        e.stage AS stage, e.suppressed_reason AS suppressed_reason, e.turn_id AS turn_id," +
	"        e.attempt AS attempt, e.finalized_at AS became_final_at," +
	"        COALESCE(e.finalized_at, e.first_seen_at) AS ordering_at," +
	"        w.submission_no AS report_submission, w.cxc_status AS cxc_status," +
	"        w.cxc_reason AS cxc_reason, w.next_action AS next_action," +
	"        d.event_id AS delivery_event, d.state AS delivery_state, d.kind AS delivery_kind," +
	"        d.recipient_task_id AS recipient_task_id, d.attempt_count AS attempt_count," +
	"        d.hold_reason AS hold_reason, d.dispatch_turn_id AS dispatch_turn_id," +
	"        d.dispatch_evidence AS dispatch_evidence," +
	"        i.event_id AS intent_event, i.attempts AS intent_attempts," +
	"        i.last_error AS intent_error, i.next_retry_at AS intent_next_retry_at," +
	"        x.reason AS supersession_reason, x.applied AS supersession_applied," +
	"        a.request_id AS request_id, a.attempt_no AS attempt_no," +
	"        a.internal_state AS attempt_internal_state, a.state AS attempt_state," +
	"        a.sent_at AS attempt_sent_at," +
	"        k.event_id AS ack_event, k.accepted AS ack_accepted," +
	"        k.rejection_reason AS ack_rejection, k.verified AS ack_verified," +
	"        v.event_id AS ack_evidence_event, v.tier AS ack_tier," +
	"        r.superseded_by AS superseded_by, ce.event_id AS correction_event," +
	"        cd.state AS correction_state, cd.attempt_count AS correction_attempts," +
	"        cd.hold_reason AS correction_hold," +
	"        cd.next_eligible_at AS correction_next_eligible_at," +
	"        cf.error_code AS correction_lifecycle_withhold," +
	"        cf.occurred_at AS correction_lifecycle_recorded_at," +
	"        cf.next_retry_at AS correction_lifecycle_next_retry_at," +
	"        cx.reason AS correction_supersession_reason," +
	"        cx.applied AS correction_supersession_applied" +
	"   FROM relationships r" +
	"   LEFT JOIN relationship_scope s ON s.relationship_id = r.relationship_id" +
	"   LEFT JOIN events e ON e.relationship_id = r.relationship_id" +
	"                     AND e.execution_generation = r.execution_generation" +
	"                     AND e.outcome IN ('ready_for_review','failed','interrupted'," +
	"                                       'blocked_needs_input')" +
	"   LEFT JOIN work_reports w ON w.event_id = e.event_id" +
	"                           AND w.submission_no = (SELECT MAX(submission_no)" +
	"                                                    FROM work_reports" +
	"                                                   WHERE event_id = e.event_id)" +
	"   LEFT JOIN deliveries d ON d.event_id = e.event_id" +
	"   LEFT JOIN attempts a ON a.event_id = e.event_id AND a.attempt_no = d.attempt_count" +
	"   LEFT JOIN delivery_intent i ON i.event_id = e.event_id" +
	"   LEFT JOIN delivery_supersession x ON x.event_id = e.event_id" +
	"   LEFT JOIN acks k ON k.event_id = e.event_id" +
	"   LEFT JOIN ack_evidence v ON v.event_id = e.event_id" +
	"   LEFT JOIN events ce ON ce.event_id = (SELECT MIN(rq.event_id) FROM events rq" +
	"                                          WHERE rq.relationship_id = r.relationship_id" +
	"                                            AND rq.execution_generation" +
	"                                                = r.execution_generation" +
	"                                            AND rq.outcome = 'revision_request'" +
	"                                            AND rq.suppressed_reason IS NULL)" +
	"   LEFT JOIN deliveries cd ON cd.event_id = ce.event_id" +
	"   LEFT JOIN delivery_supersession cx ON cx.event_id = ce.event_id" +
	lifecycleWithholdJoin("cf", "ce", "cd") +
	"  WHERE (? IS NULL OR (s.project_key = ?" +
	"                       AND r.status IN ('active','paused') AND r.superseded_by IS NULL))" +
	"    AND (? IS NULL OR r.relationship_id = ?)" +
	" ORDER BY kind, relationship_id, ordering_at, event_id"

// dispRow is one row of dispositionsSQL by column name.
type dispRow map[string]any

func (r dispRow) s(name string) string { v, _ := r[name].(string); return v }

// rowsReader is store.ReadOnlyRows, replaceable so a test can hand back a read that saw a
// replaced database (DSP-10).
type rowsReader func(ctx context.Context, selection store.StateSelection, query string, args []any, each func(store.RowScanner) error) store.RowsRead

// dispositionColumns are the output names of dispositionsSQL, in order (its first arm).
var dispositionColumns = func() []string {
	meta := dispositionsSQL[:strings.Index(dispositionsSQL, " UNION ALL")]
	var names []string
	for _, piece := range strings.Split(meta, " AS ")[1:] {
		names = append(names, strings.TrimRight(strings.Fields(piece)[0], ","))
	}
	return names
}()

// DispositionsExit is cmd_dispositions_show's PayloadExit: an unreadable store refuses (exit 2)
// with the whole answer.
type DispositionsExit struct{ Payload contract.OrderedObject }

func (e *DispositionsExit) Error() string { return "the store is unreadable" }

// ExitPayload prints the whole answer with exit 2.
func (e *DispositionsExit) ExitPayload() (contract.OrderedObject, int) {
	return e.Payload, contract.ExitRefused
}

// ReadDispositions is dispositions.read. Exactly one of project and relationship is set.
func ReadDispositions(ctx context.Context, selection store.StateSelection, project, relationship *string) contract.OrderedObject {
	return readDispositions(ctx, selection, project, relationship, store.ReadOnlyRows)
}

func optionalArg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func readDispositions(ctx context.Context, selection store.StateSelection, project, relationship *string, read rowsReader) contract.OrderedObject {
	selector := contract.OrderedObject{{Key: "relationshipId", Value: optionalArg(relationship)}}
	if project != nil {
		selector = contract.OrderedObject{{Key: "projectKey", Value: *project}}
	}
	var rows []dispRow
	answer := read(ctx, selection, dispositionsSQL, []any{optionalArg(project), optionalArg(project), optionalArg(relationship), optionalArg(relationship)},
		func(scanner store.RowScanner) error {
			values := make([]any, len(dispositionColumns))
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := scanner.Scan(pointers...); err != nil {
				return err
			}
			row := dispRow{}
			for i, name := range dispositionColumns {
				if b, ok := values[i].([]byte); ok {
					values[i] = string(b)
				}
				row[name] = values[i]
			}
			rows = append(rows, row)
			return nil
		})
	blank := func(detail string) contract.OrderedObject {
		return contract.OrderedObject{{Key: "selector", Value: selector}, {Key: "readable", Value: false}, {Key: "detail", Value: detail},
			{Key: "store", Value: contract.OrderedObject{{Key: "storeId", Value: nil}, {Key: "dbPath", Value: selection.DBPath()},
				{Key: "device", Value: nil}, {Key: "inode", Value: nil}, {Key: "links", Value: nil}}},
			{Key: "children", Value: []any{}}, {Key: "counts", Value: dispositionCounts(nil)}, {Key: "limits", Value: DispositionReadingLimits}}
	}
	if !answer.Readable {
		detail := answer.Detail
		if detail == "" {
			detail = UnopenedDetail
		}
		return blank(detail)
	}
	if answer.Detail != "" {
		return blank(answer.Detail)
	}
	var meta dispRow
	for _, row := range rows {
		if row["kind"] == "meta" {
			meta = row
			break
		}
	}
	if meta == nil {
		return blank(NoIdentityDetail)
	}
	derived := deriveDispositions(rows, selector)
	return append(derived, contract.Field{Key: "store", Value: contract.OrderedObject{{Key: "storeId", Value: meta["store_id"]},
		{Key: "dbPath", Value: selection.DBPath()}, {Key: "device", Value: int64(answer.Device)}, {Key: "inode", Value: int64(answer.Inode)},
		{Key: "links", Value: int64(answer.Links)}}})
}

type dispChild struct {
	record      contract.OrderedObject
	eventIDs    []any
	events      []contract.OrderedObject
	disposition contract.OrderedObject
	correction  contract.OrderedObject
}

func zeroIfNil(v any) any {
	if v == nil {
		return int64(0)
	}
	return v
}

// deriveDispositions is dispositions.derive: pure over the flat rows.
func deriveDispositions(rows []dispRow, selector contract.OrderedObject) contract.OrderedObject {
	children := map[string]*dispChild{}
	for _, row := range rows {
		if row["kind"] != "child" {
			continue
		}
		rid := row.s("relationship_id")
		c := children[rid]
		if c == nil {
			c = &dispChild{correction: dispositionCorrection(row)}
			c.record = contract.OrderedObject{
				{Key: "relationshipId", Value: row["relationship_id"]}, {Key: "issueKey", Value: row["issue_key"]},
				{Key: "projectKey", Value: row["project_key"]}, {Key: "relationshipStatus", Value: row["relationship_status"]},
				{Key: "parentTaskId", Value: row["parent_task_id"]}, {Key: "childTaskId", Value: row["child_task_id"]},
				{Key: "executionGeneration", Value: row["execution_generation"]},
			}
			c.record = append(c.record, contract.Field{Key: "reviewableCount", Value: zeroIfNil(row["reviewable_count"])},
				contract.Field{Key: "earlierGenerationEvents", Value: zeroIfNil(row["earlier_events"])})
			children[rid] = c
		}
		if row["event_id"] == nil {
			continue
		}
		c.events = append(c.events, dispositionEvent(row))
		if row["outcome"] == reviewable && row["stage"] == "final" && !truthyValue(row["suppressed_reason"]) {
			c.eventIDs = append(c.eventIDs, row["event_id"])
		}
	}
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]contract.OrderedObject, 0, len(keys))
	for _, k := range keys {
		c := children[k]
		c.disposition = turnDisposition(c.events)
		reviewableCount, _ := getField(c.record, "reviewableCount")
		earlier, _ := getField(c.record, "earlierGenerationEvents")
		rid, _ := getField(c.record, "relationshipId")
		base := c.record[:7]
		ids := c.eventIDs
		if ids == nil {
			ids = []any{}
		}
		events := make([]any, len(c.events))
		for i, e := range c.events {
			events[i] = e
		}
		out := append(contract.OrderedObject{}, base...)
		out = append(out,
			contract.Field{Key: "turnDisposition", Value: c.disposition},
			contract.Field{Key: "reviewable", Value: contract.OrderedObject{{Key: "eventIds", Value: ids}, {Key: "eventsInGeneration", Value: reviewableCount},
				{Key: "head", Value: "not_derived_here"}, {Key: "readWith", Value: "assignment-show --relationship " + pyText(rid)}}},
			contract.Field{Key: "earlierGenerationEvents", Value: earlier},
			contract.Field{Key: "correction", Value: objectOrNil(c.correction)},
			contract.Field{Key: "events", Value: events})
		ordered = append(ordered, out)
	}
	list := make([]any, len(ordered))
	for i, c := range ordered {
		list[i] = c
	}
	return contract.OrderedObject{{Key: "selector", Value: selector}, {Key: "readable", Value: true}, {Key: "detail", Value: nil},
		{Key: "children", Value: list}, {Key: "counts", Value: dispositionCounts(ordered)}, {Key: "limits", Value: DispositionReadingLimits}}
}

// pyText is str(value).
func pyText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

func dispositionEvent(row dispRow) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "eventId", Value: row["event_id"]}, {Key: "outcome", Value: row["outcome"]}, {Key: "producer", Value: row["producer"]},
		{Key: "stage", Value: row["stage"]}, {Key: "suppressedReason", Value: row["suppressed_reason"]}, {Key: "turnId", Value: row["turn_id"]},
		{Key: "attempt", Value: row["attempt"]}, {Key: "becameFinalAt", Value: row["became_final_at"]}, {Key: "orderingAt", Value: row["ordering_at"]},
		{Key: "workReport", Value: workReport(row)}, {Key: "delivery", Value: dispositionDelivery(row)}, {Key: "acknowledgement", Value: acknowledgement(row)},
	}
}

func dispositionCorrection(row dispRow) contract.OrderedObject {
	if row["correction_event"] == nil {
		return nil
	}
	state := row["correction_state"]
	superseded := row["correction_supersession_reason"] != nil
	var delivery any
	if state != nil {
		observation := obsSuperseded
		if !superseded {
			observation = observationByState[row.s("correction_state")]
			if observation == "" {
				observation = obsUnrecognised
			}
		}
		var supersession any
		if superseded {
			supersession = contract.OrderedObject{{Key: "reason", Value: row["correction_supersession_reason"]},
				{Key: "applied", Value: truthyValue(row["correction_supersession_applied"])}}
		}
		delivery = contract.OrderedObject{{Key: "observation", Value: observation}, {Key: "state", Value: state},
			{Key: "attemptCount", Value: row["correction_attempts"]}, {Key: "holdReason", Value: row["correction_hold"]},
			{Key: "nextEligibleAt", Value: row["correction_next_eligible_at"]}, {Key: "supersession", Value: supersession}}
	}
	reason := undeliveredReason(reasonRow{delivered: state != nil, state: row.s("correction_state"), hold: row.s("correction_hold"),
		relationshipStatus: row["relationship_status"], supersededBy: row["superseded_by"],
		lifecycleWithhold: row["correction_lifecycle_withhold"], lifecycleAt: row["correction_lifecycle_recorded_at"],
		lifecycleRetry: row["correction_lifecycle_next_retry_at"]})
	return contract.OrderedObject{{Key: "eventId", Value: row["correction_event"]}, {Key: "delivery", Value: delivery},
		{Key: "undeliveredReason", Value: objectOrNil(reason)}}
}

func workReport(row dispRow) contract.OrderedObject {
	if row["report_submission"] == nil {
		return contract.OrderedObject{{Key: "recorded", Value: false}, {Key: "submissionNo", Value: nil}, {Key: "cxcStatus", Value: nil},
			{Key: "cxcReason", Value: nil}, {Key: "nextAction", Value: nil}}
	}
	return contract.OrderedObject{{Key: "recorded", Value: true}, {Key: "submissionNo", Value: row["report_submission"]},
		{Key: "cxcStatus", Value: row["cxc_status"]}, {Key: "cxcReason", Value: row["cxc_reason"]}, {Key: "nextAction", Value: row["next_action"]}}
}

func dispositionDelivery(row dispRow) contract.OrderedObject {
	observation, detail := sendObservation(row)
	recipient, recipientDetail := recipientObservation(row)
	intent := contract.OrderedObject{{Key: "recorded", Value: false}, {Key: "attempts", Value: nil}, {Key: "lastError", Value: nil}, {Key: "nextRetryAt", Value: nil}}
	if row["intent_event"] != nil {
		intent = contract.OrderedObject{{Key: "recorded", Value: true}, {Key: "attempts", Value: row["intent_attempts"]},
			{Key: "lastError", Value: row["intent_error"]}, {Key: "nextRetryAt", Value: row["intent_next_retry_at"]}}
	}
	var supersession, current any
	if row["supersession_reason"] != nil {
		supersession = contract.OrderedObject{{Key: "reason", Value: row["supersession_reason"]}, {Key: "applied", Value: truthyValue(row["supersession_applied"])}}
	}
	if row["request_id"] != nil {
		current = contract.OrderedObject{{Key: "requestId", Value: row["request_id"]}, {Key: "attemptNo", Value: row["attempt_no"]},
			{Key: "internalState", Value: row["attempt_internal_state"]}, {Key: "state", Value: row["attempt_state"]}, {Key: "sentAt", Value: row["attempt_sent_at"]}}
	}
	return contract.OrderedObject{
		{Key: "observation", Value: observation}, {Key: "recipientObservation", Value: recipient},
		{Key: "state", Value: row["delivery_state"]}, {Key: "kind", Value: row["delivery_kind"]},
		{Key: "recipientTaskId", Value: row["recipient_task_id"]}, {Key: "attemptCount", Value: row["attempt_count"]},
		{Key: "holdReason", Value: row["hold_reason"]}, {Key: "dispatchTurnId", Value: row["dispatch_turn_id"]},
		{Key: "dispatchEvidence", Value: row["dispatch_evidence"] != nil}, {Key: "detail", Value: detail},
		{Key: "recipientDetail", Value: recipientDetail}, {Key: "readWith", Value: "status --relationship " + pyText(row["relationship_id"])},
		{Key: "intent", Value: intent}, {Key: "supersession", Value: supersession}, {Key: "currentAttempt", Value: current},
	}
}

// sendObservation is dispositions._observation.
func sendObservation(row dispRow) (string, any) {
	if truthyValue(row["suppressed_reason"]) {
		return obsSuppressed, "the claim was suppressed, so there is no delivery obligation to measure: " + pyText(row["suppressed_reason"])
	}
	if row["stage"] != "final" {
		return "not_deliverable:" + pyText(row["stage"]), "only a final event may be delivered, so an event at stage " + pyRepr(row["stage"]) +
			" has no delivery to measure and is never delivery evidence"
	}
	if row["delivery_event"] != nil {
		if row["supersession_reason"] != nil {
			return obsSuperseded, observationDetail[obsSuperseded]
		}
		observation, known := observationByState[row.s("delivery_state")]
		if !known || row["delivery_state"] == nil {
			return obsUnrecognised, "the delivery records state " + pyRepr(row["delivery_state"]) + ", which this reader has no word for; read it with status"
		}
		return observation, observationDetail[observation]
	}
	if row["intent_event"] != nil {
		return obsRefusedPreQueue, RefusedPreQueueDetail
	}
	var present []string
	for _, p := range []struct {
		name string
		ok   bool
	}{{"an acknowledgement", row["ack_event"] != nil}, {"acknowledgement evidence", row["ack_evidence_event"] != nil},
		{"a supersession note", row["supersession_reason"] != nil}} {
		if p.ok {
			present = append(present, p.name)
		}
	}
	if len(present) > 0 {
		return obsDisagree, "this event has no delivery row, yet the store holds " + strings.Join(present, ", ") +
			" for it, so no single answer about its delivery is supportable"
	}
	return obsUnmeasured, UnmeasuredDetail
}

// recipientObservation is dispositions._recipient_observation.
func recipientObservation(row dispRow) (string, any) {
	if row["ack_event"] != nil {
		if row["ack_tier"] == "host_read" {
			return obsHostRead, nil
		}
		return obsClaimed, ClaimedDetail
	}
	if row["ack_evidence_event"] != nil {
		return obsDisagree, EvidenceWithoutAckDetail
	}
	return obsUnmeasured, RecipientUnmeasuredDetail
}

func acknowledgement(row dispRow) contract.OrderedObject {
	present := row["ack_event"] != nil
	when := func(v any) any {
		if !present {
			return nil
		}
		return v
	}
	tier := row["ack_tier"]
	if tier == nil {
		tier = "unrecorded"
	}
	return contract.OrderedObject{{Key: "recorded", Value: present}, {Key: "accepted", Value: when(truthyValue(row["ack_accepted"]))},
		{Key: "rejectionReason", Value: when(row["ack_rejection"])}, {Key: "settlement", Value: when(row["ack_verified"])}, {Key: "evidenceTier", Value: tier}}
}

func eventField(e contract.OrderedObject, key string) any { v, _ := getField(e, key); return v }

// turnDisposition is dispositions._turn_disposition.
func turnDisposition(events []contract.OrderedObject) contract.OrderedObject {
	var candidates []contract.OrderedObject
	for _, e := range events {
		outcome, _ := eventField(e, "outcome").(string)
		if contains(executionOnly, outcome) && eventField(e, "stage") == "final" && !truthyValue(eventField(e, "suppressedReason")) {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return contract.OrderedObject{{Key: "outcome", Value: nil}, {Key: "eventId", Value: nil}, {Key: "basis", Value: "none"},
			{Key: "candidates", Value: []any{}}, {Key: "detail", Value: NoDispositionDetail}}
	}
	var ids []string
	outcomes := map[string]bool{}
	for _, e := range candidates {
		ids = append(ids, pyText(eventField(e, "eventId")))
		outcomes[eventField(e, "outcome").(string)] = true
	}
	sort.Strings(ids)
	if len(outcomes) > 1 {
		var words []string
		for w := range outcomes {
			words = append(words, w)
		}
		sort.Strings(words)
		return contract.OrderedObject{{Key: "outcome", Value: nil}, {Key: "eventId", Value: nil}, {Key: "basis", Value: "contested"},
			{Key: "candidates", Value: anyStrings(ids)}, {Key: "detail", Value: "this generation holds " + itoa(len(outcomes)) +
				" disagreeing final dispositions (" + strings.Join(words, ", ") + "), and choosing between them would be a guess"}}
	}
	key := func(e contract.OrderedObject) [2]string {
		ordering, _ := eventField(e, "orderingAt").(string)
		return [2]string{ordering, pyText(eventField(e, "eventId"))}
	}
	newest := candidates[0]
	for _, e := range candidates[1:] {
		k, n := key(e), key(newest)
		if k[0] > n[0] || (k[0] == n[0] && k[1] > n[1]) {
			newest = e
		}
	}
	if len(candidates) == 1 {
		return contract.OrderedObject{{Key: "outcome", Value: eventField(newest, "outcome")}, {Key: "eventId", Value: eventField(newest, "eventId")},
			{Key: "basis", Value: "sole"}, {Key: "candidates", Value: anyStrings(ids)}, {Key: "detail", Value: nil}}
	}
	return contract.OrderedObject{{Key: "outcome", Value: eventField(newest, "outcome")}, {Key: "eventId", Value: eventField(newest, "eventId")},
		{Key: "basis", Value: "latest_of_same_outcome"}, {Key: "candidates", Value: anyStrings(ids)}, {Key: "detail", Value: SameOutcomeDetail}}
}

// dispositionCounts is dispositions._counts: counted from the list it summarises.
func dispositionCounts(children []contract.OrderedObject) contract.OrderedObject {
	var withDisposition, blocked, contested, sendUnmeasured, recipientUnmeasured, missing, notSent, withheld int64
	for _, c := range children {
		disposition, _ := getField(c, "turnDisposition")
		d := disposition.(contract.OrderedObject)
		if eventField(d, "basis") != "none" {
			withDisposition++
		}
		if eventField(d, "outcome") == "blocked_needs_input" {
			blocked++
		}
		if eventField(d, "basis") == "contested" {
			contested++
		}
		events, _ := getField(c, "events")
		for _, raw := range events.([]any) {
			e := raw.(contract.OrderedObject)
			delivery := eventField(e, "delivery").(contract.OrderedObject)
			if eventField(delivery, "observation") == obsUnmeasured {
				sendUnmeasured++
			}
			if eventField(delivery, "recipientObservation") == obsUnmeasured {
				recipientUnmeasured++
			}
			outcome, _ := eventField(e, "outcome").(string)
			if contains(executionOnly, outcome) && eventField(eventField(e, "workReport").(contract.OrderedObject), "recorded") == false {
				missing++
			}
		}
		correction, _ := getField(c, "correction")
		if co, ok := correction.(contract.OrderedObject); ok {
			if delivery, ok := eventField(co, "delivery").(contract.OrderedObject); ok {
				if eventField(delivery, "observation") == obsNotSent {
					notSent++
				}
				if eventField(delivery, "state") == "withheld_pre_send" && eventField(delivery, "supersession") == nil {
					withheld++
				}
			}
		}
	}
	return contract.OrderedObject{{Key: "children", Value: int64(len(children))}, {Key: "withExecutionOnlyDisposition", Value: withDisposition},
		{Key: "blocked", Value: blocked}, {Key: "contested", Value: contested}, {Key: "deliveryUnmeasured", Value: sendUnmeasured},
		{Key: "recipientUnmeasured", Value: recipientUnmeasured}, {Key: "workReportMissing", Value: missing},
		{Key: "correctionNotSent", Value: notSent}, {Key: "correctionWithheld", Value: withheld}}
}
