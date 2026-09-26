package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// assignment.py: one issue, one responsible child, and where that assignment actually stands.
// Derived from the records, except the one fact the relay cannot observe (a merge mark).

// Assignment states (assignment.py).
const (
	StateRequested    = "requested"
	StateReceived     = "received"
	StateVerifying    = "verifying"
	StateNeedsChanges = "needs_changes"
	StateCorrected    = "corrected"
	StateVerified     = "verified"
	StateMerged       = "merged"
	StatePaused       = "paused"
	StateAmbiguous    = "ambiguous"
	StateRereview     = "re_review_needed"
	StateClosed       = "closed"
	StateAbandoned    = "abandoned"
)

// nextAction is assignment.NEXT_ACTION.
var nextAction = map[string]string{
	StateRequested: "child_emits", StateReceived: "daemon_delivers", StateVerifying: "parent_verifies",
	StateNeedsChanges: "child_corrects", StateCorrected: "parent_verifies", StateVerified: "coordinator_integrates",
	StateMerged: "none", StatePaused: "owner_resumes", StateAmbiguous: "child_declares_supersession",
	StateRereview: "parent_verifies", StateClosed: "none", StateAbandoned: "none",
}

// LifecycleWithholdSource is assignment.LIFECYCLE_WITHHOLD_SOURCE.
const LifecycleWithholdSource = "failed_operations.lifecycle_read"

// ParentRecoveryThen is assignment.PARENT_RECOVERY_THEN.
const ParentRecoveryThen = "read the report, then open a fresh execution generation" +
	" (generation-open) if the work still needs verifying"

// lifecycleWithholdJoin is assignment.LIFECYCLE_WITHHOLD_JOIN for the given aliases.
func lifecycleWithholdJoin(alias, event, delivery string) string {
	r := strings.NewReplacer("{alias}", alias, "{event}", event, "{delivery}", delivery)
	return r.Replace(" LEFT JOIN failed_operations {alias} ON {alias}.scope_key = {event}.event_id" +
		"       AND {alias}.operation = 'lifecycle_read'" +
		"       AND {alias}.occurred_at = {delivery}.updated_at" +
		"       AND {alias}.next_retry_at = {delivery}.next_eligible_at" +
		"       AND NOT EXISTS (SELECT 1 FROM failed_operations other" +
		"                        WHERE other.scope_key = {event}.event_id" +
		"                          AND other.operation <> {alias}.operation" +
		"                          AND other.occurred_at = {alias}.occurred_at)")
}

// settingsHoldColumns is delivery.SETTINGS_HOLD_COLUMNS for an event expression and delivery alias.
func settingsHoldColumns(event, delivery string) string {
	latest := func(tag string) string {
		return "(SELECT " + tag + ".request_id FROM attempts " + tag + " WHERE " + tag + ".event_id = " + event +
			" AND " + tag + ".internal_state = 'settled' ORDER BY " + tag + ".attempt_no DESC LIMIT 1)"
	}
	return delivery + ".state AS sh_state, " + delivery + ".hold_reason AS sh_hold_reason," +
		" " + latest("sha") + " AS sh_request," +
		" (SELECT json_object('seq', shj.seq, 'detail', shj.detail) FROM journal shj" +
		"   WHERE shj.subject = " + event + " AND +shj.kind = 'delivery_attempted'" +
		"     AND (CASE WHEN json_valid(shj.detail)" +
		"          THEN json_extract(shj.detail, '$.requestId') END)" +
		"         = " + latest("shb") +
		"   ORDER BY shj.seq DESC LIMIT 1) AS sh_settled_sent," +
		" (SELECT json_object('seq', shr.seq, 'detail', shr.detail) FROM journal shr" +
		"   WHERE shr.subject = " + latest("shc") +
		"     AND +shr.kind = 'reconciled'" +
		"   ORDER BY shr.seq DESC LIMIT 1) AS sh_settled_reconciled," +
		" (SELECT json_object('seq', shp.seq, 'detail', shp.detail) FROM journal shp" +
		"   WHERE shp.subject = " + event + " AND +shp.kind = 'delivery_presend_withheld'" +
		"   ORDER BY shp.seq DESC LIMIT 1) AS sh_presend," +
		" (SELECT shf.occurred_at FROM failed_operations shf WHERE shf.scope_key = " + event +
		"   AND shf.operation = 'settings_check') AS sh_settings_at," +
		" (SELECT shl.occurred_at FROM failed_operations shl WHERE shl.scope_key = " + event +
		"   AND shl.operation = 'lifecycle_read') AS sh_lifecycle_at," +
		" (SELECT shi.at FROM journal shi WHERE shi.subject = " + event +
		"   AND +shi.kind = 'delivery_withheld_inactive' ORDER BY shi.seq DESC LIMIT 1)" +
		"   AS sh_inactive_at"
}

// Delivery states the projection distinguishes (transport.py).
var (
	notSentStates     = []string{"queued", "deferred_busy", "withheld_pre_send"}
	undecidedChecks   = []string{"turn_check_undecided:", "unknown_send_lost:", "unknown_send_undecided:"}
	parentRecoveryAll = []string{ActionHostLostHeld, ActionUnknownSendHeld, ActionUnknownSendUndecided, ActionParentRecoversSettings}
)

