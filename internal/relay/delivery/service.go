package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Journal kinds of a pre-send transition and an accepted settings note (CRW-235).
const (
	PresendWithheld = "delivery_presend_withheld"
	SettingsNoted   = "delivery_settings_noted"
)

var claimable = []string{Queued, DeferredBusy, WithheldPreSend}

var executionOnlyOutcomes = []string{"failed", "interrupted", "blocked_needs_input"}

// LinkageReader is the linkage `up` reading delivery verifies its recipient against. Nil means
// the service copies the relationship row, as every caller written before the linkage does.
type LinkageReader interface {
	Up(ctx context.Context, relationshipID string) (Obj, error)
}

// Service is delivery.DeliveryService: one path to a transport call, through one atomic claim.
type Service struct {
	Store                    *store.Store
	Clock                    Clock
	Policy                   RetryPolicy
	RequireLifecycleEvidence bool
	Linkage                  LinkageReader
	RoleGate                 RoleGate
	// RateLimited replaces the preflight when set (tests blind it to reach the claim's check).
	RateLimited func(recipient string, now float64) bool
}

// NewService builds a service with Python's defaults.
func NewService(s *store.Store, clock Clock) *Service {
	return &Service{Store: s, Clock: clock, Policy: DefaultPolicy(), RequireLifecycleEvidence: true}
}

// Get is DeliveryService.get.
func (d *Service) Get(ctx context.Context, eventID string) (Row, error) {
	row, err := d.Find(ctx, eventID)
	if err == nil && row == nil {
		err = refuse(NotClaimable, "no delivery queued for event %s", store.PyRepr(eventID))
	}
	return row, err
}

// Find is DeliveryService.find.
func (d *Service) Find(ctx context.Context, eventID string) (Row, error) {
	return one(ctx, d.Store, "SELECT * FROM deliveries WHERE event_id = ?", eventID)
}

func (d *Service) eventRow(ctx context.Context, eventID string) (Row, error) {
	return one(ctx, d.Store, "SELECT * FROM events WHERE event_id = ?", eventID)
}

// Receipt is intake.get: the stored receipt record, or nil.
func (d *Service) Receipt(ctx context.Context, eventID string) (Obj, error) {
	row, err := d.eventRow(ctx, eventID)
	if err != nil || row == nil {
		return nil, err
	}
	return loadsObj(row.S("receipt")), nil
}

