package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The Go twins of tests/test_host_lost_turn.py's HostLossCase (and test_unknown_send_lost.py's
// UnknownSendCase and the other modules mirror names). Each twin performs its Python test's steps
// and records the value it produced at each of that test's assertions (the first argument of
// assertEqual, the expression of assertTrue, ...); the list, the delivery tables row for row and
// the host's sends are checked against the golden, which began as what the Python test asserted
// (testdata/capture.py, removed with the Python implementation). So every asserted value, reason,
// state and next-action word is a checked answer, never a constant of the test.

// hl is one mirrored test: the fixture in its tree, the fake host, the services, the daemon's
// in-memory state, and the capture list.
type hl struct {
	*fixture
	name    string
	ack     *Ack
	rc      *Reconciler
	checks  *TurnChecks
	adapter Adapter
	got     []any
	policy  tickPolicy
}

// tickPolicy is the part of RetryPolicy the daemon's tick reads.
type tickPolicy struct{ maxTurnChecks, maxSendsTick, maxReconciles int }

func defaultTick() tickPolicy { return tickPolicy{4, 4, 8} }

// mirror runs body as the Go twin of module's Class.method name, in the tree that test ran in, and
// checks every value it recorded (eq), the delivery and fault tables and the host's sends against
// the golden, under "<module>/<name> captures", "... delivery tables", "... fault tables" and
// "... sends".
func mirror(t *testing.T, module, name string, body func(h *hl)) {
	t.Helper()
	root, tree := captureTree(t, module, name)
	f := newFixture(t, tree)
	f.rid = ""
	h := &hl{fixture: f, name: name, ack: NewAck(f.delivery), rc: NewReconciler(f.delivery), adapter: f.host, policy: defaultTick()}
	h.checks = &TurnChecks{Reconciler: h.rc, Budget: 4}
	body(h)
	key := module + "/" + name
	opts := []golden.Option{golden.Substitute(tree, "<tree>"), golden.Substitute(root, "<root>"), golden.Substitute(installationDir(), "<installation>")}
	golden.CheckJSON(t, key+" captures", capturedValues(t, h.got), opts...)
	golden.CheckJSON(t, key+" delivery tables", deliveryTableRows(t, f), opts...)
	golden.CheckJSON(t, key+" fault tables", stalledFaultRows(t, f), opts...)
	golden.CheckJSON(t, key+" sends", normalizeJSON(t, sendsJSON(f.host)), opts...)
}

// captureTree is the tree module's Class.method name runs in, and the root of every tree of
// module: fixed paths, because the golden values hold ids derived from them (lockCaptureTree),
// the tree held for this test only.
func captureTree(t *testing.T, module, name string) (string, string) {
	t.Helper()
	root, tree, release, err := lockCaptureTree(module, name)
	mustDo(t, err)
	t.Cleanup(release)
	return root, tree
}

var storeAcceptNone = store.AcceptOptions{}

// capturedValues are the values a mirror recorded, normalized as JSON, with the Go store's
// directory (gostate) spelled as the Python store's (state), as the twin test named it: the two
// stores sat side by side in one tree.
func capturedValues(t *testing.T, got []any) any {
	t.Helper()
	g := normalizeJSON(t, jsonable(got))
	if g == nil {
		return []any{}
	}
	values := g.([]any)
	for i := range values {
		values[i] = sameStore(values[i])
	}
	return values
}

// sameStore maps the Go store's directory (gostate) onto the Python store's (state) in a captured
// string: a recovery command names its own store.
func sameStore(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, string(filepath.Separator)+"gostate", string(filepath.Separator)+"state")
	case []any:
		for i := range x {
			x[i] = sameStore(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = sameStore(x[k])
		}
	}
	return v
}

// deliveryTables are the tables the delivery, reconciliation and host-loss paths write. The
// daemon's observation pass (poll_observations, its cursors) is todo 29's and is not mirrored.
var deliveryTables = []string{"acks", "ack_evidence", "attempt_messages", "attempts", "deliveries", "events", "failed_operations", "generations", "journal", "reconcile_gate", "recipient_rate", "verdicts"}