// RetryPolicy is the part of policy.RetryPolicy the assignment view reads: the send budget.
type RetryPolicy struct {
	MinSendIntervalSeconds float64
	MaxSendsPerHour        int64
}

// DefaultRetryPolicy is RetryPolicy().
var DefaultRetryPolicy = RetryPolicy{MinSendIntervalSeconds: 5, MaxSendsPerHour: 12}

const rateWindowSeconds = 3600

// rateWindows is RetryPolicy.rate_windows.
func (p RetryPolicy) rateWindows(now float64) (int64, int64) {
	window := int64(math.Floor(now/rateWindowSeconds)) * rateWindowSeconds
	reach := int64(rateWindowSeconds) * (1 + int64(math.Floor(p.MinSendIntervalSeconds/rateWindowSeconds)))
	return window, window - reach
}

// pacing is RetryPolicy.pacing; nil is None.
func (p RetryPolicy) pacing(now float64, sends any, last any) contract.OrderedObject {
	window, _ := p.rateWindows(now)
	count, _ := toFloat(sends)
	reading := contract.OrderedObject{{Key: "sends", Value: numberValue(sends, 0)}, {Key: "cap", Value: p.MaxSendsPerHour}, {Key: "windowStart", Value: window}}
	var gapEnds *float64
	if l, ok := toFloat(last); ok && last != nil {
		g := l + p.MinSendIntervalSeconds
		gapEnds = &g
	}
	if count >= float64(p.MaxSendsPerHour) {
		if p.MaxSendsPerHour <= 0 {
			return append(append(contract.OrderedObject{{Key: "reason", Value: "hourly_cap"}, {Key: "reopensAt", Value: nil}}, reading...),
				contract.Field{Key: "detail", Value: "a cap of zero refuses every send; only a changed policy reopens it"})
		}
		var reopens any = window + rateWindowSeconds
		if gapEnds != nil && *gapEnds > float64(window+rateWindowSeconds) {
			reopens = *gapEnds
		}
		return append(contract.OrderedObject{{Key: "reason", Value: "hourly_cap"}, {Key: "reopensAt", Value: reopens}}, reading...)
	}
	if gapEnds != nil && now < *gapEnds {
		return append(contract.OrderedObject{{Key: "reason", Value: "min_send_interval"}, {Key: "reopensAt", Value: *gapEnds}}, reading...)
	}
	return nil
}