func manifestPaths(event Row) []string {
	if event == nil {
		return nil
	}
	receipt := loadsObj(event.S("receipt"))
	entries, _ := get(receipt, "manifest")
	list, _ := entries.([]any)
	var paths []string
	for _, e := range list {
		if o, ok := e.(Obj); ok {
			if p, ok := func() (string, bool) { v, _ := get(o, "path"); s, ok := v.(string); return s, ok }(); ok {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// ResolveRecipient is resolve_recipient: who a delivery goes to, verified against the linkage
// when one is wired, and the relationship row otherwise.
func (d *Service) ResolveRecipient(ctx context.Context, r Relationship, kind string) (string, Obj, error) {
	frozen := r.Parent.TaskID
	if kind == Revision {
		frozen = r.Child.TaskID
	}
	if d.Linkage == nil {
		return frozen, Obj{{Key: "source", Value: "relationship_row"}, {Key: "verified", Value: false}}, nil
	}
	rid := r.ID
	reading, err := d.Linkage.Up(ctx, rid)
	if err != nil {
		return "", nil, err
	}
	if readable, _ := get(reading, "readable"); readable != true {
		return "", nil, refuse(RelationUnreadable, "the linkage could not be read for relationship %s, so who owns its scope is unknown; the relationship row is not used as a fallback because an unreadable store has said nothing about the owner", store.PyRepr(rid))
	}
	contention, _ := get(reading, "contention")
	if state, _ := get(reading, "state"); state == "ambiguous" {
		return "", nil, refuse(DuplicateScopeOwner, "the linkage reports more than one candidate for relationship %s; this reader will not choose between them: %s", store.PyRepr(rid), pyReprValue(contention))
	}
	var live []any
	items, _ := contention.([]any)
	drifting := false
	for _, item := range items {
		o, _ := item.(Obj)
		if c, _ := get(o, "contention"); truthy(c) {
			live = append(live, item)
			if c == "owner_drift" {
				drifting = true
			}
		}
	}
	if len(live) > 0 {
		reason := LinkConflict
		if drifting {
			reason = RelationOwnerDrift
		}
		return "", nil, refuse(reason, "the linkage reports the hierarchy of relationship %s as inconsistent, so who owns its scope is not settled: %s. A resolved state with contention is not a resolved owner, and delivery waits for the hierarchy to settle rather than picking the side that happens to match the frozen row", store.PyRepr(rid), pyReprValue(live))
	}
	var wanted string
	switch kind {
	case Revision:
		wanted = "issue"
	case Completion, MergeTurnGrant:
		wanted = "project"
	default:
		return "", nil, refuse(NotClaimable, "%s is not a delivery direction, so it has no resolvable recipient", store.PyRepr(kind))
	}
	var level Obj
	levels, _ := get(reading, "levels")
	list, _ := levels.([]any)
	for _, lv := range list {
		o, _ := lv.(Obj)
		if k, _ := get(o, "scopeKind"); k == wanted {
			level = o
			break
		}
	}
	owner, _ := get(level, "owner")
	if level == nil || owner == nil {
		gaps, _ := get(reading, "gaps")
		return "", nil, refuse(UnregisteredScope, "the linkage records no live %s owner for relationship %s; gaps %s. Nothing found is reported as nothing found, never as a delivery that may proceed", wanted, store.PyRepr(rid), pyReprValue(gaps))
	}
	current := ""
	var revision any
	if o, ok := owner.(Obj); ok {
		current = str(o, "taskId")
		revision, _ = get(o, "revision")
	} else {
		current = pyStr(owner)
	}
	scopeKey, _ := get(level, "scopeKey")
	if current != frozen {
		return "", nil, refuse(RelationOwnerDrift, "relationship %s names %s but the linkage says %s %s is owned by %s. A report that arrived after the relationship changed is held rather than credited to either task; re-register the assignment under the current owner and deliver that", store.PyRepr(rid), store.PyRepr(frozen), wanted, pyReprValue(scopeKey), store.PyRepr(current))
	}
	return current, Obj{{Key: "source", Value: "linkage"}, {Key: "verified", Value: true}, {Key: "scopeKind", Value: wanted}, {Key: "scopeKey", Value: scopeKey}, {Key: "revision", Value: revision}}, nil
}

// Enqueue is DeliveryService.enqueue: idempotent, and only for a final event. recipient ""
// resolves the recipient.
func (d *Service) Enqueue(ctx context.Context, eventID, kind, recipient string) (Row, error) {
	if kind == "" {
		kind = Completion
	}
	event, err := d.eventRow(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, refuse(NotClaimable, "event %s was never accepted", store.PyRepr(eventID))
	}
	if event.S("stage") != "final" {
		return nil, refuse(NotClaimable, "event %s is %s; only a final event may be delivered", store.PyRepr(eventID), store.PyRepr(event.S("stage")))
	}
	rid := event.S("relationship_id")
	relationship, err := RequireActive(ctx, d.Store, rid)
	if err != nil {
		return nil, err
	}
	var resolution Obj
	if recipient == "" {
		if recipient, resolution, err = d.ResolveRecipient(ctx, relationship, kind); err != nil {
			return nil, err
		}
	}
	if err := assertAssignmentDelivery(relationship, kind, recipient, nil, &rid, manifestPaths(event)); err != nil {
		return nil, err
	}
	existing, err := d.Find(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, d.ClearIntent(ctx, eventID)
	}
	err = d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if err := d.EnqueueIn(ctx, eventID, rid, kind, recipient); err != nil {
			return err
		}
		_, err := execSQL(ctx, d.Store, "DELETE FROM delivery_intent WHERE event_id = ?", eventID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if verified, _ := get(resolution, "verified"); verified == true {
		if err := d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
			return journal(ctx, d.Store, "delivery_recipient_resolved", eventID, resolution, d.Clock.ISO())
		}); err != nil {
			return nil, err
		}
	}
	return d.Get(ctx, eventID)
}

// EnqueueIn queues inside the caller's transaction (ctx must carry it).
func (d *Service) EnqueueIn(ctx context.Context, eventID, rid, kind, recipient string) error {
	now := d.Clock.ISO()
	if _, err := execSQL(ctx, d.Store, "INSERT OR IGNORE INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at, created_at, updated_at) VALUES (?,?,?,?,?,?,0,NULL,?,?)", eventID, rid, kind, recipient, recipient, Queued, now, now); err != nil {
		return err
	}
	if err := journal(ctx, d.Store, "delivery_queued", eventID, Obj{{Key: "kind", Value: kind}}, now); err != nil {
		return err
	}
	return d.AnnotatePredecessorsIn(ctx, eventID)
}

// RecordIntentIn is record_intent_in: delivery was wanted here and refused for now.
func (d *Service) RecordIntentIn(ctx context.Context, eventID, rid, kind, recipient, errorText string, now float64) error {
	row, err := one(ctx, d.Store, "SELECT attempts FROM delivery_intent WHERE event_id = ?", eventID)
	if err != nil {
		return err
	}
	attempts := int64(1)
	if row != nil {
		attempts = row.I("attempts") + 1
	}
	_, err = execSQL(ctx, d.Store, "INSERT INTO delivery_intent (event_id, relationship_id, kind, recipient_task_id, attempts, next_retry_at, last_error, noted_at) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET attempts = excluded.attempts, next_retry_at = excluded.next_retry_at, last_error = excluded.last_error",
		eventID, rid, kind, recipient, attempts, now+d.backoff(attempts), errorText, d.Clock.ISO())
	return err
}

func (d *Service) backoff(attempts int64) float64 {
	base, ceiling := d.Policy.PresendBase, d.Policy.PresendMax
	if base <= 0 {
		return ceiling
	}
	steps := max(0, attempts-1)
	if ceiling <= base || float64(steps) >= math.Ceil(math.Log2(ceiling/base)) {
		return ceiling
	}
	return math.Min(ceiling, base*math.Pow(2, float64(steps)))
}

// ClearIntent is clear_intent.
func (d *Service) ClearIntent(ctx context.Context, eventID string) error {
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, d.Store, "DELETE FROM delivery_intent WHERE event_id = ?", eventID)
		return err
	})
}

const eligibleWhere = " WHERE d.state IN (?,?,?) AND d.hold_reason IS NULL AND (d.next_eligible_at IS NULL OR d.next_eligible_at <= ?) AND r.status = 'active' AND r.superseded_by IS NULL AND e.stage = 'final'"

// EligibleParents is eligible_parents.
func (d *Service) EligibleParents(ctx context.Context, now float64) ([]string, error) {
	rows, err := all(ctx, d.Store, "SELECT DISTINCT r.parent_task_id AS parent_task_id FROM deliveries d JOIN relationships r ON r.relationship_id = d.relationship_id JOIN events e ON e.event_id = d.event_id"+eligibleWhere+" ORDER BY r.parent_task_id", Queued, DeferredBusy, WithheldPreSend, now)
	var out []string
	for _, r := range rows {
		out = append(out, r.S("parent_task_id"))
	}
	return out, err
}

