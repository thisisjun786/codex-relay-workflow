package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// omissionObserverGrace is the report grace the fault sweep judges with: none. A settled turn that owes a
// report is filed as soon as the sweep reads it, as the sweep has always done. The supervisor's standing
// readers wait 300 seconds before they call a turn owed; applying that wait here would delay faults and is
// a separate decision (docs/port/refactor-backlog.md).
const omissionObserverGrace = 0.0

// OmissionObserver is the faults.ManagedReadingObserver the relay daemon's fault sweep reads its settled
// managed turns through. It answers with delivery's own omission judgment (delivery.ObserveOmission, which
// classifies with delivery.ClassifyOmission), so a reading says whether a report is still owed: a turn that
// a later admitted turn or its own final receipt has overtaken reads unreported with owed false, and the
// sweep files nothing for it. The package faults cannot call delivery (delivery's tests import faults), so
// the daemon installs this observer, in adapter.daemonFactory, in the field faults.Sweeper.ManagedObserver.
type OmissionObserver struct{}

var _ faults.ManagedReadingObserver = OmissionObserver{}

// Observe reads the exact settled turn r names, without writing the marker tree or the store. The
// selection must be the store.StateSelection the sweep was given.
func (OmissionObserver) Observe(ctx context.Context, r faults.ManagedReadingRequest) (any, error) {
	selection, ok := r.Selection.(store.StateSelection)
	if !ok {
		return nil, errors.New("TypeError: managed readings need a store selection")
	}
	reading := delivery.ObserveOmission(ctx, selection, r.Root, r.Workspace, r.Assignment, r.Session, r.Turn, r.Now, omissionObserverGrace)
	reading = unclaimedOmission(ctx, selection, r, reading, nil)
	return orderedMap(reading), nil
}

// A claim is the child's declaration, not the relay's admission. For this one missing-claim case,
// the attached start and exact admitted settlement can establish what the turn owes without a Stop.
// Other marker failures and every claimed reading retain the delivery reader's answer.
func unclaimedOmission(ctx context.Context, selection store.StateSelection, r faults.ManagedReadingRequest, original delivery.Obj, afterRead func()) delivery.Obj {
	if original.Get("reportingState") != "unmeasured" || original.Get("reason") != "dispatch_uncorrelated" {
		return original
	}
	directory, err := delivery.AssignmentDir(r.Root, r.Workspace, r.Assignment)
	if err != nil {
		return original
	}
	marker, unreadable := delivery.ReadAssignment(ctx, directory)
	if len(unreadable) != 0 || delivery.Malformed(marker) != "" || delivery.CorrelationProblem(marker, r.Session, r.Assignment) != delivery.ClaimAbsent {
		return original
	}
	intent, _ := marker.Get("intent").(delivery.Obj)
	bound, _ := marker.Get("bound").(delivery.Obj)
	relation, _ := marker.Get("relationship").(delivery.Obj)
	samePath := func(value any, expected string) bool {
		path, ok := value.(string)
		if !ok || path == "" || expected == "" {
			return false
		}
		a, ea := store.Realpath(path)
		b, eb := store.Realpath(expected)
		return ea == nil && eb == nil && a == b
	}
	if intent.Get("dispatchRequestIdHash") != r.Assignment || !samePath(intent.Get("workspace"), r.Workspace) || !samePath(intent.Get("dbPath"), selection.DBPath()) || bound.Get("sessionId") != r.Session || bound.Get("taskId") != r.Session {
		return original
	}
	if disposition, readable := delivery.ReadDisposition(ctx, directory, r.Session, r.Turn); !readable || disposition != nil {
		return original
	}
	var rows []unclaimedTurn
	read := func() store.RowsRead {
		rows = nil
		return store.ReadOnlyRows(ctx, selection, unclaimedTurnSQL, []any{r.Session, r.Turn, r.Session, r.Turn, relation.Get("relationshipId"), r.Session}, func(row store.RowScanner) error {
			var turn unclaimedTurn
			var settlements, admissions, events string
			if err := row.Scan(&turn.Request, &turn.Dispatch, &turn.Issue, &turn.Workspace, &turn.Root, &turn.Standby, &turn.Anchor, &turn.Status, &turn.Parent, &turn.Cwd, &turn.Generation, &turn.Requests, &settlements, &admissions, &events); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(settlements), &turn.Settlements); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(admissions), &turn.Admissions); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(events), &turn.Events); err != nil {
				return err
			}
			rows = append(rows, turn)
			return nil
		})
	}
	snapshot := read()
	if !snapshot.Readable || snapshot.Detail != "" || snapshot.Raised != nil || len(rows) != 1 {
		return original
	}
	t := rows[0]
	if t.Requests != 1 || delivery.AssignmentID(t.Dispatch) != r.Assignment || t.Issue != intent.Get("issueKey") || fmt.Sprint(t.Generation) != fmt.Sprint(relation.Get("executionGeneration")) || !samePath(t.Workspace, r.Workspace) || !samePath(t.Cwd, r.Workspace) || !samePath(t.Root, r.Root) || r.Turn == t.Standby || len(t.Settlements) > delivery.OmittedMaxFacts || len(t.Admissions) > delivery.OmittedMaxFacts || len(t.Events) > delivery.OmittedMaxFacts {
		return original
	}
	admitted := r.Turn == t.Anchor
	var own *unclaimedAdmission
	for i := range t.Admissions {
		if t.Admissions[i].Turn == r.Turn {
			admitted, own = true, &t.Admissions[i]
		}
	}
	if !admitted {
		return original
	}
	later := false
	for _, a := range t.Admissions {
		if a.Turn == r.Turn {
			continue
		}
		if own == nil {
			later = true
			continue
		}
		at, before := delivery.Moment(a.At), delivery.Moment(own.At)
		if before != nil && (at == nil || at.After(*before) || at.Equal(*before) && a.Row > own.Row) {
			later = true
		}
	}
	// Test seam for a real claim/declaration arriving while the store was being read.
	if afterRead != nil {
		afterRead()
	}
	afterMarker, problems := delivery.ReadAssignment(ctx, directory)
	disposition, readable := delivery.ReadDisposition(ctx, directory, r.Session, r.Turn)
	if len(problems) != 0 || !reflect.DeepEqual(marker, afterMarker) || !readable || disposition != nil || read() != snapshot || len(rows) != 1 || !reflect.DeepEqual(t, rows[0]) {
		return original
	}
	witness := true
	receipted := false
	reports := []any{}
	for _, e := range t.Events {
		if e["producer"] == "child" && e["stage"] == "final" {
			receipted = true
		}
		if len(t.Settlements) > 0 && e["producer"] == store.ProducerDaemon && e["stage"] == "final" && e["outcome"] == t.Settlements[0].Status && e["status"] == t.Settlements[0].Status {
			reports = append(reports, e)
		}
	}
	verdict := delivery.ClassifyOmission(delivery.OmissionFacts{Witness: &witness, Admission: "admitted", Settlements: t.Settlements, Label: "undeclared_turn_end", ExecutionReport: len(reports) > 0, Receipted: receipted, LaterAdmitted: later, Now: r.Now, Grace: omissionObserverGrace})
	answer := append(delivery.Obj(nil), original...)
	for _, field := range verdict {
		answer = answer.Set(field.Key, field.Value)
	}
	answer = answer.Set("relationshipId", relation.Get("relationshipId")).Set("relationshipStatus", t.Status).Set("executionGeneration", t.Generation).Set("parentTaskId", t.Parent).Set("turnAdmission", "admitted")
	answer = answer.Set("managedRequests", []any{map[string]any{"requestId": t.Request, "state": "attached", "child": r.Session, "standby": t.Standby, "relationship": relation.Get("relationshipId"), "generation": t.Generation, "workspace": t.Workspace, "markerRoot": t.Root, "issue": t.Issue}})
	answer = answer.Set("currentObservation", delivery.Obj{{Key: "label", Value: "undeclared_turn_end"}})
	if len(t.Settlements) > 0 && verdict.Get("reason") != "terminal_conflict" {
		records := make([]any, len(t.Settlements))
		for i, s := range t.Settlements {
			records[i] = delivery.Obj{{Key: "status", Value: s.Status}, {Key: "at", Value: s.At}}
		}
		answer = answer.Set("terminalObservation", delivery.Obj{{Key: "source", Value: "relay_settlement"}, {Key: "records", Value: records}, {Key: "status", Value: t.Settlements[0].Status}})
	}
	if verdict.Get("reason") == "daemon_execution_report" {
		answer = answer.Set("executionReports", reports)
	}
	return answer
}