// pacingHolding is policy.pacing_holding.
func pacingHolding(pacing contract.OrderedObject, nextEligible any) any {
	if pacing == nil {
		return nil
	}
	reopens, _ := getField(pacing, "reopensAt")
	r, rok := toFloat(reopens)
	n, nok := toFloat(nextEligible)
	if reopens == nil || nextEligible == nil || !rok || !nok || n <= r {
		return pacing
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func numberValue(v any, fallback int64) any {
	if v == nil {
		return fallback
	}
	return v
}

// AssignmentView is assignment.AssignmentView over one registry.
type AssignmentView struct {
	R *Registry
	// Clock is clock.now() in seconds; the system clock when nil.
	Clock  func() float64
	Policy RetryPolicy
	// Program is supervisorchannel.relay_program(); RelayProgram when nil.
	Program func() []string
	// statement, when set, sees every single-row read this view makes (the one-snapshot tests).
	statement func(query string)
}

// NewAssignmentView is AssignmentView(store, registry, clock) with the default budget.
func NewAssignmentView(r *Registry) *AssignmentView {
	return &AssignmentView{R: r, Policy: DefaultRetryPolicy}
}

func (v *AssignmentView) now() float64 {
	if v.Clock == nil {
		return float64(time.Now().UnixMicro()) / 1e6
	}
	return v.Clock()
}

func (v *AssignmentView) s() *store.Store { return v.R.Store }

func (v *AssignmentView) one(ctx context.Context, query string, args ...any) (store.Row, error) {
	if v.statement != nil {
		v.statement(query)
	}
	return v.s().One(ctx, query, args...)
}

// State is AssignmentView.state.
func (v *AssignmentView) State(ctx context.Context, rid string) (contract.OrderedObject, error) {
	relationship, err := v.R.Get(ctx, rid)
	if err != nil {
		return nil, err
	}
	generation := relationship.Generation
	head, err := HeadRevision(ctx, v.s(), rid, generation)
	if err != nil {
		return nil, err
	}
	var verdict contract.OrderedObject
	if head.EventID != "" {
		if verdict, err = v.verdictFor(ctx, head.EventID); err != nil {
			return nil, err
		}
	}
	registered, digest, err := v.criteria(ctx, rid)
	if err != nil {
		return nil, err
	}
	current := criteriaCurrent(verdict, digest)
	marks, err := v.marks(ctx, rid)
	if err != nil {
		return nil, err
	}
	markAt := -1
	if current {
		markAt = currentMark(marks, head, generation, verdict)
	}
	var mark any
	history := []any{}
	for i, m := range marks {
		if i == markAt {
			mark = m
		} else {
			history = append(history, m)
		}
	}
	state, err := v.resolve(ctx, relationship, head, verdict, markAt >= 0, marks, markAt, current)
	if err != nil {
		return nil, err
	}
	mode, err := v.mode(ctx, rid)
	if err != nil {
		return nil, err
	}
	var reviewed any
	if verdict != nil {
		reviewed, _ = getField(verdict, "setDigest")
	}
	projection, err := v.projection(ctx, rid, generation, head, verdict, state)
	if err != nil {
		return nil, err
	}
	decoded := toMap(projection)
	action := CorrectionNextAction(state, decoded)
	if action == "" {
		action = CompletionNextAction(state, decoded)
	}
	if action == "" {
		action = nextAction[state]
		if action == "" {
			action = "none"
		}
	}
	record := contract.OrderedObject{
		{Key: "relationshipId", Value: rid},
		{Key: "issueKey", Value: relationship.IssueKey},
		{Key: "parentTaskId", Value: relationship.Parent.TaskID},
		{Key: "childTaskId", Value: relationship.Child.TaskID},
		{Key: "relationshipStatus", Value: relationship.Status},
		{Key: "state", Value: state},
		{Key: "executionGeneration", Value: generation},
		{Key: "head", Value: head.record()},
		{Key: "lastVerdict", Value: objectOrNil(verdict)},
		{Key: "mark", Value: mark},
		{Key: "markHistory", Value: history},
		{Key: "nextExpectedAction", Value: action},
		{Key: "criteria", Value: contract.OrderedObject{{Key: "mode", Value: mode}, {Key: "setDigest", Value: digest},
			{Key: "registered", Value: registered}, {Key: "reviewedSetDigest", Value: reviewed}, {Key: "current", Value: current}}},
		{Key: "projection", Value: projection},
	}
	directory := filepath.Dir(absolutePath(v.s().Path))
	recovery := v.parentRecovery(action, decoded, directory)
	if recovery == nil {
		recipient, anchor := relationship.Parent.TaskID, "completion"
		if state == StateNeedsChanges {
			recipient, anchor = relationship.Child.TaskID, "correction"
		}
		recovery = v.settingsHoldRecoveryFor(action, decoded, directory, recipient, anchor)
	}
	if recovery != nil {
		record = append(record, contract.Field{Key: "recovery", Value: recovery})
	}
	return record, nil
}

func objectOrNil(o contract.OrderedObject) any {
	if o == nil {
		return nil
	}
	return o
}

// toMap decodes an ordered answer into plain maps, as the next-action rules read it.
func toMap(o contract.OrderedObject) map[string]any {
	var b strings.Builder
	if err := contract.Emit(&b, o); err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(b.String()), &out) != nil {
		return nil
	}
	return out
}

func (v *AssignmentView) projection(ctx context.Context, rid string, generation int64, head Head, verdict contract.OrderedObject, state string) (contract.OrderedObject, error) {
	correction, err := v.one(ctx, "SELECT event_id FROM events"+
		" WHERE relationship_id = ? AND execution_generation = ?"+
		"   AND outcome = 'revision_request' AND suppressed_reason IS NULL"+
		" ORDER BY event_id LIMIT 1", rid, generation)
	if err != nil {
		return nil, err
	}
	completion, err := v.Anchored(ctx, nullText(head.EventID), generation)
	if err != nil {
		return nil, err
	}
	var correctionID any
	if correction != nil {
		correctionID = correction.Get("event_id")
	}
	corrected, err := v.Anchored(ctx, correctionID, generation)
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{{Key: "completion", Value: completion}, {Key: "correction", Value: corrected},
		{Key: "verdict", Value: objectOrNil(verdict)}, {Key: "assignment", Value: contract.OrderedObject{{Key: "state", Value: state}}}}, nil
}

// Anchored is AssignmentView._anchored: one event's whole lifecycle, read in ONE statement.
func (v *AssignmentView) Anchored(ctx context.Context, eventID any, generation int64) (contract.OrderedObject, error) {
	record := contract.OrderedObject{{Key: "eventId", Value: eventID}, {Key: "executionGeneration", Value: generation},
		{Key: "event", Value: nil}, {Key: "delivery", Value: nil}, {Key: "ack", Value: nil}, {Key: "undeliveredReason", Value: nil},
		{Key: "supersession", Value: nil}}
	if eventID == nil {
		return append(record, contract.Field{Key: "detail", Value: "this generation has no such event"}), nil
	}
	now := v.now()
	window, earliest := v.Policy.rateWindows(now)
	row, err := v.one(ctx, "SELECT e.stage AS stage,"+
		"       d.event_id AS delivered, d.state AS delivery_state,"+
		"       d.hold_reason AS hold_reason, d.dispatch_evidence AS dispatch_evidence,"+
		"       d.next_eligible_at AS next_eligible_at,"+
		"       a.request_id AS request_id, a.attempt_no AS attempt_no,"+
		"       a.state AS attempt_state, a.recipient_scan AS attempt_turn_check,"+
		"       v.last_reason AS ack_last_reason,"+
		"       (SELECT COUNT(*) FROM attempts h WHERE h.event_id = e.event_id"+
		"         AND h.state = 'host_lost_turn') AS host_lost_attempts,"+
		"       (SELECT sends FROM recipient_rate WHERE recipient_task_id = d.recipient_task_id"+
		"         AND window_start = ?) AS rate_sends,"+
		"       (SELECT MAX(last_send_at) FROM recipient_rate"+
		"         WHERE recipient_task_id = d.recipient_task_id"+
		"           AND window_start BETWEEN ? AND ?) AS rate_last,"+
		"       k.event_id AS acked, k.verified AS ack_verified,"+
		"       k.accepted AS ack_accepted, k.rejection_reason AS ack_rejection,"+
		"       v.tier AS ack_tier,"+
		"       r.status AS relationship_status, r.superseded_by AS superseded_by,"+
		"       lf.error_code AS lifecycle_withhold,"+
		"       lf.occurred_at AS lifecycle_recorded_at,"+
		"       lf.next_retry_at AS lifecycle_next_retry_at,"+
		"       sx.reason AS supersession_reason, sx.applied AS supersession_applied,"+
		"       (SELECT reason FROM refusals WHERE event_id = e.event_id"+
		"         ORDER BY id DESC LIMIT 1) AS refusal_reason,"+
		settingsHoldColumns("e.event_id", "d")+
		"  FROM events e"+
		"  LEFT JOIN deliveries d ON d.event_id = e.event_id"+
		"  LEFT JOIN attempts a ON a.event_id = e.event_id"+
		"                      AND a.attempt_no = d.attempt_count"+
		"  LEFT JOIN acks k ON k.event_id = e.event_id"+
		"  LEFT JOIN ack_evidence v ON v.event_id = e.event_id"+
		"  LEFT JOIN delivery_supersession sx ON sx.event_id = e.event_id"+
		"  LEFT JOIN relationships r ON r.relationship_id = e.relationship_id"+
		lifecycleWithholdJoin("lf", "e", "d")+
		" WHERE e.event_id = ?", window, earliest, window, eventID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return append(record, contract.Field{Key: "detail", Value: "the store holds no such event"}), nil
	}
	record = setField(record, "event", contract.OrderedObject{{Key: "stage", Value: row.Get("stage")}})
	state := colString(row, "delivery_state")
	if row.Get("delivered") != nil {
		var turnCheck any
		check := colString(row, "attempt_turn_check")
		for _, prefix := range undecidedChecks {
			if strings.HasPrefix(check, prefix) {
				turnCheck = check
			}
		}
		var pacing any
		if contains(notSentStates, state) && !truthyValue(row.Get("hold_reason")) {
			pacing = pacingHolding(v.Policy.pacing(now, row.Get("rate_sends"), row.Get("rate_last")), row.Get("next_eligible_at"))
		}
		reading := SettingsHoldReading(holdColumnsOf(row))
		var settingsHold any
		if hold, _ := getField(reading, "hold"); hold != nil {
			kind, _ := getField(reading, "kind")
			settingsHold = setField(copyObject(hold.(contract.OrderedObject)), "kind", kind)
		}
		record = setField(record, "delivery", contract.OrderedObject{
			{Key: "state", Value: row.Get("delivery_state")},
			{Key: "requestId", Value: row.Get("request_id")},
			{Key: "attemptNo", Value: row.Get("attempt_no")},
			{Key: "attemptState", Value: row.Get("attempt_state")},
			{Key: "dispatchEvidence", Value: row.Get("dispatch_evidence")},
			{Key: "holdReason", Value: row.Get("hold_reason")},
			{Key: "hostLostAttempts", Value: row.Get("host_lost_attempts")},
			{Key: "turnCheck", Value: turnCheck},
			{Key: "pacing", Value: pacing},
			{Key: "settingsHold", Value: settingsHold},
		})
	}
	acked := row.Get("acked") != nil
	ifAcked := func(value any) any {
		if !acked {
			return nil
		}
		return value
	}
	tier := row.Get("ack_tier")
	if tier == nil {
		tier = "unrecorded"
	}
	record = setField(record, "ack", contract.OrderedObject{
		{Key: "accepted", Value: ifAcked(truthyValue(row.Get("ack_accepted")))},
		{Key: "rejectionReason", Value: ifAcked(row.Get("ack_rejection"))},
		{Key: "settlement", Value: ifAcked(row.Get("ack_verified"))},
		{Key: "lastReason", Value: ifAcked(row.Get("ack_last_reason"))},
		{Key: "evidenceTier", Value: tier},
	})
	record = setField(record, "undeliveredReason", objectOrNil(undeliveredReason(reasonRow{
		delivered: row.Get("delivered") != nil, state: state, hold: colString(row, "hold_reason"),
		relationshipStatus: row.Get("relationship_status"), supersededBy: row.Get("superseded_by"),
		lifecycleWithhold: row.Get("lifecycle_withhold"), lifecycleAt: row.Get("lifecycle_recorded_at"),
		lifecycleRetry: row.Get("lifecycle_next_retry_at"), refusal: row.Get("refusal_reason"),
	})))
	if row.Get("supersession_reason") != nil {
		record = setField(record, "supersession", contract.OrderedObject{{Key: "reason", Value: row.Get("supersession_reason")},
			{Key: "applied", Value: truthyValue(row.Get("supersession_applied"))}})
	}
	return record, nil
}

func holdColumnsOf(row store.Row) *HoldColumns {
	return &HoldColumns{State: colString(row, "sh_state"), HoldReason: colString(row, "sh_hold_reason"),
		SettledSent: colString(row, "sh_settled_sent"), SettledReconciled: colString(row, "sh_settled_reconciled"),
		Presend: colString(row, "sh_presend"), Request: colString(row, "sh_request"),
		SettingsAt: colString(row, "sh_settings_at"), LifecycleAt: colString(row, "sh_lifecycle_at"), InactiveAt: colString(row, "sh_inactive_at")}
}

// truthyValue is Python bool() of a column value.
func truthyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []byte:
		return len(x) > 0
	}
	return true
}