func (d *Service) eligibleForParent(ctx context.Context, parent string, now float64, limit, offset int) ([]Row, error) {
	return all(ctx, d.Store, "SELECT d.*, r.parent_task_id AS parent_task_id FROM deliveries d JOIN relationships r ON r.relationship_id = d.relationship_id JOIN events e ON e.event_id = d.event_id"+eligibleWhere+" AND r.parent_task_id = ? ORDER BY d.created_at LIMIT ? OFFSET ?", Queued, DeferredBusy, WithheldPreSend, now, parent, limit, offset)
}

// EligibleCount is eligible_count.
func (d *Service) EligibleCount(ctx context.Context, parent string, now float64) (int64, error) {
	row, err := one(ctx, d.Store, "SELECT COUNT(*) AS c FROM deliveries d JOIN relationships r ON r.relationship_id = d.relationship_id JOIN events e ON e.event_id = d.event_id"+eligibleWhere+" AND r.parent_task_id = ?", Queued, DeferredBusy, WithheldPreSend, now, parent)
	return row.I("c"), err
}

// Eligible is eligible: every eligible parent, then a bounded share each, dealt one at a time.
func (d *Service) Eligible(ctx context.Context, now float64, limit, perParent, cursor int, offsets map[string]int) ([]Row, error) {
	parents, err := d.EligibleParents(ctx, now)
	if err != nil || len(parents) == 0 {
		return nil, err
	}
	share := perParent
	if share == 0 {
		share = d.Policy.MaxSendsPerParentPerTick
	}
	start := cursor % len(parents)
	order := append(slices.Clone(parents[start:]), parents[:start]...)
	queues := map[string][]Row{}
	for _, parent := range order {
		offset := offsets[parent]
		taken, err := d.eligibleForParent(ctx, parent, now, share, offset)
		if err != nil {
			return nil, err
		}
		if len(taken) < share && offset > 0 {
			seen := map[string]bool{}
			for _, r := range taken {
				seen[r.S("event_id")] = true
			}
			again, err := d.eligibleForParent(ctx, parent, now, share, 0)
			if err != nil {
				return nil, err
			}
			for _, r := range again {
				if len(taken) >= share {
					break
				}
				if !seen[r.S("event_id")] {
					taken = append(taken, r)
				}
			}
		}
		queues[parent] = taken
	}
	var selected []Row
	for len(selected) < limit {
		any := false
		for _, parent := range order {
			if len(selected) >= limit {
				break
			}
			if len(queues[parent]) > 0 {
				selected = append(selected, queues[parent][0])
				queues[parent] = queues[parent][1:]
				any = true
			}
		}
		if !any {
			break
		}
	}
	return selected, nil
}

var errNotClaimable = errors.New("not claimable")
var errPaced = errors.New("paced")

type superseded struct {
	reason string
	late   bool
}

func (s *superseded) Error() string { return "superseded: " + s.reason }

// pacing reads send_pacing on the ctx-aware querier.
func (d *Service) pacing(ctx context.Context, recipient string, now float64) (Obj, error) {
	window, earliest := d.Policy.RateWindows(now)
	last, err := one(ctx, d.Store, "SELECT MAX(last_send_at) AS last FROM recipient_rate WHERE recipient_task_id = ? AND window_start BETWEEN ? AND ?", recipient, earliest, window)
	if err != nil {
		return nil, err
	}
	used, err := one(ctx, d.Store, "SELECT sends FROM recipient_rate WHERE recipient_task_id = ? AND window_start = ?", recipient, window)
	if err != nil {
		return nil, err
	}
	var lastAt *float64
	if last != nil && !last.N("last") {
		v := last.F("last")
		lastAt = &v
	}
	return d.Policy.Pacing(now, used.I("sends"), lastAt), nil
}

// SendRefusal is send_refusal: why recipient may not be woken at now, or "".
func (d *Service) SendRefusal(ctx context.Context, recipient string, now float64) (string, error) {
	p, err := d.pacing(ctx, recipient, now)
	return str(p, "reason"), err
}

// ReserveSend is reserve_send, inside the caller's claim transaction.
func (d *Service) ReserveSend(ctx context.Context, recipient string, now float64) (string, error) {
	refused, err := d.SendRefusal(ctx, recipient, now)
	if err != nil || refused != "" {
		return refused, err
	}
	window := math.Floor(now/3600) * 3600
	_, err = execSQL(ctx, d.Store, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,1,?) ON CONFLICT(recipient_task_id, window_start) DO UPDATE SET sends = sends + 1, last_send_at = excluded.last_send_at", recipient, window, now)
	return "", err
}

func (d *Service) rateLimited(ctx context.Context, recipient string, now float64) (bool, error) {
	if d.RateLimited != nil {
		return d.RateLimited(recipient, now), nil
	}
	refused, err := d.SendRefusal(ctx, recipient, now)
	return refused != "", err
}

func (d *Service) pacedUntil(ctx context.Context, recipient string, now float64) (float64, error) {
	p, err := d.pacing(ctx, recipient, now)
	if err != nil {
		return 0, err
	}
	if str(p, "reason") == HourlyCap {
		reopens, _ := get(p, "reopensAt")
		at, ok := reopens.(float64)
		if !ok {
			ws, _ := get(p, "windowStart")
			at = ws.(float64) + RateWindowSeconds
		}
		return math.Min(at, now+60), nil
	}
	return now + d.Policy.MinSendInterval, nil
}