// deliveryTableRows are the rows of deliveryTables, normalized as JSON (an empty table is null).
// A reconcile gate's fingerprint is shown without its receipt turn id, as the Python gate had
// none; the Go-only regression tests verify that component.
func deliveryTableRows(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	got := normalizeJSON(t, f.tables()).(map[string]any)
	out := map[string]any{}
	for _, n := range deliveryTables {
		if n == "reconcile_gate" && got[n] != nil {
			for _, value := range got[n].([]any) {
				row := value.(map[string]any)
				if fingerprint, ok := row["fingerprint"].(string); ok {
					status, tail, found := strings.Cut(fingerprint, "|")
					if found {
						_, content, found := strings.Cut(tail, "|")
						if found {
							row["fingerprint"] = status + "|" + content
						}
					}
				}
			}
		}
		out[n] = got[n]
	}
	return out
}

// eq records the value the twin Python test's assertEqual/assertNotEqual/assertIsNone/assertTrue
// asserted.
func (h *hl) eq(values ...any) {
	if len(values) == 1 {
		h.got = append(h.got, values[0])
		return
	}
	h.got = append(h.got, values)
}

// ---------------------------------------------------------------- HostLossCase

func (h *hl) parentHistory() {
	h.host.startTurn(parent, "parent-earlier", "completed", "")
	h.clock.Advance(300)
}

func (h *hl) attemptOn(event string, adapter Adapter, now *float64) Obj {
	h.t.Helper()
	record, err := h.delivery.Attempt(h.ctx, event, adapter, now, "")
	mustDo(h.t, err)
	return record
}

func (h *hl) dispatched() (string, string, string) {
	h.parentHistory()
	event := h.queuedEvent(regOpts{})
	record := h.attemptOn(event, h.host, nil)
	h.eq(str(record, "deliveryState"))
	return event, str(record, "requestId"), str(record, "turnId")
}

// completions is HostLossCase.completions: count delivered completions to one parent.
func (h *hl) completions(count int) [][3]string {
	h.parentHistory()
	var delivered [][3]string
	for i := 0; i < count; i++ {
		rid := h.register(regOpts{issue: fmt.Sprintf("REL-%d", i+2), dispatchRequest: fmt.Sprintf("dispatch-%d", i+2)})
		h.rid = rid
		payload := h.readyPayload(rid, 1, []string{h.artifact(fmt.Sprintf("out-%d.txt", i), fmt.Sprintf("deliverable %d", i))}, 1, assigned("completed"))
		_, err := h.accept(payload, storeAcceptNone)
		mustDo(h.t, err)
		_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
		mustDo(h.t, err)
		record := h.attemptOn(str(payload, "eventId"), h.host, nil)
		h.eq(str(record, "deliveryState"))
		delivered = append(delivered, [3]string{str(payload, "eventId"), str(record, "requestId"), str(record, "turnId")})
		h.clock.Advance(10)
	}
	slices.SortFunc(delivered, func(a, b [3]string) int { return strings.Compare(a[0]+a[1]+a[2], b[0]+b[1]+b[2]) })
	return delivered
}

func (h *hl) acknowledge(event, turn string) {
	t := h.host.startTurn(parent, turn, "inProgress", "")
	_, err := h.ack.Acknowledge(h.ctx, event, t.TurnID, AckProof(event, t.TurnID), true, nil, h.host)
	mustDo(h.t, err)
}

func (h *hl) reconcile(request string, adapter Adapter) Obj {
	h.t.Helper()
	out, err := h.rc.ReconcileAttempt(h.ctx, request, adapter, nil)
	mustDo(h.t, err)
	return out
}

func (h *hl) tokenConfirmed() (string, string, string) {
	h.parentHistory()
	event := h.queuedEvent(regOpts{})
	h.host.script = []string{"in_progress"}
	request := str(h.attemptOn(event, h.host, nil), "requestId")
	turn := h.host.startTurn(parent, "", "completed", "..."+request+"...")
	confirmed := h.reconcile(request, h.host)
	h.eq(str(confirmed, "evidence"))
	h.eq(h.row(event).S("state"))
	return event, request, turn.TurnID
}

func (h *hl) evidenceOf(event string) []any {
	var out []any
	for _, r := range h.attemptsFor(event) {
		out = append(out, r.Opt("affirmative_evidence"))
	}
	return out
}

func (h *hl) attemptsFor(event string) []Row {
	rows, err := all(h.ctx, h.store, "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no", event)
	mustDo(h.t, err)
	return rows
}