// reasonRow carries the columns assignment.undelivered_reason reads.
type reasonRow struct {
	delivered                                      bool
	state, hold                                    string
	relationshipStatus, supersededBy               any
	lifecycleWithhold, lifecycleAt, lifecycleRetry any
	refusal                                        any
}

// undeliveredReason is assignment.undelivered_reason; nil is None.
func undeliveredReason(row reasonRow) contract.OrderedObject {
	if row.delivered && row.hold != "" {
		return contract.OrderedObject{{Key: "source", Value: "deliveries.hold_reason"}, {Key: "value", Value: row.hold}}
	}
	if row.delivered && contains(notSentStates, row.state) && row.relationshipStatus != nil &&
		row.relationshipStatus != any("active") && row.supersededBy == nil {
		return contract.OrderedObject{{Key: "source", Value: "relationships.status"},
			{Key: "value", Value: string(contract.RefusalRelationshipNotActive)}, {Key: "relationshipStatus", Value: row.relationshipStatus}}
	}
	if row.delivered && row.state == "withheld_pre_send" && row.lifecycleWithhold != nil {
		return contract.OrderedObject{{Key: "source", Value: LifecycleWithholdSource}, {Key: "value", Value: row.lifecycleWithhold},
			{Key: "recordedAt", Value: row.lifecycleAt}, {Key: "nextRetryAt", Value: row.lifecycleRetry}}
	}
	if row.delivered {
		return nil
	}
	if row.refusal != nil {
		return contract.OrderedObject{{Key: "source", Value: "refusals.reason"}, {Key: "value", Value: row.refusal}}
	}
	return nil
}