// SupersessionReason is _supersession_reason, read on ctx's querier. "" means still current.
func (d *Service) SupersessionReason(ctx context.Context, eventID string) (string, error) {
	event, err := one(ctx, d.Store, "SELECT relationship_id, execution_generation, outcome, event_id, receipt FROM events WHERE event_id = ?", eventID)
	if err != nil || event == nil {
		return "", err
	}
	if event.S("outcome") == MergeTurnGrant {
		return "", fmt.Errorf("delivery: merge-turn grant supersession is owned by the merge-turn port (todo 26)")
	}
	rel, err := one(ctx, d.Store, "SELECT execution_generation FROM relationships WHERE relationship_id = ?", event.S("relationship_id"))
	if err != nil || rel == nil {
		return "", err
	}
	if event.I("execution_generation") < rel.I("execution_generation") {
		return StaleGeneration, nil
	}
	if event.S("outcome") == Revision {
		answered, err := one(ctx, d.Store, "SELECT 1 FROM events WHERE relationship_id = ? AND execution_generation = ? AND stage = 'final' AND suppressed_reason IS NULL AND event_id != ? AND outcome NOT IN ('merge_turn_grant')", event.S("relationship_id"), event.I("execution_generation"), eventID)
		if err != nil || answered == nil {
			return "", err
		}
		return SupersededRevision, nil
	}
	if slices.Contains(executionOnlyOutcomes, event.S("outcome")) {
		return "", nil
	}
	head, err := HeadRevision(ctx, d.Store, event.S("relationship_id"), event.I("execution_generation"))
	if err != nil {
		return "", err
	}
	headID, _ := get(head, "eventId")
	if headID == nil || headID == eventID {
		return "", nil
	}
	successor, err := one(ctx, d.Store, "SELECT stage FROM events WHERE event_id = ?", headID)
	if err != nil || successor == nil || successor.S("stage") != "final" {
		return "", err
	}
	return SupersededRevision, nil
}

func (d *Service) annotateIn(ctx context.Context, eventID, reason string) error {
	_, err := execSQL(ctx, d.Store, "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied) VALUES (?,?,?,0) ON CONFLICT(event_id) DO NOTHING", eventID, reason, d.Clock.ISO())
	return err
}

// AnnotatePredecessorsIn marks outstanding deliveries this newly final event replaces.
func (d *Service) AnnotatePredecessorsIn(ctx context.Context, eventID string) error {
	event, err := one(ctx, d.Store, "SELECT relationship_id, execution_generation FROM events WHERE event_id = ?", eventID)
	if err != nil || event == nil {
		return err
	}
	others, err := all(ctx, d.Store, "SELECT d.event_id FROM deliveries d JOIN events e ON e.event_id = d.event_id WHERE e.relationship_id = ? AND e.execution_generation = ? AND d.event_id != ? AND d.state IN ('queued','sending','held_uncertain','dispatched','inbox_only','deferred_busy','withheld_pre_send')", event.S("relationship_id"), event.I("execution_generation"), eventID)
	if err != nil {
		return err
	}
	for _, row := range others {
		reason, err := d.SupersessionReason(ctx, row.S("event_id"))
		if err != nil {
			return err
		}
		if reason != "" {
			if err := d.annotateIn(ctx, row.S("event_id"), reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// AnnotatePredecessors is the same annotation in its own transaction.
func (d *Service) AnnotatePredecessors(ctx context.Context, eventID string) error {
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return d.AnnotatePredecessorsIn(ctx, eventID) })
}

func (d *Service) supersedeIn(ctx context.Context, eventID, reason string) error {
	changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, hold_reason = ?, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND hold_reason IS NULL", Superseded, reason, d.Clock.ISO(), eventID, Queued, DeferredBusy, WithheldPreSend)
	if err != nil {
		return err
	}
	if changed == 1 {
		return journal(ctx, d.Store, "delivery_superseded", eventID, Obj{{Key: "reason", Value: reason}}, d.Clock.ISO())
	}
	return d.annotateIn(ctx, eventID, reason)
}

// MarkSuperseded is mark_superseded: withdraw what the recipient cannot be acting on yet.
func (d *Service) MarkSuperseded(ctx context.Context, eventID, reason string) error {
	if reason == "" {
		reason = SupersededHold
	}
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return d.supersedeIn(ctx, eventID, reason) })
}

func (d *Service) suppressIfSuperseded(ctx context.Context, eventID string) (string, error) {
	var reason string
	err := d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		reason, err = d.SupersessionReason(ctx, eventID)
		if err != nil || reason == "" {
			return err
		}
		return d.supersedeIn(ctx, eventID, reason)
	})
	return reason, err
}

// ReadWorkReport reports whether a work report exists; the composed renderer is report.py's,
// ported by todo 24. Until then an event with a work report refuses to render.
func (d *Service) hasWorkReport(ctx context.Context, eventID string) (bool, error) {
	row, err := one(ctx, d.Store, "SELECT 1 FROM work_reports WHERE event_id = ? LIMIT 1", eventID)
	return row != nil, err
}

func (d *Service) render(ctx context.Context, row Row, record Obj, request string) (string, error) {
	report, err := d.hasWorkReport(ctx, row.S("event_id"))
	if err != nil {
		return "", err
	}
	if report {
		return "", fmt.Errorf("delivery: the composed report rendering (report.py) is owned by todo 24")
	}
	switch row.S("kind") {
	case Revision:
		return renderRevision(row, record, request), nil
	case Completion:
		return renderCompletion(row, record, request), nil
	}
	return "", fmt.Errorf("delivery: the merge-turn grant notice is owned by the merge-turn port (todo 26)")
}

// PreviewMessage is preview_message / render_message: what the NEXT attempt would say.
func (d *Service) PreviewMessage(ctx context.Context, eventID string) (string, error) {
	row, err := d.Get(ctx, eventID)
	if err != nil {
		return "", err
	}
	record, err := d.Receipt(ctx, eventID)
	if err != nil {
		return "", err
	}
	request, err := store.RequestID(eventID, int(row.I("attempt_count"))+1)
	if err != nil {
		return "", err
	}
	return d.render(ctx, row, record, request)
}