func (h *hl) attemptStates(event string) []any {
	out := []any{}
	for _, r := range h.attemptsFor(event) {
		out = append(out, r.Opt("state"))
	}
	return out
}

func (h *hl) hostReloadsLosing(turn string) {
	h.host.finishTurn(parent, turn, "interrupted")
	h.dropItems(turn)
}

func (h *hl) dropItems(turn string) {
	t := h.host.threads[parent]
	kept := t.items[:0:0]
	for _, item := range t.items {
		if item[0] != turn {
			kept = append(kept, item)
		}
	}
	t.items = kept
}

func (h *hl) hostLoses(turn string, items bool) {
	t := h.host.threads[parent]
	kept := t.turns[:0:0]
	for _, one := range t.turns {
		if one.TurnID != turn {
			kept = append(kept, one)
		}
	}
	t.turns = kept
	if items {
		h.dropItems(turn)
	}
}

func (h *hl) echo(turn, text string) {
	h.host.threads[parent].items = append(h.host.threads[parent].items, [3]string{turn, text, "commandExecution"})
}

func (h *hl) item(thread, turn, text, kind string) {
	h.host.threads[thread].items = append(h.host.threads[thread].items, [3]string{turn, text, kind})
}

func (h *hl) messageOf(turn string) string {
	for _, item := range h.host.threads[parent].items {
		if item[0] == turn {
			return item[1]
		}
	}
	return ""
}

func (h *hl) uncertainDelivery(message bool) (string, string, string) {
	h.parentHistory()
	event := h.queuedEvent(regOpts{})
	h.host.script = []string{"in_progress"}
	request := str(h.attemptOn(event, h.host, nil), "requestId")
	h.eq(h.row(event).S("state"))
	h.clock.Advance(2)
	text := "another prompt"
	if message {
		text = "[codex-session-relay] verification request\nrequestId: " + request
	}
	turn := h.host.startTurn(parent, "", "inProgress", text)
	return event, request, turn.TurnID
}

func (h *hl) foldedDelivery(message bool, earlier, later int) (string, string) {
	h.parentHistory()
	event := h.queuedEvent(regOpts{})
	h.host.startTurn(parent, "folded", "inProgress", "")
	for n := 0; n < earlier; n++ {
		h.item(parent, "folded", fmt.Sprintf("earlier work %d", n), "commandExecution")
	}
	h.clock.Advance(120)
	h.host.script = []string{"in_progress"}
	request := str(h.attemptOn(event, h.host, nil), "requestId")
	h.eq(h.row(event).S("state"))
	if message {
		h.item(parent, "folded", "[codex-session-relay] verification request\nrequestId: "+request, "")
	}
	for n := 0; n < later; n++ {
		h.item(parent, "folded", fmt.Sprintf("later work %d", n), "commandExecution")
	}
	return event, request
}

func (h *hl) cliAck(event, turn string, adapter Adapter) Obj {
	h.t.Helper()
	if adapter == nil {
		adapter = h.host
	}
	out, err := AckCommand(h.ctx, h.ack, h.rc, adapter, event, turn, AckProof(event, turn), nil)
	mustDo(h.t, err)
	return out
}

func (h *hl) cliVerdict(event, turn string) Obj {
	h.t.Helper()
	mustDo(h.t, CompleteKeptAcknowledgement(h.ctx, h.ack, h.rc, h.host, event))
	out, err := h.ack.RecordVerdict(h.ctx, event, "verified", turn, nil, nil, nil, nil)
	mustDo(h.t, err)
	return out
}

func (h *hl) ackRow(event string) Row {
	return h.one("SELECT a.verified, e.last_reason FROM acks a LEFT JOIN ack_evidence e ON e.event_id = a.event_id WHERE a.event_id = ?", event)
}

func (h *hl) statusOf(event string) Obj {
	h.t.Helper()
	item, err := h.delivery.SnapshotItem(h.ctx, event)
	mustDo(h.t, err)
	return item
}

func (h *hl) statusPair(event string) []any {
	item := h.statusOf(event)
	p, _ := get(item, "phase")
	r, _ := get(item, "reported")
	return []any{p, r}
}

func (h *hl) journalled(kind string) int64 {
	return h.count("SELECT COUNT(*) AS c FROM journal WHERE kind = ?", kind)
}

func (h *hl) exec(query string, args ...any) {
	h.t.Helper()
	mustDo(h.t, h.store.Transaction(h.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, h.store, query, args...)
		return err
	}))
}

