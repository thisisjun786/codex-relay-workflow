package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-299: the batched standing read must raise, for every relationship, the obligations the
// event-by-event read raised, in the order it raised them. The oracle below is that read as it was
// before: FromEvent as written, and the per-relationship loop of Standing.

// legacyFromEvent is FromEvent before CRW-299, kept as written.
func legacyFromEvent(ctx context.Context, c *Channel, eventID string) (*Obligation, error) {
	var relation, revision, outcome, producer, stage string
	var generation int64
	var suppressed sql.NullString
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT relationship_id, execution_generation, revision_hash, outcome, producer, stage, suppressed_reason FROM events WHERE event_id = ?", eventID).Scan(&relation, &generation, &revision, &outcome, &producer, &stage, &suppressed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stage != "final" || suppressed.Valid || (producer != "child" && producer != "daemon_observation") {
		return nil, nil
	}
	report, err := currentReport(ctx, c.Store, eventID)
	if err != nil {
		return nil, err
	}
	kind := ""
	switch report.status {
	case "DONE", "NOOP":
		kind = "completion"
	case "BLOCKED", "BUDGET_EXHAUSTED":
		kind = "blocked"
	case "UNSAFE", "NEEDS_HUMAN":
		kind = "decision_request"
	}
	if kind == "" {
		switch outcome {
		case "ready_for_review":
			kind = "completion"
		case "blocked_needs_input":
			kind = "blocked"
		}
	}
	if kind == "" {
		return nil, nil
	}
	subject := eventID
	if kind != "completion" {
		cause := pyjson.Dumps([]string{firstNonempty(report.status, outcome), report.reason, report.summary}, pyjson.Options{Compact: true, Unicode: true})
		subject = fmt.Sprintf("g%d:%s", generation, hash32(cause)[:16])
	}
	var issue sql.NullString
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT issue_key FROM relationships WHERE relationship_id = ?", relation).Scan(&issue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	detail := report.summary
	if detail == "" {
		detail = "the turn ended " + outcome
	}
	var status any
	if report.status != "" {
		status = report.status
	}
	basis := map[string]any{"table": "events", "eventId": eventID, "outcome": outcome, "cxcStatus": status}
	result := &Obligation{Schema: "supervisor-obligation/1", ID: hash32(kind + "|" + relation + "|" + subject), Kind: kind, RelationID: relation, Subject: subject, Generation: generation, Revision: &revision, Basis: basis, Detail: detail}
	if issue.Valid {
		result.Issue = &issue.String
	}
	return result, nil
}

// legacyProjectObligations is the loop at the heart of Standing before CRW-299, up to the
// obligations each relationship's events raise, in the order of its relationships.
func legacyProjectObligations(ctx context.Context, t *testing.T, c *Channel, project string) map[string][]*Obligation {
	t.Helper()
	ids, err := c.Store.ScopedRelationships(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]*Obligation{}
	for _, id := range ids {
		events, err := c.Store.All(ctx, "SELECT event_id FROM events WHERE relationship_id = ? ORDER BY first_seen_at", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			o, err := legacyFromEvent(ctx, c, event.Get("event_id").(string))
			if err != nil {
				t.Fatal(err)
			}
			if o != nil {
				out[id] = append(out[id], o)
			}
		}
	}
	return out
}

