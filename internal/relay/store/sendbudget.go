package store

import (
	"context"
	"math"
	"strings"
	"time"
)

// SendStamp is an instant in the text form every send is stamped with (datetime.isoformat at
// microseconds, UTC). Stamps of this one fixed-width form order as text, so "stamped inside an
// hour window" is a text comparison.
func SendStamp(at float64) string {
	sec := math.Floor(at)
	micro := math.RoundToEven((at - sec) * 1e6)
	if micro >= 1e6 {
		sec++
		micro -= 1e6
	}
	return time.Unix(int64(sec), int64(micro)*1000).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// RelationshipSendsSQL is the SQL expression counting the sends one relationship has charged to
// one recipient inside an hour window. A send is one distinct event (a supervisor message, for a
// transport): the attempts made at it, however many, are one send, and what bounds an event that is
// tried again and again is its own attempt cap (delivery.RetryPolicy.MaxAttempts), not the hour.
// relationship and recipient are SQL operands: "?" for a bound value, or a column of the enclosing
// statement. The expression takes four stamps, in text order: the window's start and end for
// delivery claims, then the same two for supervisor transports (see RelationshipSendsArgs).
//
// Two kinds of row are counted, each by the event (message) it belongs to, and nothing is stored for
// the count:
//
//   - a delivery attempt (one per claim) of a delivery of that relationship to that recipient,
//     by the stamp the claim wrote when the send started, unless it settled as a send that never
//     left (withheld before the send, or the recipient found busy): a claim in flight and an unknown
//     outcome count, a failure before the send woke nobody and does not;
//   - a supervisor transport of a message of that relationship to that recipient, by the stamp of
//     its transport start, and only when the attempt may have gone: it sent something or its
//     retry was not shown safe. That is the repository's own test for "an attempt that pins its
//     message" (supervisor staging and fault notices never re-address a message that has one), so a
//     counted transport can never move to another relationship or recipient afterwards. A
//     transport that started and sent nothing woke nobody.
func RelationshipSendsSQL(relationship, recipient string) string {
	return "((SELECT COUNT(DISTINCT sba.event_id) FROM attempts sba JOIN deliveries sbd ON sbd.event_id = sba.event_id" +
		" WHERE sbd.relationship_id = " + relationship + " AND sbd.recipient_task_id = " + recipient +
		" AND sba.sent_at >= ? AND sba.sent_at < ?" +
		" AND (sba.state IS NULL OR sba.state NOT IN ('withheld_pre_send', 'deferred_busy')))" +
		" + (SELECT COUNT(DISTINCT sbs.message_id) FROM supervisor_attempts sbs JOIN supervisor_messages sbm ON sbm.message_id = sbs.message_id" +
		" WHERE sbm.relationship_id = " + relationship + " AND sbm.recipient_task_id = " + recipient +
		" AND sbs.transport_started_at >= ? AND sbs.transport_started_at < ?" +
		" AND (sbs.send_attempted <> 'no' OR sbs.retry_safe = 0)))"
}

// SendSubject names the one thing a send is about: a delivery's event, or a supervisor message. The zero
// value names nothing, and a send that names nothing is a new one.
type SendSubject struct{ EventID, MessageID string }

// notSendStates is the attempt states that are not a send: the claim settled as a send that never left.
const notSendStates = "('withheld_pre_send', 'deferred_busy')"

// EventCountedSQL is the SQL condition that an event already counts in an hour window: an attempt of it
// is one RelationshipSendsSQL counts. A retry of such an event adds no distinct event to the window, so
// the hour cap does not refuse it (what bounds it is its own attempt cap and the recipient gap). event
// is a SQL operand; the condition takes the window's two stamps.
func EventCountedSQL(event string) string {
	return "EXISTS (SELECT 1 FROM attempts cta WHERE cta.event_id = " + event + " AND cta.sent_at >= ? AND cta.sent_at < ?" +
		" AND (cta.state IS NULL OR cta.state NOT IN " + notSendStates + "))"
}

// EventsCountedSQL is a derived table of the events that count in an hour window (the one column
// event_id): the join form of EventCountedSQL for a read over many candidate rows, which must not
// evaluate a correlated condition for each of them (the scheduler's due list). It takes the window's two
// stamps.
const EventsCountedSQL = "(SELECT DISTINCT cte.event_id AS event_id FROM attempts cte WHERE cte.sent_at >= ? AND cte.sent_at < ?" +
	" AND (cte.state IS NULL OR cte.state NOT IN " + notSendStates + "))"

// MessageCountedSQL is EventCountedSQL for a supervisor message: a transport of it is one
// RelationshipSendsSQL counts. It takes the window's two stamps.
func MessageCountedSQL(message string) string {
	return "EXISTS (SELECT 1 FROM supervisor_attempts cts WHERE cts.message_id = " + message + " AND cts.transport_started_at >= ? AND cts.transport_started_at < ?" +
		" AND (cts.send_attempted <> 'no' OR cts.retry_safe = 0))"
}

// SubjectCounted is whether the subject already counts in the hour window that opens at window. A read
// inside a transaction sees that transaction's writes, so a claim that reads it before inserting its own
// attempt row does not count itself.
func (s *Store) SubjectCounted(ctx context.Context, subject SendSubject, window float64) (bool, error) {
	from, to := SendStamp(window), SendStamp(window+3600)
	var query string
	switch {
	case subject.EventID != "":
		query = "SELECT " + EventCountedSQL("?") + " AS counted"
		return s.counted(ctx, query, subject.EventID, from, to)
	case subject.MessageID != "":
		query = "SELECT " + MessageCountedSQL("?") + " AS counted"
		return s.counted(ctx, query, subject.MessageID, from, to)
	}
	return false, nil
}

func (s *Store) counted(ctx context.Context, query string, args ...any) (bool, error) {
	row, err := s.One(ctx, query, args...)
	if err != nil || row == nil {
		return false, err
	}
	n, _ := row.Get("counted").(int64)
	return n != 0, nil
}

// RelationshipSpentSQL is a derived table of what every relationship has sent to every recipient
// inside an hour window: one row per (relationship_id, recipient_task_id) with its count in spent.
// It takes the four stamps of RelationshipSendsArgs and counts the same two kinds of row as
// RelationshipSendsSQL, once for all pairs. A read over many candidate rows (the scheduler's due
// list) joins this instead of evaluating the correlated expression for each candidate, which
// rescans the relationship's deliveries every time and made a large backlog quadratic. The test
// beside RelationshipSendsSQL holds the two to the same numbers.
const RelationshipSpentSQL = "(SELECT relationship_id, recipient_task_id, SUM(sends) AS spent FROM (" +
	"SELECT sbd.relationship_id AS relationship_id, sbd.recipient_task_id AS recipient_task_id, COUNT(DISTINCT sba.event_id) AS sends" +
	" FROM attempts sba JOIN deliveries sbd ON sbd.event_id = sba.event_id" +
	" WHERE sba.sent_at >= ? AND sba.sent_at < ?" +
	" AND (sba.state IS NULL OR sba.state NOT IN ('withheld_pre_send', 'deferred_busy'))" +
	" GROUP BY sbd.relationship_id, sbd.recipient_task_id" +
	" UNION ALL SELECT sbm.relationship_id, sbm.recipient_task_id, COUNT(DISTINCT sbs.message_id)" +
	" FROM supervisor_attempts sbs JOIN supervisor_messages sbm ON sbm.message_id = sbs.message_id" +
	" WHERE sbs.transport_started_at >= ? AND sbs.transport_started_at < ?" +
	" AND (sbs.send_attempted <> 'no' OR sbs.retry_safe = 0) GROUP BY sbm.relationship_id, sbm.recipient_task_id" +
	") GROUP BY relationship_id, recipient_task_id)"

// RelationshipSendsArgs are the four stamps RelationshipSendsSQL takes for the hour window that
// opens at window.
func RelationshipSendsArgs(window float64) []any {
	from, to := SendStamp(window), SendStamp(window+3600)
	return []any{from, to, from, to}
}

// RelationshipSends is how many sends the relationship has charged to recipient inside the hour
// window that opens at window. A read inside a transaction sees that transaction's writes, so a
// claim that reads it before inserting its own attempt does not count itself.
func (s *Store) RelationshipSends(ctx context.Context, relationship, recipient string, window float64) (int64, error) {
	from, to := SendStamp(window), SendStamp(window+3600)
	row, err := s.One(ctx, "SELECT "+RelationshipSendsSQL("?", "?")+" AS sends", relationship, recipient, from, to, relationship, recipient, from, to)
	if err != nil || row == nil {
		return 0, err
	}
	n, _ := row.Get("sends").(int64)
	return n, nil
}

// OrchestratorParentSQL is the SQL condition that a relationship is of class orchestrator, given the
// operand that names its parent task (a column of the enclosing statement, or "?"): the parent holds the
// live parent binding of a project, active or paused and not replaced - the owner every reader of
// scope_bindings counts as the project's orchestrator (dag.liveParent). The class is read from the
// binding, so it needs no column of its own and follows a handover or a release at once.
func OrchestratorParentSQL(parent string) string {
	return "EXISTS (SELECT 1 FROM scope_bindings orb WHERE orb.task_id = " + parent +
		" AND orb.role = 'parent' AND orb.scope_kind = 'project' AND orb.status IN ('active','paused') AND orb.superseded_by IS NULL)"
}

// OrchestratorTasksSQL is a derived table of the tasks that hold the live parent binding of a project
// (the one column task_id): the join form of OrchestratorParentSQL for a read over many candidate rows,
// which must not evaluate a correlated condition for each of them (the scheduler's due list).
const OrchestratorTasksSQL = "(SELECT DISTINCT task_id FROM scope_bindings" +
	" WHERE role = 'parent' AND scope_kind = 'project' AND status IN ('active','paused') AND superseded_by IS NULL)"

// OrchestratorClassPrefixes are the relationship-id prefixes of the upward supervisor channel that are
// of class orchestrator by their id alone (CRW-1314 design section 3): "run:<id>" is a flow run waking
// its orchestrator and "initiative:<key>" a lead reporting to the management seat above its initiative.
// Neither has a relationships row to read a parent binding from, and each carries every milestone,
// decision and step of everything below it through that one id, so the ordinary hour cap would stop
// it where an orchestrator's relationship is not stopped. Only the hour cap follows the class: the
// recipient's minimum gap between two sends is the same for every class.
var OrchestratorClassPrefixes = []string{"run:", "initiative:"}

// OrchestratorClassID is whether relationship is of class orchestrator by its id alone
// (OrchestratorClassPrefixes).
func OrchestratorClassID(relationship string) bool {
	for _, prefix := range OrchestratorClassPrefixes {
		if strings.HasPrefix(relationship, prefix) {
			return true
		}
	}
	return false
}

// RelationshipIsOrchestrator is whether relationship is of class orchestrator: an id of the upward
// supervisor channel's orchestrator class (OrchestratorClassID), or a relationship whose parent holds
// the live parent binding of a project (OrchestratorParentSQL).
func (s *Store) RelationshipIsOrchestrator(ctx context.Context, relationship string) (bool, error) {
	if OrchestratorClassID(relationship) {
		return true, nil
	}
	row, err := s.One(ctx, "SELECT "+OrchestratorParentSQL("r.parent_task_id")+" AS orchestrator FROM relationships r WHERE r.relationship_id = ?", relationship)
	if err != nil || row == nil {
		return false, err
	}
	n, _ := row.Get("orchestrator").(int64)
	return n != 0, nil
}

// LiveOrchestrators is how many tasks hold the live parent binding of a project: the orchestrators the
// daemon's per-tick budgets scale with.
func (s *Store) LiveOrchestrators(ctx context.Context) (int, error) {
	row, err := s.One(ctx, "SELECT COUNT(DISTINCT task_id) AS live FROM scope_bindings WHERE role = 'parent' AND scope_kind = 'project' AND status IN ('active','paused') AND superseded_by IS NULL")
	if err != nil || row == nil {
		return 0, err
	}
	n, _ := row.Get("live").(int64)
	return int(n), nil
}