func (v *AssignmentView) resolve(ctx context.Context, x Relationship, head Head, verdict contract.OrderedObject, hasMark bool, marks []contract.OrderedObject, markAt int, criteriaCurrent bool) (string, error) {
	switch {
	case x.Status == "cancelled":
		return StateAbandoned, nil
	case x.Status == "archived" || x.SupersededBy.String != "":
		return StateClosed, nil
	case x.Status == "paused":
		return StatePaused, nil
	case head.Ambiguous():
		return StateAmbiguous, nil
	}
	verdictWord := ""
	if verdict != nil {
		w, _ := getField(verdict, "verdict")
		verdictWord, _ = w.(string)
	}
	if verdictWord == StateVerified && !criteriaCurrent {
		return StateRereview, nil
	}
	if hasMark {
		m, _ := getField(marks[markAt], "mark")
		return m.(string), nil
	}
	if verdictWord == StateVerified {
		return StateVerified, nil
	}
	previous, err := v.one(ctx, "SELECT v.event_id FROM verdicts v JOIN events e ON e.event_id = v.event_id"+
		" WHERE e.relationship_id = ? AND e.execution_generation < ?"+
		"   AND v.verdict = 'needs_changes'"+
		" ORDER BY e.execution_generation DESC LIMIT 1", x.ID, x.Generation)
	if err != nil {
		return "", err
	}
	if head.EventID != "" {
		if previous != nil {
			return StateCorrected, nil
		}
		claimed, err := v.one(ctx, "SELECT 1 FROM verification_claims WHERE event_id = ?", head.EventID)
		if err != nil {
			return "", err
		}
		if claimed != nil {
			return StateVerifying, nil
		}
		return StateReceived, nil
	}
	if previous != nil {
		return StateNeedsChanges, nil
	}
	receipt, err := v.one(ctx, "SELECT 1 FROM events WHERE relationship_id = ? AND outcome = 'ready_for_review'", x.ID)
	if err != nil {
		return "", err
	}
	if receipt != nil {
		return StateReceived, nil
	}
	return StateRequested, nil
}

// verdictFor is AssignmentView._verdict_for; nil is None.
func (v *AssignmentView) verdictFor(ctx context.Context, event string) (contract.OrderedObject, error) {
	row, err := v.one(ctx, "SELECT * FROM verdicts WHERE event_id = ?", event)
	if err != nil || row == nil {
		return nil, err
	}
	decoded, err := decodeJSON([]byte(colString(row, "record")))
	if err != nil {
		return nil, &HostError{Class: "JSONDecodeError", Detail: err.Error()}
	}
	record, _ := decoded.(contract.OrderedObject)
	generation, _ := getField(record, "executionGeneration")
	context_, err := v.one(ctx, "SELECT set_digest, coverage FROM verdict_context WHERE event_id = ?", event)
	if err != nil {
		return nil, err
	}
	var digest, coverage any
	if context_ != nil {
		digest, coverage = context_.Get("set_digest"), context_.Get("coverage")
	}
	return contract.OrderedObject{{Key: "verdict", Value: row.Get("verdict")}, {Key: "eventId", Value: event},
		{Key: "executionGeneration", Value: generation}, {Key: "nextExecutionGeneration", Value: row.Get("next_generation")},
		{Key: "decidedAt", Value: row.Get("decided_at")}, {Key: "setDigest", Value: digest}, {Key: "coverage", Value: coverage}}, nil
}