func field(o Obj, key string) any {
	v, _ := get(o, key)
	return v
}

func sub(o Obj, key string) Obj {
	v, _ := get(o, key)
	s, _ := v.(Obj)
	return s
}

// ---------------------------------------------------------------- the tick

// tickReport is daemon.TickReport for the passes this package owns; the others stay 0.
type tickReport struct {
	reconciled, delivered, deferred, skipped, acksVerified, anchorsBound, turnsLost, turnsUndecided int
	notes                                                                                           []string
}

func (r tickReport) quiet() bool {
	return r.reconciled+r.delivered+r.deferred+r.acksVerified+r.anchorsBound+r.turnsLost+r.turnsUndecided == 0
}

func (r tickReport) asDict() Obj {
	return Obj{{Key: "observed", Value: 0}, {Key: "reconciled", Value: r.reconciled}, {Key: "delivered", Value: r.delivered}, {Key: "deferred", Value: r.deferred},
		{Key: "skipped", Value: r.skipped}, {Key: "acksVerified", Value: r.acksVerified}, {Key: "anchorsBound", Value: r.anchorsBound}, {Key: "requeued", Value: 0},
		{Key: "faultsRecorded", Value: 0}, {Key: "supervisorStaged", Value: 0}, {Key: "supervisorSent", Value: 0}, {Key: "notificationsDelivered", Value: 0},
		{Key: "turnsLost", Value: r.turnsLost}, {Key: "turnsUndecided", Value: r.turnsUndecided}, {Key: "quiet", Value: r.quiet()}}
}

// tick is RelayDaemon.tick over the passes this package ports, in its order: bind anchors,
// reconcile, bind again, verify acknowledgements (kept ones confirmed first), check delivered
// turns, deliver. Observation of child turns, requeue, the fault sweep and the supervisor pass
// (todo 29, 22, 24) have nothing to do in these scenarios and are not run.
func (h *hl) tickWith(policy tickPolicy, adapter Adapter, checks *TurnChecks) tickReport {
	h.t.Helper()
	now := h.clock.Now()
	var r tickReport
	bind := func() {
		bound, err := h.ack.BindPendingAnchors(h.ctx)
		if err != nil {
			r.notes = append(r.notes, "anchor recovery failed: "+err.Error())
			return
		}
		r.anchorsBound += len(bound)
	}
	bind()
	var rr ReconcileReport
	mustDo(h.t, ReconcilePass(h.ctx, h.rc, adapter, policy.maxReconciles, now, &rr))
	r.reconciled, r.skipped = rr.Reconciled, rr.Skipped
	bind()
	r.notes = append(r.notes, ConfirmKeptAcks(h.ctx, h.ack, h.rc, adapter, now)...)
	results, err := h.ack.VerifyPendingAcks(h.ctx, adapter, 8, &now)
	mustDo(h.t, err)
	for _, one := range results {
		if str(one.(Obj), "outcome") == "verified" {
			r.acksVerified++
		}
	}
	checks.Budget = policy.maxTurnChecks
	var tc TurnCheckReport
	checks.Pass(h.ctx, adapter, now, &tc)
	r.turnsLost, r.turnsUndecided = tc.TurnsLost, tc.TurnsUndecided
	sends := policy.maxSendsTick
	if sends == 0 {
		sends = -1
	}
	var counts TickCounts
	mustDo(h.t, (&Scheduler{Delivery: h.delivery, Ack: h.ack, MaxSendsTick: sends}).Deliver(h.ctx, adapter, now, &counts))
	r.delivered, r.deferred, r.skipped = counts.Delivered, counts.Deferred, r.skipped+counts.Skipped
	return r
}

func (h *hl) tick() tickReport { return h.tickWith(h.policy, h.adapter, h.checks) }

// ---------------------------------------------------------------- assignment-show

// nextAction is AssignmentView.state(rid)["nextExpectedAction"] for the states these tests
// reach, read over the same projection statement (Anchored): correction_next_action, then
// completion_next_action, then NEXT_ACTION. The assignment view itself is todo 25's
// (registry.CompletionNextAction on crw-154); this reads what delivery owns.
func (h *hl) nextAction() string { return str(h.assignment(), "nextExpectedAction") }