type claimed struct {
	attemptNo int64
	requestID string
	message   string
}

// claim is _claim: authorization, staging and eligibility decided in one atomic statement,
// with the attempt, its bytes and the send budget written in the same transaction.
func (d *Service) claim(ctx context.Context, eventID string, now float64, owner, recipient string) (claimed, error) {
	reason, err := d.suppressIfSuperseded(ctx, eventID)
	if err != nil {
		return claimed{}, err
	}
	if reason != "" {
		return claimed{}, &superseded{reason: reason}
	}
	var out claimed
	err = d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		late, err := d.SupersessionReason(ctx, eventID)
		if err != nil {
			return err
		}
		if late != "" {
			return &superseded{reason: late, late: true}
		}
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, lease_owner = ?, lease_until = ?, attempt_count = attempt_count + 1, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND hold_reason IS NULL AND (next_eligible_at IS NULL OR next_eligible_at <= ?) AND EXISTS (SELECT 1 FROM relationships r WHERE r.relationship_id = deliveries.relationship_id AND r.status = 'active' AND r.superseded_by IS NULL) AND EXISTS (SELECT 1 FROM events e WHERE e.event_id = deliveries.event_id AND e.stage = 'final') AND NOT EXISTS (SELECT 1 FROM events ev JOIN relationships rr ON rr.relationship_id = ev.relationship_id WHERE ev.event_id = deliveries.event_id AND ev.outcome NOT IN ('merge_turn_grant') AND ev.execution_generation < rr.execution_generation)",
			Sending, owner, now+d.Policy.Lease, d.Clock.ISO(), eventID, Queued, DeferredBusy, WithheldPreSend, now)
		if err != nil {
			return err
		}
		if changed != 1 {
			return errNotClaimable
		}
		row, err := d.Get(ctx, eventID)
		if err != nil {
			return err
		}
		out.attemptNo = row.I("attempt_count")
		if out.requestID, err = store.RequestID(eventID, int(out.attemptNo)); err != nil {
			return err
		}
		clash, err := one(ctx, d.Store, "SELECT event_id FROM attempts WHERE request_id = ?", out.requestID)
		if err != nil {
			return err
		}
		if clash != nil && clash.S("event_id") != eventID {
			return refuse(NotClaimable, "request id %s already belongs to event %s", store.PyRepr(out.requestID), store.PyRepr(clash.S("event_id")))
		}
		record, err := d.Receipt(ctx, eventID)
		if err != nil {
			return err
		}
		if out.message, err = d.render(ctx, row, record, out.requestID); err != nil {
			return err
		}
		if _, err := execSQL(ctx, d.Store, "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?)", out.requestID, eventID, out.attemptNo, row.S("kind"), "in_flight", HeldUncertain, d.Clock.ISO(), d.Clock.ISO()); err != nil {
			return err
		}
		if _, err := execSQL(ctx, d.Store, "INSERT INTO attempt_messages (request_id, event_id, attempt_no, kind, message, rendered_at) VALUES (?,?,?,?,?,?)", out.requestID, eventID, out.attemptNo, row.S("kind"), out.message, d.Clock.ISO()); err != nil {
			return err
		}
		if row.S("kind") == Revision {
			if err := d.journalRestorationAttempted(ctx, eventID, record, out.attemptNo); err != nil {
				return err
			}
		}
		refused, err := d.ReserveSend(ctx, recipient, now)
		if err != nil {
			return err
		}
		if refused != "" {
			return errPaced
		}
		return nil
	})
	return out, err
}

// journalRestorationAttempted records what the frozen bytes carried of a declared block
// (restoration.project_cap over the legacy renderer's cap).
func (d *Service) journalRestorationAttempted(ctx context.Context, eventID string, record Obj, attemptNo int64) error {
	findingsValue, _ := get(record, "criteria")
	findings, _ := findingsValue.([]any)
	index := -1
	var block Obj
	for i, f := range findings {
		o, _ := f.(Obj)
		if v, _ := get(o, "restoration"); truthy(v) {
			index, block = i, o
			break
		}
	}
	if block == nil {
		return nil
	}
	where := fmt.Sprintf("finding %d of %d", index+1, len(findings))
	outcome, detail := "carried", fmt.Sprintf("%s, within the %d this renderer shows", where, manifestLines)
	if index >= manifestLines {
		outcome, detail = "truncated", fmt.Sprintf("%s, past the %d this renderer shows", where, manifestLines)
	}
	id, _ := get(block, "id")
	return journal(ctx, d.Store, "restoration_attempted", eventID, Obj{{Key: "outcome", Value: outcome}, {Key: "basis", Value: "relay-message/legacy"}, {Key: "criterion", Value: id}, {Key: "detail", Value: detail}, {Key: "attempt", Value: attemptNo}}, d.Clock.ISO())
}

func statusForRecord(l Lifecycle) string {
	if l.Deliverable == "busy" {
		return "active"
	}
	if s, ok := l.RuntimeStatus.(string); ok && slices.Contains([]string{"idle", "active", "notLoaded", "systemError"}, s) {
		return s
	}
	return "unknown"
}

