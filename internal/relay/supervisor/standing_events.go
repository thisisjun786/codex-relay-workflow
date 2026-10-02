package supervisor

import (
	"context"
	"database/sql"
)

// projectEventsSQL is what FromEvent reads one event at a time, read for a whole project in one
// statement: every event of the project's relationships that can raise an obligation
// (eventRaises, selected here so the rest are never read), with the newest work report of each
// and the issue its relationship works, oldest first.
//
// The relay never runs ANALYZE, so SQLite plans from its defaults, in which an equality on an
// index looks like ten rows. Planned freely this statement walks events_stage, every final event
// of every project, and looks each one's relationship up. The CROSS JOINs keep the order written
// (the project's relationships, then the events of each through events_relationship) and the
// unary plus keeps stage out of the index choice; the plan test holds both.
//
// An event's turn order is first_seen_at, then its generation and row, which is the order the
// events of a relationship came in by when they were read relationship by relationship.
const (
	projectEventsSQL = "SELECT e.relationship_id, e.event_id, e.execution_generation, e.revision_hash, e.outcome, e.producer, e.stage, e.suppressed_reason, w.cxc_status, w.cxc_reason, w.summary, r.issue_key" +
		" FROM relationship_scope s CROSS JOIN relationships r ON r.relationship_id = s.relationship_id" +
		" CROSS JOIN events e ON e.relationship_id = r.relationship_id" +
		" LEFT JOIN work_reports w ON w.event_id = e.event_id AND w.submission_no = (SELECT MAX(x.submission_no) FROM work_reports x WHERE x.event_id = e.event_id)" +
		" WHERE s.project_key = ? AND +e.stage = 'final' AND e.suppressed_reason IS NULL AND e.producer IN ('child', 'daemon_observation')"
	// visitEventsSQL leaves out the events of released relationships (see standing): archived ones
	// whose issue has neither a live owner nor a live execution edge above it, the two things
	// StoreLinkage.Up starts from, so nothing can be addressed for them. An archived relationship
	// whose issue was taken again (a successor, or a new registration for the issue) is not released.
	visitEventsSQL = projectEventsSQL + " AND (r.status <> 'archived'" +
		" OR EXISTS (SELECT 1 FROM scope_bindings b WHERE b.scope_kind = 'issue' AND b.scope_key = r.issue_key AND b.status IN ('active', 'paused') AND b.superseded_by IS NULL)" +
		" OR EXISTS (SELECT 1 FROM scope_links l WHERE l.lower_kind = 'issue' AND l.lower_key = r.issue_key AND l.link_kind = 'execution' AND l.status IN ('active', 'paused') AND l.superseded_by IS NULL))"
	eventsOrder = " ORDER BY e.first_seen_at, e.execution_generation, e.rowid"
)

// projectObligations is the obligations the events of a project's relationships raise, by
// relationship, each relationship's in the order its events came in. With visit it leaves out the
// events of released relationships. It is one statement for the project, whatever the number of its
// relationships and events.
func (c *Channel) projectObligations(ctx context.Context, project string, visit bool) (_ map[string][]*Obligation, err error) {
	query := projectEventsSQL
	if visit {
		query = visitEventsSQL
	}
	rows, err := c.Store.Q(ctx).QueryContext(ctx, query+eventsOrder, project)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	raised := map[string][]*Obligation{}
	for rows.Next() {
		var f eventFacts
		var status, reason, summary sql.NullString
		if err = rows.Scan(&f.relation, &f.eventID, &f.generation, &f.revision, &f.outcome, &f.producer, &f.stage, &f.suppressed, &status, &reason, &summary, &f.issue); err != nil {
			return nil, err
		}
		f.report = workReportFact{status: status.String, reason: reason.String, summary: summary.String}
		if o := obligationFrom(f); o != nil {
			raised[f.relation] = append(raised[f.relation], o)
		}
	}
	return raised, rows.Err()
}