func (h *hl) assignment() Obj {
	h.t.Helper()
	r, err := LoadRelationship(h.ctx, h.store, h.rid)
	mustDo(h.t, err)
	head, err := HeadRevision(h.ctx, h.store, h.rid, r.Generation)
	mustDo(h.t, err)
	headID, _ := get(head, "eventId")
	var verdict Row
	if headID != nil {
		verdict = h.one("SELECT verdict FROM verdicts WHERE event_id = ?", headID)
	}
	previous := h.one("SELECT v.event_id FROM verdicts v JOIN events e ON e.event_id = v.event_id WHERE e.relationship_id = ? AND e.execution_generation < ? AND v.verdict = 'needs_changes' ORDER BY e.execution_generation DESC LIMIT 1", h.rid, r.Generation)
	state := "requested"
	switch {
	case verdict != nil && verdict.S("verdict") == "verified":
		state = "verified"
	case headID != nil && previous != nil:
		state = "corrected"
	case headID != nil && h.one("SELECT 1 FROM verification_claims WHERE event_id = ?", headID) != nil:
		state = "verifying"
	case headID != nil:
		state = "received"
	case previous != nil:
		state = "needs_changes"
	}
	projection := h.projection(r.Generation, headID)
	action := correctionNextAction(state, projection)
	if action == "" {
		action = completionNextAction(state, projection)
	}
	if action == "" {
		action = map[string]string{"requested": "child_emits", "received": "daemon_delivers", "verifying": "parent_verifies", "needs_changes": "child_corrects", "corrected": "parent_verifies", "verified": "coordinator_integrates"}[state]
	}
	out := Obj{{Key: "state", Value: state}, {Key: "nextExpectedAction", Value: action}, {Key: "projection", Value: projection}}
	if recovery := h.parentRecovery(action, projection); recovery != nil {
		out = append(out, F{Key: "recovery", Value: recovery})
	}
	return out
}

func (h *hl) projection(generation int64, headID any) Obj {
	anchored := func(event any) Obj {
		out, err := Anchored(h.ctx, h.delivery, event, generation)
		if err != nil && strings.Contains(err.Error(), "settings-hold reading") {
			// withheld_pre_send / inbox_only: the settings-hold reading is todo 25's; these
			// scenarios stage no settings refusal, so Python reads no settings hold either.
			return h.anchoredWithoutSettings(event, generation)
		}
		mustDo(h.t, err)
		return out
	}
	correction := h.one("SELECT event_id FROM events WHERE relationship_id = ? AND execution_generation = ? AND outcome = 'revision_request' AND suppressed_reason IS NULL ORDER BY event_id LIMIT 1", h.rid, generation)
	var correctionID any
	if correction != nil {
		correctionID = correction.S("event_id")
	}
	return Obj{{Key: "completion", Value: anchored(headID)}, {Key: "correction", Value: anchored(correctionID)}}
}

func (h *hl) anchoredWithoutSettings(event any, generation int64) Obj {
	row := h.one("SELECT d.state, d.hold_reason, (SELECT COUNT(*) FROM attempts x WHERE x.event_id = d.event_id AND x.state = 'host_lost_turn') AS lost, k.event_id AS acked, k.verified, k.accepted, v.last_reason FROM deliveries d LEFT JOIN acks k ON k.event_id = d.event_id LEFT JOIN ack_evidence v ON v.event_id = d.event_id WHERE d.event_id = ?", event)
	delivery := Obj{{Key: "state", Value: row.S("state")}, {Key: "holdReason", Value: row.Opt("hold_reason")}, {Key: "hostLostAttempts", Value: row.I("lost")}, {Key: "pacing", Value: nil}, {Key: "settingsHold", Value: nil}}
	ack := Obj{{Key: "settlement", Value: nil}}
	if !row.N("acked") {
		ack = Obj{{Key: "accepted", Value: row.I("accepted") != 0}, {Key: "settlement", Value: row.Opt("verified")}, {Key: "lastReason", Value: row.Opt("last_reason")}}
	}
	var reason any
	if row.S("hold_reason") != "" {
		reason = Obj{{Key: "source", Value: "deliveries.hold_reason"}, {Key: "value", Value: row.S("hold_reason")}}
	}
	return Obj{{Key: "eventId", Value: event}, {Key: "executionGeneration", Value: generation}, {Key: "delivery", Value: delivery}, {Key: "ack", Value: ack}, {Key: "undeliveredReason", Value: reason}, {Key: "supersession", Value: nil}}
}