// Attempt is DeliveryService.attempt. A nil record with a nil error is Python's None: nothing
// was due, or the delivery was deferred or withheld without an attempt.
func (d *Service) Attempt(ctx context.Context, eventID string, adapter Adapter, now *float64, owner string) (Obj, error) {
	at := d.Clock.Now()
	if now != nil {
		at = *now
	}
	if owner == "" {
		owner = "relay"
	}
	row, err := d.Get(ctx, eventID)
	if err != nil {
		return nil, err
	}
	relationship, err := LoadRelationship(ctx, d.Store, row.S("relationship_id"))
	if err != nil {
		return nil, err
	}
	recipient := row.S("recipient_task_id")
	if !row.N("hold_reason") && row.S("hold_reason") != "" {
		return nil, nil
	}
	if !slices.Contains(claimable, row.S("state")) {
		return nil, nil
	}
	if relationship.Status != "active" && relationship.SupersededBy == nil {
		return d.WithholdInactive(ctx, eventID, relationship, at, row.I("attempt_count"))
	}
	if !row.N("next_eligible_at") && row.F("next_eligible_at") > at {
		return nil, nil
	}
	event, err := d.eventRow(ctx, eventID)
	if err != nil {
		return nil, err
	}
	eventRID := event.S("relationship_id")
	thread := row.S("recipient_thread_id")
	if err := assertAssignmentDelivery(relationship, row.S("kind"), recipient, &thread, &eventRID, manifestPaths(event)); err != nil {
		return nil, err
	}
	limited, err := d.rateLimited(ctx, recipient, at)
	if err != nil {
		return nil, err
	}
	if limited {
		when, err := d.pacedUntil(ctx, recipient, at)
		if err != nil {
			return nil, err
		}
		return nil, d.reschedule(ctx, eventID, row.S("state"), when, row.I("attempt_count"))
	}
	var cwd any
	if row.S("kind") == Completion || row.S("kind") == MergeTurnGrant {
		cwd = relationship.Parent.Cwd
	}
	observation := Observe(adapter, thread, cwd, d.RequireLifecycleEvidence)
	if err := RecordLifecycle(ctx, d.Store, d.Clock, observation); err != nil {
		return nil, err
	}
	if observation.IsBusy() {
		return nil, d.deferBusy(ctx, eventID, row, at)
	}
	if !observation.MaySend() {
		return nil, d.withhold(ctx, eventID, observation, at, row, relationship)
	}
	settings, err := AuthorizedSettings(ctx, d.Store, recipient, d.RoleGate)
	if Reason(err) != "" {
		return nil, d.withholdSettings(ctx, eventID, at, err, row)
	}
	if err != nil {
		return nil, err
	}
	known, err := adapter.ListTurnIDs(thread, 25)
	if err != nil {
		return nil, err
	}
	c, err := d.claim(ctx, eventID, at, owner, recipient)
	var gone *superseded
	switch {
	case errors.Is(err, errPaced):
		when, err := d.pacedUntil(ctx, recipient, at)
		if err != nil {
			return nil, err
		}
		return nil, d.reschedule(ctx, eventID, row.S("state"), when, row.I("attempt_count"))
	case errors.Is(err, errNotClaimable):
		return nil, nil
	case errors.As(err, &gone):
		if gone.late {
			if _, err := d.suppressIfSuperseded(ctx, eventID); err != nil {
				return nil, err
			}
		}
		return Obj{{Key: "deliveryState", Value: Superseded}, {Key: "supersededReason", Value: gone.reason}, {Key: "eventId", Value: eventID}, {Key: "sendAttempted", Value: "no"}}, nil
	case err != nil:
		return nil, err
	}
	receipt, sendErr := adapter.SendMessage(c.requestID, thread, c.message, settings)
	if sendErr != nil {
		receipt = Obj{{Key: "requestId", Value: c.requestID}, {Key: "status", Value: OutcomeUnknown}, {Key: "error", Value: errorLabel(sendErr)}}
	}
	facts := Classify(receipt)
	findings, _ := get(receipt, "settingsFindings")
	if facts.FailedOperation != nil || slices.Contains([]string{WithheldPreSend, InboxOnly, HeldUncertain}, facts.DeliveryState) {
		operation := "transport"
		if op, ok := facts.FailedOperation.(string); ok {
			operation = op
		}
		if truthy(findings) {
			operation = "settings_check"
		}
		detail := facts.DeliveryState
		if t, ok := facts.ErrorText.(string); ok && t != "" {
			detail = t
		}
		safe := facts.RetrySafe
		if err := d.RecordFailure(ctx, eventID, operation, detail, row.S("relationship_id"), relationship.Parent.TaskID, facts.RPCErrorCode, renderFindings(findings), &safe, nil); err != nil {
			return nil, err
		}
	}
	previously := false
	if t, ok := facts.TurnID.(string); ok && slices.Contains(known, t) {
		previously = true
	}
	record, err := AttemptRecord(facts, c.requestID, eventID, c.attemptNo, recipient, statusForRecord(observation), d.Clock.ISO(), nil)
	if err != nil {
		return nil, err
	}
	notes, _ := get(receipt, "settingsNotes")
	elsewhere, err := d.settle(ctx, eventID, c.requestID, record, facts, previously, at, settingsRefusalOf(facts, findings), notes)
	if err != nil {
		return nil, err
	}
	if elsewhere != nil {
		result := record
		if !elsewhere.N("record") {
			result = loadsObj(elsewhere.S("record"))
		}
		result = append(append(Obj(nil), result...), F{Key: "_settledElsewhere", Value: true}, F{Key: "_deliveryState", Value: elsewhere.Opt("delivery_state")}, F{Key: "_lifecycle", Value: observation.Deliverable})
		return result, nil
	}
	origin := any(nil)
	if previously {
		origin = "steered_observed_turn"
	} else if facts.TurnID != nil {
		origin = "unknown"
	}
	result := append(append(Obj(nil), record...), F{Key: "_turnPreviouslyObserved", Value: previously}, F{Key: "_turnOrigin", Value: origin}, F{Key: "_lifecycle", Value: observation.Deliverable})
	return result, nil
}