// criteria is CriteriaService.get's count and digest; digest nil when none is registered.
func (v *AssignmentView) criteria(ctx context.Context, rid string) (int64, any, error) {
	rows, err := v.s().All(ctx, "SELECT * FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", rid)
	if err != nil || len(rows) == 0 {
		return 0, nil, err
	}
	return int64(len(rows)), rows[0].Get("set_digest"), nil
}

// mode is CriteriaService.mode.
func (v *AssignmentView) mode(ctx context.Context, rid string) (any, error) {
	row, err := v.one(ctx, "SELECT mode FROM verification_mode WHERE relationship_id = ?", rid)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return "legacy", nil
	}
	return row.Get("mode"), nil
}

func criteriaCurrent(verdict contract.OrderedObject, digest any) bool {
	if verdict == nil {
		return true
	}
	reviewed, _ := getField(verdict, "setDigest")
	return reviewed == digest
}

func (v *AssignmentView) marks(ctx context.Context, rid string) ([]contract.OrderedObject, error) {
	rows, err := v.s().All(ctx, "SELECT * FROM assignment_marks WHERE relationship_id = ? ORDER BY marked_at", rid)
	if err != nil {
		return nil, err
	}
	out := make([]contract.OrderedObject, len(rows))
	for i, row := range rows {
		out[i] = contract.OrderedObject{{Key: "mark", Value: row.Get("mark")}, {Key: "eventId", Value: row.Get("event_id")},
			{Key: "executionGeneration", Value: row.Get("execution_generation")}, {Key: "revisionHash", Value: row.Get("revision_hash")},
			{Key: "evidence", Value: row.Get("evidence")}, {Key: "actor", Value: row.Get("actor")}, {Key: "markedAt", Value: row.Get("marked_at")}}
	}
	return out, nil
}

// currentMark is AssignmentView._current_mark: the index of the mark that is state, or -1.
func currentMark(marks []contract.OrderedObject, head Head, generation int64, verdict contract.OrderedObject) int {
	if head.EventID == "" || verdict == nil {
		return -1
	}
	if w, _ := getField(verdict, "verdict"); w != StateVerified {
		return -1
	}
	for i, m := range marks {
		event, _ := getField(m, "eventId")
		gen, _ := getField(m, "executionGeneration")
		hash, _ := getField(m, "revisionHash")
		if event == head.EventID && gen == generation && hash == head.RevisionHash {
			return i
		}
	}
	return -1
}

// ForIssue is AssignmentView.for_issue: what a coordinator asks BEFORE creating anything.
func (v *AssignmentView) ForIssue(ctx context.Context, issueKey string) (contract.OrderedObject, error) {
	rows, err := v.s().All(ctx, "SELECT relationship_id FROM relationships WHERE issue_key = ? ORDER BY created_at", issueKey)
	if err != nil {
		return nil, err
	}
	assignments := []any{}
	var owning []contract.OrderedObject
	for _, row := range rows {
		state, err := v.State(ctx, colString(row, "relationship_id"))
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, state)
		status, _ := getField(state, "relationshipStatus")
		word, _ := getField(state, "state")
		if (status == "active" || status == "paused") && word != StateClosed {
			owning = append(owning, state)
		}
	}
	var child, relationship any
	if len(owning) > 0 {
		child, _ = getField(owning[0], "childTaskId")
		relationship, _ = getField(owning[0], "relationshipId")
	}
	record := contract.OrderedObject{{Key: "issueKey", Value: issueKey}, {Key: "assignments", Value: assignments},
		{Key: "responsibleChild", Value: child}, {Key: "responsibleRelationship", Value: relationship}}
	record = append(record, v.projectContext(ctx, owning)...)
	provenance, err := v.relayProvenance(ctx, len(owning) > 0)
	if err != nil {
		return nil, err
	}
	return append(record, contract.Field{Key: "relay", Value: provenance}), nil
}

// projectContext is AssignmentView._project_context.
func (v *AssignmentView) projectContext(ctx context.Context, owning []contract.OrderedObject) contract.OrderedObject {
	blank := contract.OrderedObject{{Key: "projectKey", Value: nil}, {Key: "projectParentTaskId", Value: nil}, {Key: "scopeState", Value: "unscoped"}}
	if len(owning) == 0 {
		return blank
	}
	rid, _ := getField(owning[0], "relationshipId")
	scoped, err := v.one(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	if err != nil {
		return contract.OrderedObject{{Key: "projectKey", Value: nil}, {Key: "projectParentTaskId", Value: nil}, {Key: "scopeState", Value: "unreadable"}}
	}
	if scoped == nil {
		return blank
	}
	project := scoped.Get("project_key")
	held, err := v.s().All(ctx, "SELECT task_id FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, binding_id", scopeProject, project)
	if err != nil {
		return contract.OrderedObject{{Key: "projectKey", Value: nil}, {Key: "projectParentTaskId", Value: nil}, {Key: "scopeState", Value: "unreadable"}}
	}
	parentTask, _ := getField(owning[0], "parentTaskId")
	if len(held) > 1 {
		candidates := make([]string, len(held))
		for i, h := range held {
			candidates[i] = colString(h, "task_id")
		}
		sortStrings(candidates)
		return contract.OrderedObject{{Key: "projectKey", Value: project}, {Key: "projectParentTaskId", Value: nil},
			{Key: "scopeState", Value: "ambiguous"}, {Key: "projectParentCandidates", Value: anyStrings(candidates)}, {Key: "parentOwnsProject", Value: nil}}
	}
	var holder any
	if len(held) == 1 {
		holder = held[0].Get("task_id")
	}
	return contract.OrderedObject{{Key: "projectKey", Value: project}, {Key: "projectParentTaskId", Value: holder},
		{Key: "scopeState", Value: "scoped"}, {Key: "parentOwnsProject", Value: holder != nil && holder == parentTask}}
}

func pathIdentity(path string) (uint64, uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), st.Ino, true
}