type unclaimedAdmission struct {
	Turn, At string
	Row      int64
}

type unclaimedTurn struct {
	Request, Dispatch, Issue, Workspace, Root, Standby, Anchor, Status, Parent, Cwd string
	Generation, Requests                                                            int64
	Settlements                                                                     []delivery.OmissionSettlement
	Admissions                                                                      []unclaimedAdmission
	Events                                                                          []map[string]any
}

// Admission, terminal and event evidence come from one statement, bounded like the delivery reader.
// A second identical snapshot keeps a registry change during the marker reads from mixing identities.
const unclaimedTurnSQL = `SELECT m.request_id,m.dispatch_request_id,m.issue_key,m.workspace,m.marker_root,
 COALESCE(m.standby_turn_id,''),COALESCE(g.dispatch_turn_id,''),r.status,r.parent_task_id,
 COALESCE(r.child_cwd,''),g.execution_generation,
 (SELECT count(*) FROM managed_start_requests x WHERE x.dispatch_request_id=g.dispatch_request_id),
 (SELECT json_group_array(json_object('status',s.terminal_status,'at',s.settled_at)) FROM
  (SELECT terminal_status,settled_at FROM assignment_settlements WHERE relationship_id=r.relationship_id AND thread_id=? AND turn_id=? LIMIT 513) s),
 (SELECT json_group_array(json_object('turn',a.turn_id,'at',a.admitted_at,'row',a.rowid)) FROM
  (SELECT turn_id,admitted_at,rowid FROM generation_turns WHERE relationship_id=r.relationship_id
   AND execution_generation=g.execution_generation AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
   AND evidence=('explicit_admission_bound:' || g.dispatch_turn_id) LIMIT 513) a),
 (SELECT json_group_array(json_object('eventId',e.event_id,'outcome',e.outcome,'stage',e.stage,
  'producer',e.producer,'status',e.turn_status)) FROM (SELECT event_id,outcome,stage,producer,turn_status FROM events
  WHERE relationship_id=r.relationship_id AND execution_generation=g.execution_generation
  AND turn_thread_id=? AND turn_id=? LIMIT 513) e)
 FROM managed_start_requests m JOIN relationships r ON r.relationship_id=m.relationship_id
 JOIN generations g ON g.relationship_id=r.relationship_id AND g.execution_generation=r.execution_generation
  AND g.dispatch_request_id=m.dispatch_request_id
 WHERE m.state='attached' AND m.relationship_id=? AND m.child_task_id=? AND r.child_task_id=m.child_task_id
  AND r.issue_key=m.issue_key AND m.execution_generation=r.execution_generation AND r.superseded_by IS NULL LIMIT 2`
