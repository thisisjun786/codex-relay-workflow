package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func devinFixture(t *testing.T) *stageFixture {
	t.Helper()
	f := fixture24(t)
	for _, table := range []string{"work_reports", "events"} {
		if _, err := f.s.DB.ExecContext(f.ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func copyReading(t *testing.T, reading map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(reading)
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func checkDevinStage(t *testing.T, f *stageFixture, project string, readings []map[string]any) map[string]any {
	t.Helper()
	answer, err := f.c.StageStandingWithObservations(context.Background(), project, readings, f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
	return answer
}

func addForeignRelationship(t *testing.T, f *stageFixture) {
	t.Helper()
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-foreign','BETA-1','active','parent-b','host','child-b','host',1,'[]','[]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.RecordRelationshipScope(f.ctx, f.s, "rel-foreign", "BETA", "t"); err != nil {
		t.Fatal(err)
	}
}

func Test24DevinSelectorConflictMatchesTheGolden(t *testing.T) {
	f := devinFixture(t)
	first := omissionReading24(f)
	second := copyReading(t, first)
	second["selectors"].(map[string]any)["workspace"] = "/new"
	answer := checkDevinStage(t, f, "PRJ-1", []map[string]any{first, second})
	want := []any{map[string]any{"obligationId": ObservationObligation(first).ID, "kind": "unreported", "reason": "contradictory_observation", "detail": "2 readings of this omission disagree about what it is or where it can be read, and a report carries exactly one; nothing was staged for it"}}
	if !reflect.DeepEqual(answer["refused"], want) || len(answer["staged"].([]any)) != 0 {
		t.Fatalf("answer %#v", answer)
	}
}

func Test24DevinForeignRelationshipMatchesTheGolden(t *testing.T) {
	f := devinFixture(t)
	addForeignRelationship(t, f)
	foreign := omissionReading24(f)
	foreign["relationshipId"] = "rel-foreign"
	foreign["selectors"].(map[string]any)["turn"] = "turn-foreign"
	answer := checkDevinStage(t, f, "PRJ-1", []map[string]any{foreign})
	if len(answer["staged"].([]any)) != 0 || len(answer["refused"].([]any)) != 0 || len(answer["gaps"].([]any)) != 0 {
		t.Fatalf("foreign reading was not ignored: %#v", answer)
	}
}

type devinReadbackCase struct {
	name      string
	uncertain bool
	named     string
	holder    string
	turns     map[string]float64
}

// devinGoldenTokens substitutes the delivery tokens Go's attempts drew at random, in the order the
// attempts were made, so a golden names none of them.
func devinGoldenTokens(t *testing.T, f *stageFixture) []golden.Option {
	t.Helper()
	rows, err := f.s.DB.QueryContext(f.ctx, "SELECT delivery_token FROM supervisor_attempts ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var options []golden.Option
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		options = append(options, golden.Substitute(token, fmt.Sprintf("<delivery token %d>", len(options)+1)))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return options
}

func checkDevinReadback(t *testing.T, tc devinReadbackCase) map[string]any {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	h := &sendHost{status: "idle"}
	if tc.uncertain {
		h.outcome = "unknown"
	}
	if _, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000); err != nil {
		t.Fatal(err)
	}
	h.turns = tc.turns
	h.items = map[string]string{tc.holder: h.sends[0]}
	answer, err := f.c.ReadBack(f.ctx, id, tc.named, Proof(id, tc.named), "", h, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	opts := append(devinGoldenTokens(t, f), fixtureGolden(t, f.root)...)
	checkSupervisorValues(t, []any{answer}, opts...)
	checkSupervisorTables(t, f.s, opts...)
	return answer
}

func Test24DevinReadbackChronologyMatchesTheGolden(t *testing.T) {
	cases := []devinReadbackCase{
		{name: "F1 unrelated holder predates transport", named: "turn-read", holder: "turn-old", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-old": 1_699_999_990}},
		{name: "neighbor token in named turn", named: "turn-read", holder: "turn-read", turns: map[string]float64{"turn-read": 1_700_000_010}},
		{name: "neighbor token in transport-reported steered turn", named: "turn-read", holder: "turn-supervisor-1", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-supervisor-1": 1_699_999_990}},
		{name: "neighbor holder time unavailable", named: "turn-read", holder: "turn-missing", turns: map[string]float64{"turn-read": 1_700_000_010}},
		{name: "F2 uncertain holder 0.5ms before transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_699_999_999.9995}},
		{name: "neighbor uncertain holder exactly at transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_700_000_000}},
		{name: "neighbor uncertain holder 1ms before transport", uncertain: true, named: "turn-read", holder: "turn-holder", turns: map[string]float64{"turn-read": 1_700_000_010, "turn-holder": 1_699_999_999.999}},
		{name: "neighbor uncertain holder missing", uncertain: true, named: "turn-read", holder: "turn-missing", turns: map[string]float64{"turn-read": 1_700_000_010}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkDevinReadback(t, tc) })
	}
}

func Test24DevinStageStandingOrderMatchesTheGolden(t *testing.T) {
	f := fixture24(t)
	first := omissionReading24(f)
	first["executionGeneration"] = 1
	first["selectors"].(map[string]any)["turn"] = "turn-omission-a"
	second := copyReading(t, first)
	second["executionGeneration"] = 1
	second["selectors"].(map[string]any)["turn"] = "turn-omission-c"
	answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{first, second}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)

	var firstBytes []byte
	for run := 0; run < 20; run++ {
		runFixture := fixture24(t)
		a := omissionReading24(runFixture)
		a["executionGeneration"] = 1
		a["selectors"].(map[string]any)["turn"] = "turn-omission-a"
		c := copyReading(t, a)
		c["executionGeneration"] = 1
		c["selectors"].(map[string]any)["turn"] = "turn-omission-c"
		answer, err := runFixture.c.StageStandingWithObservations(runFixture.ctx, "PRJ-1", []map[string]any{a, c}, runFixture.at)
		if err != nil {
			t.Fatal(err)
		}
		var journal []string
		rows, err := runFixture.s.DB.QueryContext(runFixture.ctx, "SELECT subject FROM journal WHERE kind='supervisor_report' ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var subject string
			if err := rows.Scan(&subject); err != nil {
				t.Fatal(err)
			}
			journal = append(journal, subject)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		bytes, _ := json.Marshal(map[string]any{"answer": answer, "journal": journal})
		bytes = []byte(strings.ReplaceAll(string(bytes), runFixture.root, "<ROOT>"))
		if run == 0 {
			firstBytes = bytes
		} else if string(bytes) != string(firstBytes) {
			t.Errorf("run %d is nondeterministic\nfirst: %s\nnow: %s", run, firstBytes, bytes)
		}
	}
}

func moveDevinSupervisor(t *testing.T, f *stageFixture) {
	t.Helper()
	if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "b-successor", f.at); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(f.ctx, f.s, store.ScopeBindingsRow{BindingID: "b-successor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "successor", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t2", UpdatedAt: "t2"}); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.RepointScopeLink(f.ctx, f.s, "lnk-project", "active", "parent", "successor", f.at); err != nil {
		t.Fatal(err)
	}
}

func checkDevinRestage(t *testing.T, moved, changed bool) map[string]any {
	t.Helper()
	f := fixture24(t)
	original := omissionReading24(f)
	original["executionGeneration"] = 1
	o := ObservationObligation(original)
	if _, err := f.c.StageWithReading(f.ctx, *o, original, "", f.at); err != nil {
		t.Fatal(err)
	}
	if moved {
		moveDevinSupervisor(t, f)
	}
	incoming := copyReading(t, original)
	incoming["executionGeneration"] = 1
	if changed {
		incoming["selectors"].(map[string]any)["markerRoot"] = "/markers/b"
	}
	result, err := f.c.StageWithReading(f.ctx, *o, incoming, "", f.at)
	var answer map[string]any
	if err != nil {
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatal(err)
		}
		answer = map[string]any{"error": "DeliveryRefused", "reason": refusal.Reason, "detail": refusal.Detail}
	} else {
		answer = map[string]any{"ok": result}
	}
	checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
	return answer
}

func Test24DevinRepeatedReadingObservedAtMatchesTheGolden(t *testing.T) {
	f := fixture24(t)
	original := omissionReading24(f)
	original["executionGeneration"] = 1
	o := ObservationObligation(original)
	if _, err := f.c.StageWithReading(f.ctx, *o, original, "", f.at); err != nil {
		t.Fatal(err)
	}
	incoming := copyReading(t, original)
	incoming["observedAt"] = "2023-11-14T22:14:20.000000+00:00"
	result, err := f.c.StageWithReading(f.ctx, *o, incoming, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	checkSupervisorValues(t, []any{map[string]any{"ok": result}}, fixtureGolden(t, f.root)...)
	checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
}

func Test24DevinFrozenReadingCheckedBeforeReaddressMatchesTheGolden(t *testing.T) {
	for _, tc := range []struct {
		name           string
		moved, changed bool
	}{{"moved identical reading", true, false}, {"moved different reading", true, true}, {"unmoved different reading", false, true}} {
		t.Run(tc.name, func(t *testing.T) { checkDevinRestage(t, tc.moved, tc.changed) })
	}
}

func Test24DevinStageUnsentFiltersEveryStandingObligationLikeTheGolden(t *testing.T) {
	for _, tc := range []struct {
		name, state, hold string
		confirmed         bool
	}{{"dispatched unconfirmed", "dispatched", "", false}, {"sending", "sending", "", false}, {"dispatched confirmed", "dispatched", "", true}, {"held", "queued", "hierarchy_unresolved", false}, {"failed retryable", "deferred_busy", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, reading, project := snapshotFixture(t, "plain")
			relation := reading["relationshipId"].(string)
			if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-f6',?,1,'abc123456789abcdef','ready_for_review','child','child','turn-f6','completed','{}',?,?)`, relation, f.at, f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-f6',1,?,1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','done','merge',?)`, relation, f.at); err != nil {
				t.Fatal(err)
			}
			o := ObservationObligation(reading)
			if _, err := f.c.StageWithReading(f.ctx, *o, reading, "", f.at); err != nil {
				t.Fatal(err)
			}
			event, err := f.c.FromEvent(f.ctx, "event-f6")
			if err != nil || event == nil {
				t.Fatalf("event: %v", err)
			}
			if _, err := f.c.Stage(f.ctx, *event, "", f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state=?,hold_reason=?", tc.state, nullString(tc.hold)); err != nil {
				t.Fatal(err)
			}
			if tc.confirmed {
				for _, query := range []string{"INSERT INTO sync_targets(relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')", "INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) SELECT 'sync-1',relationship_id,issue_key,'coordination_document','doc-1','verdict','event-f6','digest','summary','confirmed','t','t' FROM relationships LIMIT 1"} {
					if _, err := f.s.DB.ExecContext(f.ctx, query); err != nil {
						t.Fatal(err)
					}
				}
			}
			answer, err := f.c.stageUnsentWithReadings(f.ctx, project, f.at, []map[string]any{reading})
			if err != nil {
				t.Fatal(err)
			}
			checkSupervisorValues(t, []any{answer}, fixtureGolden(t, f.root)...)
			checkSupervisorTables(t, f.s, fixtureGolden(t, f.root)...)
		})
	}
}

func Test24DevinObservationNeighboursMatchTheGolden(t *testing.T) {
	t.Run("same selectors and irrelevant metadata", func(t *testing.T) {
		f := devinFixture(t)
		first := omissionReading24(f)
		second := copyReading(t, first)
		second["irrelevant"] = map[string]any{"changed": true}
		answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{first, second}, f.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(answer["staged"].([]any)) != 1 || len(answer["refused"].([]any)) != 0 {
			t.Fatalf("identical reading did not converge: %#v", answer)
		}
	})
	t.Run("malformed foreign reading", func(t *testing.T) {
		f := devinFixture(t)
		addForeignRelationship(t, f)
		foreign := omissionReading24(f)
		foreign["relationshipId"] = "rel-foreign"
		foreign["selectors"] = []any{"malformed"}
		answer := checkDevinStage(t, f, "PRJ-1", []map[string]any{foreign})
		if len(answer["staged"].([]any)) != 0 || len(answer["refused"].([]any)) != 0 || len(answer["gaps"].([]any)) != 0 {
			t.Fatalf("malformed foreign reading was not ignored: %#v", answer)
		}
	})
	t.Run("own and foreign readings", func(t *testing.T) {
		f := devinFixture(t)
		addForeignRelationship(t, f)
		own := omissionReading24(f)
		foreign := copyReading(t, own)
		foreign["relationshipId"] = "rel-foreign"
		foreign["selectors"].(map[string]any)["turn"] = "turn-foreign"
		answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{foreign, own}, f.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(answer["staged"].([]any)) != 1 || len(answer["refused"].([]any)) != 0 {
			t.Fatalf("mixed readings: %#v", answer)
		}
	})
}