// relayProvenance is AssignmentView._relay_provenance.
func (v *AssignmentView) relayProvenance(ctx context.Context, holds bool) (contract.OrderedObject, error) {
	bd, bi, bok := pathIdentity(v.s().Path)
	located, err := v.s().Locate(ctx)
	if err != nil {
		return nil, err
	}
	var socket any
	meta, err := v.one(ctx, "SELECT value FROM schema_meta WHERE key = ?", "socket_path")
	if err != nil {
		return nil, err
	}
	if meta != nil {
		socket = meta.Get("value")
	}
	ad, ai, aok := pathIdentity(v.s().Path)
	if !bok || !aok || bd != ad || bi != ai {
		return contract.OrderedObject{{Key: "holds", Value: holds}, {Key: "store", Value: contract.OrderedObject{
			{Key: "identified", Value: false}, {Key: "storeId", Value: nil}, {Key: "dbPath", Value: located.DBPath},
			{Key: "realPath", Value: nil}, {Key: "device", Value: nil}, {Key: "inode", Value: nil}, {Key: "recordedSocket", Value: nil},
			{Key: "detail", Value: "the database at this path was replaced while it was read"}}}}, nil
	}
	var real, device, inode any
	if located.RealPath != "" {
		real, device, inode = located.RealPath, int64(located.Device), int64(located.Inode)
	}
	return contract.OrderedObject{{Key: "holds", Value: holds}, {Key: "store", Value: contract.OrderedObject{
		{Key: "identified", Value: true}, {Key: "storeId", Value: located.StoreID}, {Key: "dbPath", Value: located.DBPath},
		{Key: "realPath", Value: real}, {Key: "device", Value: device}, {Key: "inode", Value: inode},
		{Key: "recordedSocket", Value: socket}, {Key: "detail", Value: nil}}}}, nil
}

// Mark is AssignmentView.mark: the one assignment fact the relay cannot observe for itself,
// decided inside the write transaction against the exact event the caller integrated.
func (v *AssignmentView) Mark(ctx context.Context, rid, mark, evidence, actor, expectedEvent string) (contract.OrderedObject, error) {
	if mark != StateMerged {
		return nil, refuse(contract.RefusalDispositionConflict, "unknown mark %s", pyStr(mark))
	}
	if strings.TrimSpace(evidence) == "" {
		return nil, refuse(contract.RefusalFindingsRequired, "a mark carries its evidence")
	}
	if strings.TrimSpace(expectedEvent) == "" {
		return nil, refuse(contract.RefusalStaleMarkContext, "a mark names the exact event it integrated")
	}
	now := v.R.now()
	err := v.s().Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		x, err := readRow(ctx, v.s(), rid)
		if err != nil {
			return err
		}
		if x == nil {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
		}
		if x.Status != Active || x.SupersededBy.String != "" {
			return refuse(contract.RefusalRelationshipNotActive, "relationship %s is %s", pyStr(rid), pyStr(x.Status))
		}
		generation := x.Generation
		head, err := HeadRevision(ctx, v.s(), rid, generation)
		if err != nil {
			return err
		}
		if head.EventID == "" || head.Ambiguous() {
			why := head.Detail
			if why == "" {
				why = head.Evidence
			}
			return refuse(contract.RefusalRevisionAmbiguous, "generation %d has no single current revision to mark: %s", generation, why)
		}
		if head.EventID != expectedEvent {
			return refuse(contract.RefusalStaleMarkContext, "this mark names %s, but the current revision of generation %d is %s; "+
				"re-read the assignment before recording what was integrated", pyStr(expectedEvent), generation, pyStr(head.EventID))
		}
		verdict, err := v.verdictFor(ctx, head.EventID)
		if err != nil {
			return err
		}
		if word, _ := getField(verdict, "verdict"); verdict == nil || word != StateVerified {
			return refuse(contract.RefusalNotAcknowledged, "the current revision of generation %d is not verified, so it cannot be marked merged", generation)
		}
		_, digest, err := v.criteria(ctx, rid)
		if err != nil {
			return err
		}
		if !criteriaCurrent(verdict, digest) {
			return refuse(contract.RefusalCriteriaSetChanged, "the canonical criteria changed after this revision was verified, so it "+
				"needs re-review before it can be recorded as integrated")
		}
		if _, err := v.s().Querier(ctx).ExecContext(ctx, "INSERT INTO assignment_marks (relationship_id, mark, event_id,"+
			" execution_generation, revision_hash, evidence, actor, marked_at)"+
			" VALUES (?,?,?,?,?,?,?,?)"+
			" ON CONFLICT(relationship_id, mark, event_id) DO UPDATE SET"+
			"   evidence = excluded.evidence, actor = excluded.actor,"+
			"   marked_at = excluded.marked_at", rid, mark, head.EventID, generation, head.RevisionHash, evidence, actor, now); err != nil {
			return err
		}
		return journal(ctx, v.s(), "assignment_marked", rid, contract.OrderedObject{{Key: "mark", Value: mark},
			{Key: "eventId", Value: head.EventID}, {Key: "generation", Value: generation}}, now)
	})
	if err != nil {
		return nil, err
	}
	return v.State(ctx, rid)
}