func renderFindings(findings any) any {
	list, _ := findings.([]any)
	var parts []string
	for _, f := range list {
		o, ok := f.(Obj)
		if !ok {
			continue
		}
		field, present := get(o, "field")
		if !present {
			if field, present = get(o, "code"); !present {
				field = "?"
			}
		}
		expected, _ := get(o, "expected")
		returned, _ := get(o, "returned")
		parts = append(parts, fmt.Sprintf("%s: expected %s, host %s", pyStr(field), pyReprValue(expected), pyReprValue(returned)))
	}
	if len(parts) == 0 {
		return nil
	}
	return strings.Join(parts, "; ")
}

var attemptSettingsCodes = append(slices.Clone(SettingsRefusals), UnsupportedApprovalPolicy)

// settingsRefusalOf is settings_refusal_of: this attempt's settings cause, or nil.
func settingsRefusalOf(facts Facts, findings any) any {
	code, ok := facts.RPCErrorCode.(string)
	if facts.FailedOperation != "thread/resume" || !ok || !slices.Contains(attemptSettingsCodes, code) {
		return nil
	}
	var field any
	if list, _ := findings.([]any); len(list) > 0 {
		if o, ok := list[0].(Obj); ok {
			if f, ok := get(o, "field"); ok {
				if s, ok := f.(string); ok {
					field = s
				}
			}
		}
	}
	return Obj{{Key: "reason", Value: code}, {Key: "field", Value: field}}
}

// RecordFailure is record_failure.
func (d *Service) RecordFailure(ctx context.Context, scope, operation, detail string, rid, parent, code, difference any, retrySafe *bool, nextRetry any) error {
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return d.recordFailureIn(ctx, scope, operation, d.Clock.ISO(), detail, rid, parent, code, difference, retrySafe, nextRetry)
	})
}

func (d *Service) recordFailureIn(ctx context.Context, scope, operation, occurred, detail string, rid, parent, code, difference any, retrySafe *bool, nextRetry any) error {
	var safe any
	if retrySafe != nil {
		safe = int64(0)
		if *retrySafe {
			safe = int64(1)
		}
	}
	_, err := execSQL(ctx, d.Store, "INSERT INTO failed_operations (scope_key, operation, relationship_id, parent_task_id, detail, error_code, difference, retry_safe, occurred_at, next_retry_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope_key, operation) DO UPDATE SET detail=excluded.detail, error_code=excluded.error_code, difference=excluded.difference, retry_safe=excluded.retry_safe, occurred_at=excluded.occurred_at, next_retry_at=excluded.next_retry_at, relationship_id=COALESCE(excluded.relationship_id, failed_operations.relationship_id), parent_task_id=COALESCE(excluded.parent_task_id, failed_operations.parent_task_id)",
		scope, operation, nullable(rid), nullable(parent), detail, code, difference, safe, occurred, nextRetry)
	return err
}

func (d *Service) reschedule(ctx context.Context, eventID, state string, when float64, attempts int64) error {
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?", state, when, d.Clock.ISO(), eventID, Queued, DeferredBusy, WithheldPreSend, attempts)
		return err
	})
}

// Reschedule is _reschedule (tests reach it as Python's do).
func (d *Service) Reschedule(ctx context.Context, eventID, state string, when float64, attempts int64) error {
	return d.reschedule(ctx, eventID, state, when, attempts)
}

// DeferBusy is _defer_busy: a busy recipient is left alone and retried later.
func (d *Service) DeferBusy(ctx context.Context, eventID string, row Row, now float64) error {
	return d.deferBusy(ctx, eventID, row, now)
}

func (d *Service) deferBusy(ctx context.Context, eventID string, row Row, now float64) error {
	attempts := row.I("attempt_count")
	var hold any
	if attempts >= d.Policy.BusyMaxAttempts {
		hold = d.Policy.CapReason("busy")
	}
	when := now + d.Policy.DelayFor(attempts+1, "busy")
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		stamp := d.Clock.ISO()
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = ?, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?", DeferredBusy, when, hold, stamp, eventID, Queued, DeferredBusy, WithheldPreSend, attempts)
		if err != nil {
			return err
		}
		if changed == 1 {
			if err := d.recordFailureIn(ctx, eventID, "parent_busy", stamp, "the recipient is mid-turn and is never interrupted", row.S("relationship_id"), nil, nil, nil, nil, when); err != nil {
				return err
			}
		}
		if row.S("state") != DeferredBusy {
			return journal(ctx, d.Store, "delivery_deferred_busy", eventID, "", d.Clock.ISO())
		}
		return nil
	})
}

// WithholdInactive is _withhold_inactive: a status a person set stops the delivery with the
// reason kept; nil when the relationship moved under the read.
func (d *Service) WithholdInactive(ctx context.Context, eventID string, r Relationship, now float64, attempts int64) (Obj, error) {
	when := now + d.Policy.LifecycleRecheck
	moved := false
	err := d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = MAX(?, COALESCE(next_eligible_at, 0)), updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ? AND EXISTS (SELECT 1 FROM relationships r WHERE r.relationship_id = deliveries.relationship_id AND r.status = ? AND r.superseded_by IS NULL)",
			WithheldPreSend, when, d.Clock.ISO(), eventID, Queued, DeferredBusy, WithheldPreSend, attempts, r.Status)
		if err != nil {
			return err
		}
		if changed != 1 {
			moved = true
			return nil
		}
		if err := journal(ctx, d.Store, "delivery_withheld_inactive", eventID, Obj{{Key: "relationshipId", Value: r.ID}, {Key: "status", Value: r.Status}}, d.Clock.ISO()); err != nil {
			return err
		}
		return journal(ctx, d.Store, PresendWithheld, eventID, Obj{{Key: "reason", Value: RelationshipNotActive}, {Key: "operation", Value: nil}}, d.Clock.ISO())
	})
	if err != nil || moved {
		return nil, err
	}
	return Obj{{Key: "deliveryState", Value: WithheldPreSend}, {Key: "withheldReason", Value: RelationshipNotActive}, {Key: "relationshipStatus", Value: r.Status}, {Key: "eventId", Value: eventID}, {Key: "sendAttempted", Value: "no"}}, nil
}