func neverReopens(delivery Obj) bool {
	p := sub(delivery, "pacing")
	return p != nil && str(p, "reason") == HourlyCap && field(p, "reopensAt") == nil
}

func correctionNextAction(state string, projection Obj) string {
	correction := sub(projection, "correction")
	delivery := sub(correction, "delivery")
	if state != "needs_changes" || delivery == nil {
		return ""
	}
	dstate := str(delivery, "state")
	if field(correction, "supersession") != nil || dstate == Superseded {
		return correctionAnswered
	}
	if dstate == Dispatched || dstate == Acknowledged {
		return ""
	}
	if dstate == InboxOnly || str(sub(correction, "undeliveredReason"), "source") == "deliveries.hold_reason" {
		return correctionHeld
	}
	if dstate == Sending || dstate == HeldUncertain {
		return correctionUnconfirmed
	}
	if slices.Contains(claimable, dstate) {
		if neverReopens(delivery) {
			return "operator_changes_send_policy"
		}
		return "daemon_delivers_correction"
	}
	return ""
}

func completionNextAction(state string, projection Obj) string {
	if state != "received" && state != "corrected" && state != "verifying" {
		return ""
	}
	completion := sub(projection, "completion")
	delivery := sub(completion, "delivery")
	if delivery == nil {
		return ""
	}
	ack := sub(completion, "ack")
	lost := field(delivery, "hostLostAttempts").(int64) > 0
	settlement := field(ack, "settlement")
	if settlement == "verified" {
		if state == "received" && field(ack, "accepted") == true {
			return "parent_verifies"
		}
		return ""
	}
	if hold := str(delivery, "holdReason"); hold != "" && hold != PushChannelClosed {
		switch hold {
		case HostLostTurn:
			return "parent_recovers_host_lost_turn"
		case UnknownSendLost:
			return unknownSendHeldAction
		case UnknownSendUndecided:
			return unknownSendUndecidedAction
		}
		if lost {
			return "parent_recovers_host_lost_turn"
		}
		return ""
	}
	switch str(delivery, "state") {
	case HeldUncertain, Sending:
		return reconcileAction
	case Dispatched, InboxOnly:
		if settlement != nil {
			switch field(ack, "lastReason") {
			case nil, "unverified_turn", DeliveryUnconfirmed:
				return "daemon_verifies_acknowledgement"
			}
			return "parent_reacknowledges"
		}
		return "parent_acknowledges"
	case Queued, DeferredBusy, WithheldPreSend:
		if neverReopens(delivery) {
			return "operator_changes_send_policy"
		}
		if lost {
			return "daemon_redelivers_host_lost_turn"
		}
		return "daemon_delivers"
	}
	return ""
}

func (h *hl) parentRecovery(action string, projection Obj) Obj {
	var anchored Obj
	switch action {
	case correctionHeld:
		anchored = sub(projection, "correction")
	case "parent_recovers_host_lost_turn", unknownSendHeldAction, unknownSendUndecidedAction:
		anchored = sub(projection, "completion")
	default:
		return nil
	}
	delivery := sub(anchored, "delivery")
	reason := field(delivery, "holdReason")
	if !truthy(reason) {
		reason = field(delivery, "state")
	}
	directory, _ := filepath.Abs(filepath.Dir(h.store.Path))
	return Obj{{Key: "actor", Value: "parent"}, {Key: "reason", Value: reason}, {Key: "command", Value: recoveryCommand(directory, pyvalue.Str(field(anchored, "eventId")))}, {Key: "then", Value: parentRecoveryThen}}
}

func (h *hl) completionDelivery() Obj {
	return sub(sub(sub(h.assignment(), "projection"), "completion"), "delivery")
}

// ---------------------------------------------------------------- wrapped adapters

// hooked is a fake host with some reads or the send replaced, everything else passed through
// (the Python tests' delegating wrappers).
type hooked struct {
	Adapter
	findDispatched func(thread, turn string, sentAt float64) (TurnPresence, error)
	findToken      func(thread, token string, limit int, messageOnly bool) (TokenScan, error)
	readTurn       func(thread, turn string) (*TurnInfo, error)
	send           func(requestID, thread, message string, settings *TaskSettings) (Obj, error)
	getOperation   func(requestID string) (Obj, error)
	findInTurn     func(thread, token, turnID string, limit int) (TokenScan, error)
}

