package store

import (
	"context"
	"math"
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
// one recipient inside an hour window. relationship and recipient are SQL operands: "?" for a
// bound value, or a column of the enclosing statement. The expression takes four stamps, in text
// order: the window's start and end for delivery claims, then the same two for supervisor
// transports (see RelationshipSendsArgs).
//
// Two kinds of row are counted, and nothing is stored for the count:
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
	return "((SELECT COUNT(*) FROM attempts sba JOIN deliveries sbd ON sbd.event_id = sba.event_id" +
		" WHERE sbd.relationship_id = " + relationship + " AND sbd.recipient_task_id = " + recipient +
		" AND sba.sent_at >= ? AND sba.sent_at < ?" +
		" AND (sba.state IS NULL OR sba.state NOT IN ('withheld_pre_send', 'deferred_busy')))" +
		" + (SELECT COUNT(*) FROM supervisor_attempts sbs JOIN supervisor_messages sbm ON sbm.message_id = sbs.message_id" +
		" WHERE sbm.relationship_id = " + relationship + " AND sbm.recipient_task_id = " + recipient +
		" AND sbs.transport_started_at >= ? AND sbs.transport_started_at < ?" +
		" AND (sbs.send_attempted <> 'no' OR sbs.retry_safe = 0)))"
}

// RelationshipSpentSQL is a derived table of what every relationship has sent to every recipient
// inside an hour window: one row per (relationship_id, recipient_task_id) with its count in spent.
// It takes the four stamps of RelationshipSendsArgs and counts the same two kinds of row as
// RelationshipSendsSQL, once for all pairs. A read over many candidate rows (the scheduler's due
// list) joins this instead of evaluating the correlated expression for each candidate, which
// rescans the relationship's deliveries every time and made a large backlog quadratic. The test
// beside RelationshipSendsSQL holds the two to the same numbers.
const RelationshipSpentSQL = "(SELECT relationship_id, recipient_task_id, SUM(sends) AS spent FROM (" +
	"SELECT sbd.relationship_id AS relationship_id, sbd.recipient_task_id AS recipient_task_id, COUNT(*) AS sends" +
	" FROM attempts sba JOIN deliveries sbd ON sbd.event_id = sba.event_id" +
	" WHERE sba.sent_at >= ? AND sba.sent_at < ?" +
	" AND (sba.state IS NULL OR sba.state NOT IN ('withheld_pre_send', 'deferred_busy'))" +
	" GROUP BY sbd.relationship_id, sbd.recipient_task_id" +
	" UNION ALL SELECT sbm.relationship_id, sbm.recipient_task_id, COUNT(*)" +
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