// seedRandomProjects fills one store with the given number of projects (PRJ-R0, PRJ-R1, ...) of
// relationships in every status (some superseded; the archived ones with their issue's owner and
// edge live, archived or absent), a relationship attached to no project, and
// events of every shape the judgment distinguishes: stage, suppression, producer, outcome,
// generation, first-seen time (a few instants, so many tie) and up to three work reports, whose
// newest decides. Each project is read among the others' rows. It returns the project keys.
func seedRandomProjects(t *testing.T, projects int) (*Channel, context.Context, []string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(root, "state", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	rng := rand.New(rand.NewSource(299))
	pick := func(of ...string) string { return of[rng.Intn(len(of))] }
	var keys []string
	err = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := s.Querier(ctx)
		for i := 0; i < projects*10+1; i++ {
			rid, project := fmt.Sprintf("rel-%03d", i), fmt.Sprintf("PRJ-R%d", i/10)
			if i%10 == 0 {
				keys = append(keys, project)
			}
			status := pick("active", "active", "paused", "cancelled", "archived", "archived")
			var successor any
			if status == "archived" && rng.Intn(2) == 0 {
				successor = "rel-next"
			}
			created := worldStamp(rng.Intn(4))
			if _, err := q.ExecContext(ctx, "INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,superseded_by,created_at,updated_at) VALUES (?,?,?,'parent','host','child','host',1,'[]','[]',?,?,?)", rid, "ISS-"+rid, status, successor, created, created); err != nil {
				return err
			}
			if i < projects*10 {
				if err := storeseed.RecordRelationshipScope(ctx, s, rid, project, "t"); err != nil {
					return err
				}
			}
			if status == "archived" {
				issue, child := "ISS-"+rid, "child-"+rid
				if owner := pick("", "active", "archived"); owner != "" {
					if err := storeseed.InsertScopeBinding(ctx, s, store.ScopeBindingsRow{BindingID: "b-" + rid, Role: "child", ScopeKind: "issue", ScopeKey: issue, TaskID: child, HostID: "host", Status: owner, Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
						return err
					}
				}
				if edge := pick("", "active", "paused", "archived"); edge != "" {
					if err := storeseed.InsertScopeLink(ctx, s, store.ScopeLinksRow{LinkID: "l-" + rid, LinkKind: "execution", UpperKind: "project", UpperKey: project, UpperTaskID: "parent", LowerKind: "issue", LowerKey: issue, LowerTaskID: child, Status: edge, Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
						return err
					}
				}
			}
			for k, n := 0, rng.Intn(9); k < n; k++ {
				event := fmt.Sprintf("ev-%s-%d", rid, k)
				seen := worldStamp(rng.Intn(5))
				var suppressed any
				if rng.Intn(7) == 0 {
					suppressed = "failed"
				}
				if _, err := q.ExecContext(ctx, "INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,suppressed_reason,first_seen_at,last_seen_at) VALUES (?,?,?,?,?,?,'child',?,'completed','{}',?,?,?,?)", event, rid, 1+rng.Intn(3), fmt.Sprintf("rev-%d", rng.Intn(3)), pick("ready_for_review", "blocked_needs_input", "failed", "failed", "merge_turn_grant"), pick("child", "child", "child", "daemon_observation", "relay", "parent"), "turn-"+event, pick("final", "final", "final", "staged"), suppressed, seen, seen); err != nil {
					return err
				}
				for sub, subs := 1, rng.Intn(4); sub <= subs; sub++ {
					if _, err := q.ExecContext(ctx, "INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,1,'rev','thisisjun786/codex-relay-workflow',?,?,'v1',?,'merge',?)", event, sub, rid, pick("DONE", "NOOP", "BLOCKED", "BUDGET_EXHAUSTED", "UNSAFE", "NEEDS_HUMAN", "IN_PROGRESS", ""), pick("", "waiting on review", "waiting on CI"), pick("", "the work is done", "stuck on CI"), seen); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Channel{Store: s}, ctx, keys[:projects]
}

// releasedRelationships is the relationships nothing can be addressed for, by the linkage reads
// StoreLinkage.Up starts from rather than by the visit's SQL: archived, and the issue has no live
// owner and no live execution edge above it.
func releasedRelationships(t *testing.T, c *Channel, ctx context.Context) map[string]bool {
	t.Helper()
	rows, err := c.Store.All(ctx, "SELECT relationship_id, issue_key FROM relationships WHERE status='archived'")
	if err != nil {
		t.Fatal(err)
	}
	released := map[string]bool{}
	for _, row := range rows {
		issue := row.Get("issue_key").(string)
		owners, err := c.Store.ScopeOwners(ctx, "issue", issue)
		if err != nil {
			t.Fatal(err)
		}
		edges, err := c.Store.ExecutionLinksAbove(ctx, "issue", issue)
		if err != nil {
			t.Fatal(err)
		}
		if len(owners) == 0 && len(edges) == 0 {
			released[row.Get("relationship_id").(string)] = true
		}
	}
	return released
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCRW299BatchedReadRaisesWhatTheEventByEventReadRaised(t *testing.T) {
	t.Parallel()
	raisedAtLeastOnce := 0
	c, ctx, projects := seedRandomProjects(t, 24)
	// FromEvent itself, which the refactor split into loading and judgment, still answers what it
	// answered for every event of the store.
	events, err := c.Store.All(ctx, "SELECT event_id FROM events ORDER BY event_id")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		id := event.Get("event_id").(string)
		was, err := legacyFromEvent(ctx, c, id)
		if err != nil {
			t.Fatal(err)
		}
		is, err := c.FromEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(is, was) {
			t.Fatalf("FromEvent(%s) = %+v, was %+v", id, is, was)
		}
	}
	released := releasedRelationships(t, c, ctx)
	for _, project := range projects {
		t.Run(project, func(t *testing.T) {
			want := legacyProjectObligations(ctx, t, c, project)
			got, err := c.projectObligations(ctx, project, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("the batched read raised\n%s\nthe event-by-event read raised\n%s", dumpObligations(got), dumpObligations(want))
			}
			raisedAtLeastOnce += len(want)
			// The visit's read is the same read without the released relationships.
			visit, err := c.projectObligations(ctx, project, true)
			if err != nil {
				t.Fatal(err)
			}
			wantVisit := map[string][]*Obligation{}
			for id, raised := range want {
				if !released[id] {
					wantVisit[id] = raised
				}
			}
			if !reflect.DeepEqual(visit, wantVisit) {
				t.Fatalf("the visit's read raised\n%s\nwant\n%s", dumpObligations(visit), dumpObligations(wantVisit))
			}
			checkStandingAnswers(t, c, ctx, project, released)
		})
	}
	if raisedAtLeastOnce == 0 {
		t.Fatal("no seed raised an obligation, so the comparison compared nothing")
	}
	if len(released) == 0 {
		t.Fatal("no relationship was released, so the visit's exclusion was not exercised")
	}
	if archived := archivedCount(t, c, ctx); archived == len(released) {
		t.Fatal("every archived relationship was released, so keeping one whose issue was taken again was not exercised")
	}
}

func archivedCount(t *testing.T, c *Channel, ctx context.Context) int {
	t.Helper()
	var n int
	if err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT count(*) FROM relationships WHERE status='archived'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// One statement reads a project's events, whatever the number of its relationships, those that can
// report and those released alike, and the events they hold: the read does not go by relationship.
func TestCRW299ProjectEventsAreReadInOneStatement(t *testing.T) {
	t.Parallel()
	for _, relationships := range []int{2, 20} {
		w := newStandingWorld(t, relationships, 3)
		for _, rid := range w.rels[:relationships/2] {
			w.archive(rid)
		}
		for _, visit := range []bool{false, true} {
			if n := w.statementsOf(func() {
				if _, err := w.c.projectObligations(w.ctx, worldProject, visit); err != nil {
					t.Fatal(err)
				}
			}); n != 1 {
				t.Fatalf("reading the events of %d relationships (visit %v) took %d statements, want 1", relationships, visit, n)
			}
		}
	}
}

// checkStandingAnswers: Standing's whole answer lists every relationship and every obligation, and
// the visit's answer is the whole one less the obligations of released relationships.
func checkStandingAnswers(t *testing.T, c *Channel, ctx context.Context, project string, released map[string]bool) {
	t.Helper()
	whole, err := c.Standing(ctx, project, nil)
	if err != nil {
		t.Fatal(err)
	}
	visit, err := c.standing(ctx, project, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	wholeJSON, visitJSON := asMap(t, whole), asMap(t, visit)
	kept := []any{}
	for _, entry := range wholeJSON["standing"].([]any) {
		if !released[entry.(map[string]any)["relationId"].(string)] {
			kept = append(kept, entry)
		}
	}
	if !reflect.DeepEqual(visitJSON["relations"], wholeJSON["relations"]) {
		t.Fatal("the visit's answer stopped listing the released relationships")
	}
	wholeJSON["standing"] = kept
	if !reflect.DeepEqual(wholeJSON, visitJSON) {
		t.Fatalf("the visit's answer\n%v\nis not the whole answer less released relationships\n%v", visitJSON, wholeJSON)
	}
}

func dumpObligations(m map[string][]*Obligation) string {
	var b strings.Builder
	for id, list := range m {
		for _, o := range list {
			fmt.Fprintf(&b, "  %s: %s %s %v\n", id, o.Kind, o.Subject, o.Basis["eventId"])
		}
	}
	return b.String()
}

// The statement is held to its plan, as the delivery hot queries are: the project's relationships
// first, then each one's events through events_relationship. Planned freely SQLite walks
// events_stage, which is every final event of every project, and the visit would cost the store.
func TestCRW299ProjectEventsReadIsPlannedFromTheProject(t *testing.T) {
	t.Parallel()
	c, ctx, projects := seedRandomProjects(t, 1)
	for name, query := range map[string]string{"whole": projectEventsSQL + eventsOrder, "visit": visitEventsSQL + eventsOrder} {
		rows, err := c.Store.All(ctx, "EXPLAIN QUERY PLAN "+query, projects[0])
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for _, row := range rows {
			plan = append(plan, row.Get("detail").(string))
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH e USING INDEX events_relationship") || strings.Contains(joined, "events_stage") || strings.Contains(joined, "SCAN e") || strings.Contains(joined, "SCAN r") {
			t.Fatalf("%s: the plan does not reach events through events_relationship from the project:\n%s", name, joined)
		}
		if name == "visit" && (!strings.Contains(joined, "SEARCH b USING INDEX scope_bindings_scope") || !strings.Contains(joined, "SEARCH l USING INDEX scope_links_lower") || strings.Contains(joined, "SCAN b") || strings.Contains(joined, "SCAN l")) {
			t.Fatalf("visit: the owner and edge probes are not index searches:\n%s", joined)
		}
		if first := plan[0]; !strings.HasPrefix(first, "SCAN s") {
			t.Fatalf("%s: the plan does not start from the project's relationships:\n%s", name, joined)
		}
	}
}