// ---------------------------------------------------------------- recovery

func (v *AssignmentView) program() []string {
	if v.Program != nil {
		return v.Program()
	}
	return RelayProgram()
}

// RelayProgram is supervisorchannel.relay_program for the Go installation: the
// codex-session-relay link beside the crw binary, else crw itself with its relay subcommand.
func RelayProgram() []string {
	executable, err := os.Executable()
	if err != nil {
		return []string{"codex-session-relay"}
	}
	if real, err := filepath.EvalSymlinks(executable); err == nil {
		executable = real
	}
	script := filepath.Join(filepath.Dir(executable), "codex-session-relay")
	if info, err := os.Stat(script); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return []string{script}
	}
	return []string{executable, "relay"}
}

func absolutePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, path)
}

// shellQuote is shlex.quote.
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
		}
	}
	return value
}

func (v *AssignmentView) command(argv ...string) string {
	all := append(v.program(), argv...)
	quoted := make([]string, len(all))
	for i, one := range all {
		quoted[i] = shellQuote(one)
	}
	return strings.Join(quoted, " ")
}

// settingsRecoveryRecord is assignment.settings_recovery_record.
func (v *AssignmentView) settingsRecoveryRecord(hold map[string]any, event any, recipient string, directory string, revision bool) contract.OrderedObject {
	kind, _ := hold["kind"].(string)
	code, _ := hold["reason"].(string)
	source, _ := hold["source"].(string)
	chosen := SettingsHoldRecovery(kind, code, source, revision)
	chosenCommand, _ := getField(chosen, "command")
	eventText, _ := event.(string)
	command := v.command("--state", directory, "show", "--event", eventText)
	if chosenCommand != ShowEvent && recipient != "" {
		command = v.command("--state", directory, "settings-show", "--task", recipient)
	}
	var reason any = "undetermined"
	if code != "" {
		reason = code
	}
	actor, _ := getField(chosen, "actor")
	then, _ := getField(chosen, "then")
	later, _ := getField(chosen, "laterDeliveries")
	return contract.OrderedObject{{Key: "actor", Value: actor}, {Key: "reason", Value: reason}, {Key: "command", Value: command},
		{Key: "then", Value: then}, {Key: "laterDeliveries", Value: later}, {Key: "refusalDetail", Value: hold["detail"]}}
}

// parentRecovery is assignment.parent_recovery; nil is None.
func (v *AssignmentView) parentRecovery(action string, projection map[string]any, directory string) contract.OrderedObject {
	var anchored map[string]any
	switch {
	case action == ActionCorrectionHeld:
		anchored = obj(projection["correction"])
	case contains(parentRecoveryAll, action):
		anchored = obj(projection["completion"])
	default:
		return nil
	}
	delivery := obj(anchored["delivery"])
	if action == ActionParentRecoversSettings {
		return v.settingsHoldRecoveryFor(action, projection, directory, "", "completion")
	}
	held := obj(delivery["settingsHold"])
	if action == ActionCorrectionHeld && (held["kind"] == "capped" || held["kind"] == "channel_closed") {
		return v.settingsRecoveryRecord(held, anchored["eventId"], "", directory, true)
	}
	reason := delivery["holdReason"]
	if !truthyValue(reason) {
		reason = delivery["state"]
	}
	event, _ := anchored["eventId"].(string)
	return contract.OrderedObject{{Key: "actor", Value: "parent"}, {Key: "reason", Value: reason},
		{Key: "command", Value: v.command("--state", directory, "show", "--event", event)}, {Key: "then", Value: ParentRecoveryThen}}
}

// settingsHoldRecoveryFor is assignment.settings_hold_recovery_for; nil is None.
func (v *AssignmentView) settingsHoldRecoveryFor(action string, projection map[string]any, directory, recipient, anchor string) contract.OrderedObject {
	anchored := obj(projection[anchor])
	delivery := obj(anchored["delivery"])
	hold := obj(delivery["settingsHold"])
	if len(hold) == 0 {
		return nil
	}
	actions := []string{ActionOperatorRestoresSettings, ActionParentRecoversSettings, ActionAwaitingAck}
	daemons := []string{nextAction[StateReceived], ActionHostLostRedelivery, ActionCorrectionUnsent}
	source, _ := hold["source"].(string)
	reason, _ := hold["reason"].(string)
	daemonClears := false
	if contains(daemons, action) && hold["kind"] == "withheld" && (source == "attempt" || source == "pre_send") {
		actor, _ := getField(SettingsHoldRecovery("withheld", reason, source, false), "actor")
		daemonClears = actor == "daemon"
	}
	if !contains(actions, action) && !daemonClears && source != "undetermined" {
		return nil
	}
	if action == ActionAwaitingAck && hold["kind"] != "channel_closed" {
		return nil
	}
	return v.settingsRecoveryRecord(hold, anchored["eventId"], recipient, directory, anchor == "correction")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