func (d *Service) withhold(ctx context.Context, eventID string, l Lifecycle, now float64, row Row, r Relationship) error {
	when := now + d.Policy.LifecycleRecheck
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		stamp := d.Clock.ISO()
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?", WithheldPreSend, when, stamp, eventID, Queued, DeferredBusy, WithheldPreSend, row.I("attempt_count"))
		if err != nil {
			return err
		}
		if changed == 1 {
			detail := l.Detail
			if detail == "" {
				detail = pyStr(l.WithholdReason)
				if l.WithholdReason == nil {
					detail = "not deliverable"
				}
			}
			if err := d.recordFailureIn(ctx, eventID, "lifecycle_read", stamp, detail, row.S("relationship_id"), r.Parent.TaskID, l.WithholdReason, nil, nil, when); err != nil {
				return err
			}
			if err := journal(ctx, d.Store, PresendWithheld, eventID, Obj{{Key: "reason", Value: l.WithholdReason}, {Key: "operation", Value: "lifecycle_read"}}, stamp); err != nil {
				return err
			}
		}
		return journal(ctx, d.Store, "delivery_withheld", eventID, Obj{{Key: "reason", Value: l.WithholdReason}, {Key: "detail", Value: l.Detail}}, d.Clock.ISO())
	})
}

func (d *Service) withholdSettings(ctx context.Context, eventID string, now float64, refusal error, row Row) error {
	when := now + d.Policy.LifecycleRecheck
	reason := Reason(refusal)
	if reason == "" {
		reason = "settings_unavailable"
	}
	detail := Detail(refusal)
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		stamp := d.Clock.ISO()
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, updated_at = ? WHERE event_id = ? AND state IN (?,?,?) AND attempt_count = ?", WithheldPreSend, when, stamp, eventID, Queued, DeferredBusy, WithheldPreSend, row.I("attempt_count"))
		if err != nil {
			return err
		}
		if err := journal(ctx, d.Store, "delivery_withheld", eventID, Obj{{Key: "reason", Value: reason}, {Key: "detail", Value: detail}}, d.Clock.ISO()); err != nil {
			return err
		}
		if changed != 1 {
			return nil
		}
		safe := true
		if err := d.recordFailureIn(ctx, eventID, "settings_check", stamp, detail, row.S("relationship_id"), nil, reason, nil, &safe, when); err != nil {
			return err
		}
		return journal(ctx, d.Store, PresendWithheld, eventID, Obj{{Key: "reason", Value: reason}, {Key: "operation", Value: "settings_check"}, {Key: "detail", Value: detail}}, stamp)
	})
}

// settle is _settle: this send's own attempt, if nothing else settled it first. A non-nil row
// is what another reader stored.
func (d *Service) settle(ctx context.Context, eventID, requestID string, record Obj, facts Facts, previously bool, now float64, settingsRefusal any, notes any) (Row, error) {
	if err := AssertAttemptInvariants(record); err != nil {
		return nil, err
	}
	state := facts.DeliveryState
	attemptNo, _ := get(record, "attemptNo")
	n := attemptNo.(int64)
	var hold, when any
	switch state {
	case DeferredBusy:
		when = now + d.Policy.DelayFor(n+1, "busy")
		if n >= d.Policy.BusyMaxAttempts {
			hold = d.Policy.CapReason("busy")
		}
	case WithheldPreSend:
		when = now + d.Policy.DelayFor(n+1, "presend")
		if n >= d.Policy.MaxAttempts {
			hold = d.Policy.CapReason("presend")
		}
	case InboxOnly:
		hold = PushChannelClosed
	}
	var stored Row
	err := d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		settled, err := execSQL(ctx, d.Store, "UPDATE attempts SET internal_state = 'settled', state = ?, record = ?, observed_at = ? WHERE request_id = ? AND internal_state = 'in_flight'", state, dumps(record), str(record, "observedAt"), requestID)
		if err != nil {
			return err
		}
		if settled != 1 {
			stored, err = one(ctx, d.Store, "SELECT a.record, a.state, a.affirmative_evidence, d.state AS delivery_state FROM attempts a JOIN deliveries d ON d.event_id = a.event_id WHERE a.request_id = ?", requestID)
			if err != nil {
				return err
			}
			detail := Obj{{Key: "requestId", Value: requestID}, {Key: "receiptState", Value: state}, {Key: "storedState", Value: stored.Opt("state")}, {Key: "storedEvidence", Value: stored.Opt("affirmative_evidence")}}
			return journal(ctx, d.Store, "delivery_attempt_settled_elsewhere", eventID, detail, d.Clock.ISO())
		}
		var evidence any
		if state == Dispatched {
			evidence = "transport_accepted"
		}
		if _, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = ?, dispatch_evidence = ?, dispatch_turn_id = ?, lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE event_id = ?", state, when, hold, evidence, facts.TurnID, d.Clock.ISO(), eventID); err != nil {
			return err
		}
		safe, _ := get(record, "retrySafe")
		if err := journal(ctx, d.Store, "delivery_attempted", eventID, Obj{{Key: "requestId", Value: requestID}, {Key: "state", Value: state}, {Key: "retrySafe", Value: safe}, {Key: "turnPreviouslyObserved", Value: previously}, {Key: "hold", Value: hold}, {Key: "settingsRefusal", Value: settingsRefusal}}, d.Clock.ISO()); err != nil {
			return err
		}
		if truthy(notes) {
			return journal(ctx, d.Store, SettingsNoted, eventID, Obj{{Key: "requestId", Value: requestID}, {Key: "notes", Value: notes}}, d.Clock.ISO())
		}
		return nil
	})
	return stored, err
}