func (w *hooked) FindDispatchedTurn(thread, turn string, sentAt float64) (TurnPresence, error) {
	if w.findDispatched != nil {
		return w.findDispatched(thread, turn, sentAt)
	}
	return w.Adapter.FindDispatchedTurn(thread, turn, sentAt)
}

func (w *hooked) FindToken(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
	if w.findToken != nil {
		return w.findToken(thread, token, limit, messageOnly)
	}
	return w.Adapter.FindToken(thread, token, limit, messageOnly)
}

func (w *hooked) ReadTurn(thread, turn string) (*TurnInfo, error) {
	if w.readTurn != nil {
		return w.readTurn(thread, turn)
	}
	return w.Adapter.ReadTurn(thread, turn)
}

func (w *hooked) SendMessage(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
	if w.send != nil {
		return w.send(requestID, thread, message, settings)
	}
	return w.Adapter.SendMessage(requestID, thread, message, settings)
}

func (w *hooked) GetOperation(requestID string) (Obj, error) {
	if w.getOperation != nil {
		return w.getOperation(requestID)
	}
	return w.Adapter.GetOperation(requestID)
}

func (w *hooked) FindTokenInTurn(thread, token, turnID string, limit int) (TokenScan, error) {
	if w.findInTurn != nil {
		return w.findInTurn(thread, token, turnID, limit)
	}
	return w.Adapter.FindTokenInTurn(thread, token, turnID, limit)
}

// countingLookups is CountingLookups: every recipient-turn lookup, by turn id.
func countingLookups(inner Adapter, lookups *[]string) *hooked {
	return &hooked{Adapter: inner, findDispatched: func(thread, turn string, sentAt float64) (TurnPresence, error) {
		*lookups = append(*lookups, turn)
		return inner.FindDispatchedTurn(thread, turn, sentAt)
	}}
}

// countingReads is CountingReads: the receipt and item reads a confirmation could make.
func countingReads(inner Adapter, reads *[]any) *hooked {
	return &hooked{Adapter: inner,
		getOperation: func(id string) (Obj, error) {
			*reads = append(*reads, []any{"get_operation", id})
			return inner.GetOperation(id)
		},
		findToken: func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
			*reads = append(*reads, []any{"find_token", token})
			return inner.FindToken(thread, token, limit, messageOnly)
		},
		findInTurn: func(thread, token, turnID string, limit int) (TokenScan, error) {
			*reads = append(*reads, []any{"find_token_in_turn", turnID})
			return inner.FindTokenInTurn(thread, token, turnID, limit)
		}}
}

// actsDuringTheTurnRead is ActsDuringTheTurnRead: something else runs first, once.
func actsDuringTheTurnRead(inner Adapter, action func()) *hooked {
	return &hooked{Adapter: inner, readTurn: func(thread, turn string) (*TurnInfo, error) {
		if action != nil {
			a := action
			action = nil
			a()
		}
		return inner.ReadTurn(thread, turn)
	}}
}

func preSendRejection(request string) Obj {
	return Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "failed"}, {Key: "error", Value: "thread/read: transport refused"}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "internal"}, {Key: "message", Value: "refused"}}}}
}

// stalledFaultRows are the delivery_stalled rows of the fault tables, without their seq, normalized
// as JSON; the other classes come from sources todo 22 and todo 29 own (observation_stalled needs
// the daemon's poll rows), and fault_cursors carries the wall clock.
func stalledFaultRows(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	return stalledRows(normalizeJSON(t, f.tables()).(map[string]any))
}

func stalledRows(tables map[string]any) map[string]any {
	ids := map[string]bool{}
	rows, _ := tables["fault_ledger"].([]any)
	for _, r := range rows {
		if m := r.(map[string]any); m["fault_class"] == "delivery_stalled" {
			ids[m["fault_id"].(string)] = true
		}
	}
	out := map[string]any{}
	for _, name := range []string{"fault_ledger", "fault_occurrences", "fault_timeline", "fault_publications", "fault_notifications"} {
		kept := []any{}
		rows, _ := tables[name].([]any)
		for _, r := range rows {
			m := r.(map[string]any)
			if ids[fmt.Sprint(m["fault_id"])] {
				delete(m, "seq")
				kept = append(kept, m)
			}
		}
		out[name] = kept
	}
	return out
}
