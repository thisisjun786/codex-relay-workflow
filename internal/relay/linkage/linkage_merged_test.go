package linkage

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-288: a relationship whose work is merged no longer blocks a parent handover, and an operator closes
// the merged ones in bulk. These tests are Go-native: there is no Python twin to replay.

// assign registers one more assignment in the project: its own child, issue and dispatch. Attached ones are
// scoped to the project, the others are the compatibility case with no project.
func (w *world) assign(n int, attach bool) string {
	w.t.Helper()
	x, err := w.r.Register(w.ctx, registry.Registration{
		Parent:            parentEP(parent),
		Child:             registry.Endpoint{TaskID: fmt.Sprintf("01child-%d", n), HostID: host, Cwd: ns(root)},
		IssueKey:          fmt.Sprintf("ISSUE-%d", n),
		ArtifactRoots:     []string{root},
		AllowedRecipients: []string{parent},
		DispatchRequestID: fmt.Sprintf("dispatch-%d", n),
		DispatchTurnID:    ns(fmt.Sprintf("turn-%d", n)),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if attach {
		w.must(w.r.AttachIssue(w.ctx, x.ID, project))
	}
	return x.ID
}

// verifyAt gives rid a reviewable head revision in a generation and a verified verdict on it. It returns the event
// and its revision hash.
func (w *world) verifyAt(rid string, generation int) (string, string) {
	w.t.Helper()
	event, hash := fmt.Sprintf("ev-%s-g%d", rid, generation), fmt.Sprintf("hash-%s-g%d", rid, generation)
	w.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, attempt,"+
		" turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		event, rid, generation, hash, "ready_for_review", "child", 1, "thread", "turn-"+event, "completed", "{}", "final", fakeISO, fakeISO)
	w.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?,?,?,?,?,?)",
		event, fmt.Sprintf(`{"executionGeneration":%d}`, generation), "verified", nil, "verdict-turn", fakeISO)
	return event, hash
}

func (w *world) verify(rid string) string {
	event, _ := w.verifyAt(rid, 1)
	return event
}

// mergeAt is what the parent does after landing the work: verify, then assignment-mark merged through the real mark.
func (w *world) mergeAt(rid string, generation int) (string, string) {
	w.t.Helper()
	event, hash := w.verifyAt(rid, generation)
	if _, err := registry.NewAssignmentView(w.r).Mark(w.ctx, rid, "merged", "landed on dev", "test", event); err != nil {
		w.t.Fatal(err)
	}
	return event, hash
}

func (w *world) merge(rid string) string {
	event, _ := w.mergeAt(rid, 1)
	return event
}

// delivery records a delivery row of event in state.
func (w *world) delivery(rid, event, state string) {
	w.t.Helper()
	w.exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?)", event, rid, "completion", parent, "thread", state, fakeISO, fakeISO)
}

// owe leaves a completion delivery of rid's head event still queued, so something of it is still owed.
func (w *world) owe(rid string) { w.delivery(rid, "ev-"+rid+"-g1", "queued") }

// message records a supervisor message of rid in state.
func (w *world) message(rid, state string) {
	w.t.Helper()
	w.exec("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, project_key, purpose, kind,"+
		" sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"msg-"+rid+"-"+state, "obl-"+rid+"-"+state, "completion", rid, project, "report", "completion", parent, supervisorTask,
		"ev-"+rid+"-g1", "{}", state, fakeISO, fakeISO)
}

// planNode makes rid the execution of a plan node in a generation; accept records an active acceptance of that node.
func (w *world) planNode(rid string, generation int) {
	w.t.Helper()
	w.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind)"+
		" VALUES (?,?,?,?,?,?)", "plan-1", "node-"+rid, rid, generation, "digest", "initial")
}

func (w *world) accept(rid, event, hash string, generation int) {
	w.t.Helper()
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id,"+
		" revision_hash, criteria_set_digest, verdict, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch,"+
		" accepted_at, state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		fmt.Sprintf("acc-%s-g%d", rid, generation), "plan-1", "node-"+rid, "digest", rid, generation, event, hash, "set", "verified", "tier",
		"verdict-turn", "{}", parent, 0, fakeISO, "active")
}

// registerCriteria registers the criteria set digest of rid and makes its verified head the one ruled against it, so the
// merge mark still counts.
func (w *world) registerCriteria(rid, digest string) {
	w.t.Helper()
	w.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, set_digest, recorded_at) VALUES (?,?,?,?,?)",
		rid, "c1", "a criterion", digest, fakeISO)
	w.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, ack_evidence, recorded_at) VALUES (?,?,?,?,?,?)",
		"ev-"+rid+"-g1", digest, "full", "current", "none", fakeISO)
}

func (w *world) statusNow(rid string) string {
	w.t.Helper()
	x, err := w.r.Get(w.ctx, rid)
	if err != nil {
		w.t.Fatal(err)
	}
	return x.Status
}

func (w *world) stateWord(rid string) string {
	w.t.Helper()
	state, err := registry.NewAssignmentView(w.r).State(w.ctx, rid)
	if err != nil {
		w.t.Fatal(err)
	}
	return text(field(state, "state"))
}

func (w *world) scalar(query string, args ...any) any {
	w.t.Helper()
	row, err := w.s.One(w.ctx, query, args...)
	if err != nil || row == nil {
		w.t.Fatalf("%s: %v %v", query, row, err)
	}
	return row[0].Value
}

// dump is every row of the tables, in key order, as one string: what a refusal must leave as it found it.
func (w *world) dump(tables ...string) string {
	w.t.Helper()
	var out strings.Builder
	for _, table := range tables {
		rows, err := w.s.All(w.ctx, "SELECT * FROM "+table+" ORDER BY 1, 2")
		if err != nil {
			w.t.Fatal(err)
		}
		for _, row := range rows {
			fmt.Fprintf(&out, "%s %v\n", table, row)
		}
	}
	return out.String()
}

func objectOf(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

func idsOf(t *testing.T, v any, key string) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	out := []string{}
	for _, item := range list {
		if key == "" {
			out = append(out, item.(string))
		} else {
			out = append(out, objectOf(t, item)[key].(string))
		}
	}
	return out
}

func stringsOf(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := []string{}
		for _, item := range list {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	return nil
}

func sameIDs(a, b []string) bool {
	return reflect.DeepEqual(append([]string{}, a...), append([]string{}, b...))
}

func (w *world) handoverParent(acknowledged []string) (contract.OrderedObject, error) {
	return w.r.Handover(w.ctx, "parent", project, parent, parentEP(otherParent), acknowledged, "the project changed hands", "test")
}

// Test_CRW288_handover_beside_a_merged_relationship: c1.
func Test_CRW288_handover_beside_a_merged_relationship(t *testing.T) {
	t.Run("a merged relationship does not block the handover and is closed by it", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		if got := w.stateWord(rid); got != "merged" {
			t.Fatalf("state %q, want merged", got)
		}
		if got := w.statusNow(rid); got != "active" {
			t.Fatalf("status %q: the mark must not change it", got)
		}
		if out := w.outstanding(sql.NullString{}); len(out) != 0 {
			t.Fatalf("outstanding %v, want none", out)
		}
		binding, err := w.handoverParent(nil)
		if err != nil {
			t.Fatalf("the handover was refused: %v", err)
		}
		if got := w.statusNow(rid); got != "archived" {
			t.Errorf("the relationship it left behind is %q, want archived", got)
		}
		if got := text(field(w.owner("project", project), "taskId")); got != otherParent {
			t.Errorf("project owner %q, want %q", got, otherParent)
		}
		if got := field(binding, "closedMerged"); !sameIDs(stringsOf(got), []string{rid}) {
			t.Errorf("closedMerged %v, want [%s]", got, rid)
		}
		if live := w.scalar("SELECT COUNT(*) FROM relationships WHERE status IN ('active','paused')"); live != int64(0) {
			t.Errorf("%v live relationships left", live)
		}
		if w.owner("issue", "ISSUE-1") != nil {
			t.Errorf("the closed relationship still holds its issue scope")
		}
		// the link was archived where it stood, naming the parent that stepped down, and was not repointed to the incoming one.
		row, err := w.s.One(w.ctx, "SELECT status, upper_task_id FROM scope_links WHERE lower_kind = 'issue' AND lower_key = 'ISSUE-1'")
		if err != nil || row == nil {
			t.Fatalf("the issue link is gone: %v %v", row, err)
		}
		if row[0].Value != "archived" || row[1].Value != parent {
			t.Errorf("issue link %v, want archived under %s (closed before the links moved)", row, parent)
		}
		if got := w.scalar("SELECT COUNT(*) FROM journal WHERE kind = 'status_changed' AND subject = ? AND detail LIKE '%merged_settled%'", rid); got != int64(1) {
			t.Errorf("%v status_changed rows with the reason merged_settled, want 1", got)
		}
		if got := w.scalar("SELECT COUNT(*) FROM journal WHERE kind = 'scope_handover' AND detail LIKE '%closedMerged%' AND detail LIKE ?", "%"+rid+"%"); got != int64(1) {
			t.Errorf("%v scope_handover rows naming the closed relationship, want 1", got)
		}
	})

	t.Run("an unfinished assignment still refuses, and nothing is closed", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		merged, unfinished := w.assign(1, true), w.assign(2, true)
		w.merge(merged)
		out := w.outstanding(sql.NullString{})
		if !sameIDs(out, []string{unfinished}) {
			t.Fatalf("outstanding %v, want [%s]", out, unfinished)
		}
		before := w.dump("relationships", "scope_bindings", "scope_links")
		_, err := w.handoverParent(out)
		if reasonOf(err) != "handover_would_strand" {
			t.Fatalf("got %v, want handover_would_strand", err)
		}
		if detail := err.Error(); !strings.Contains(detail, unfinished) || strings.Contains(detail, merged) {
			t.Errorf("the refusal must name the unfinished row and not the merged one: %s", detail)
		}
		if after := w.dump("relationships", "scope_bindings", "scope_links"); after != before {
			t.Errorf("a refused handover changed the rows:\n%s\nbecame\n%s", before, after)
		}
		if got := w.scalar("SELECT COUNT(*) FROM journal WHERE kind = 'status_changed' AND detail LIKE '%archived%'"); got != int64(0) {
			t.Errorf("%v relationships were archived by a refused handover", got)
		}
	})

	t.Run("a merged relationship with something still owed counts as attached", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			setup func(w *world, rid string)
			holds bool
		}{
			{"a queued delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "queued") }, true},
			{"a sending delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "sending") }, true},
			{"a held_uncertain delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "held_uncertain") }, true},
			{"a delivery that waits in the inbox", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "inbox_only") }, true},
			{"an unsent supervisor message", func(w *world, rid string) { w.message(rid, "queued") }, true},
			{"a dispatched delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "dispatched") }, false},
			{"an acknowledged delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "acknowledged") }, false},
			{"a dispatched supervisor message", func(w *world, rid string) { w.message(rid, "dispatched") }, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				w := newWorld(t)
				w.superviseDefault()
				rid := w.assign(1, true)
				w.merge(rid)
				c.setup(w, rid)
				before := w.dump("relationships", "scope_bindings", "scope_links")
				_, err := w.handoverParent(nil)
				if c.holds {
					if reasonOf(err) != "handover_would_strand" || !strings.Contains(err.Error(), rid) {
						t.Fatalf("got %v, want handover_would_strand naming %s", err, rid)
					}
					if after := w.dump("relationships", "scope_bindings", "scope_links"); after != before {
						t.Errorf("a refused handover changed the rows")
					}
					return
				}
				if err != nil {
					t.Fatalf("refused although nothing is owed: %v", err)
				}
				if got := w.statusNow(rid); got != "archived" {
					t.Errorf("status %q, want archived", got)
				}
			})
		}
	})

	t.Run("a refusal that comes after the strand check closes nothing", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		before := w.dump("relationships", "scope_bindings", "scope_links")
		// the supervisor already holds another role, so its endpoint cannot take the project's parent scope.
		_, err := w.r.Handover(w.ctx, "parent", project, parent, supervisorEP(supervisorTask), nil, "to the wrong task", "test")
		if err == nil || reasonOf(err) == "handover_would_strand" {
			t.Fatalf("got %v, want a refusal from the binding plan", err)
		}
		if after := w.dump("relationships", "scope_bindings", "scope_links"); after != before {
			t.Errorf("a refused handover changed the rows")
		}
		if got := w.statusNow(rid); got != "active" {
			t.Errorf("status %q after the refusal, want active", got)
		}
	})

	t.Run("a settled relationship under a third parent is closed too", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		first := w.assign(1, true)
		third := w.reg(registry.Registration{Parent: bare("01parent-three"), AllowedRecipients: []string{"01parent-three"}, IssueKey: "ISSUE-1", Supersedes: first}, "dispatch-third")
		w.merge(third)
		if _, err := w.handoverParent(nil); err != nil {
			t.Fatalf("refused: %v", err)
		}
		if got := w.statusNow(third); got != "archived" {
			t.Errorf("status %q, want archived", got)
		}
	})

	t.Run("unfinished work already moved under the incoming parent is acknowledged as before", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		moved, merged := w.assign(1, true), w.assign(2, true)
		w.merge(merged)
		successor := w.reg(registry.Registration{Parent: registry.Endpoint{TaskID: otherParent, HostID: host, Cwd: ns("/parent")},
			AllowedRecipients: []string{otherParent}, IssueKey: "ISSUE-1", Supersedes: moved}, "dispatch-moved")
		if _, err := w.handoverParent(w.outstanding(sql.NullString{})); err != nil {
			t.Fatalf("refused: %v", err)
		}
		if got := w.statusNow(merged); got != "archived" {
			t.Errorf("merged %q, want archived", got)
		}
		if got := w.statusNow(successor); got != "active" {
			t.Errorf("the moved unfinished assignment is %q, want active", got)
		}
	})
}

// closeMerged runs the bulk command and returns its exit code and decoded answer (stdout and stderr on a failure).
func (w *world) closeMerged(args ...string) (int, map[string]any) {
	w.t.Helper()
	code, stdout, stderr := runCLI(w.t, w, append([]string{"relationship-close-merged", "--actor", "operator"}, args...)...)
	if code != 0 {
		return code, map[string]any{"stdout": stdout, "stderr": stderr}
	}
	return code, objectOf(w.t, decodeStdout(w.t, stdout))
}

// Test_CRW288_close_merged_command: c2.
func Test_CRW288_close_merged_command(t *testing.T) {
	// a project with one of each kind of live assignment, and one merged assignment with no project.
	build := func(t *testing.T) (w *world, ids map[string]string) {
		w = newWorld(t)
		w.superviseDefault()
		ids = map[string]string{
			"merged": w.assign(1, true), "unfinished": w.assign(2, true), "verified": w.assign(3, true),
			"paused": w.assign(4, true), "owed": w.assign(5, true), "unscoped": w.assign(6, false),
		}
		w.merge(ids["merged"])
		w.verify(ids["verified"])
		w.merge(ids["paused"])
		w.setStatus(ids["paused"], "paused")
		w.merge(ids["owed"])
		w.owe(ids["owed"])
		w.merge(ids["unscoped"])
		return w, ids
	}
	snapshot := func(w *world, ids map[string]string) map[string]string {
		out := map[string]string{}
		for name, rid := range ids {
			row, err := w.s.One(w.ctx, "SELECT status || '|' || updated_at || '|' || (SELECT COUNT(*) FROM journal WHERE subject = ?) AS v FROM relationships WHERE relationship_id = ?", rid, rid)
			if err != nil || row == nil {
				t.Fatal(err)
			}
			out[name] = text(row[0].Value)
		}
		return out
	}
	journalCount := func(w *world) any { return w.scalar("SELECT COUNT(*) FROM journal") }

	t.Run("a dry run reports what it would close and writes nothing", func(t *testing.T) {
		w, ids := build(t)
		before, journaled, links, bindings := snapshot(w, ids), journalCount(w), w.dump("scope_links"), w.dump("scope_bindings")
		code, answer := w.closeMerged("--project", project)
		if code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		if answer["apply"] != false || !sameIDs(idsOf(t, answer["closable"], "relationshipId"), []string{ids["merged"]}) {
			t.Fatalf("answer %v, want exactly %s closable", answer, ids["merged"])
		}
		if len(idsOf(t, answer["closed"], "")) != 0 {
			t.Errorf("a dry run closed %v", answer["closed"])
		}
		kept := idsOf(t, answer["kept"], "relationshipId")
		for _, name := range []string{"unfinished", "verified", "paused", "owed"} {
			if !strings.Contains(strings.Join(kept, " "), ids[name]) {
				t.Errorf("%s (%s) is not reported as kept: %v", name, ids[name], kept)
			}
		}
		if !reflect.DeepEqual(snapshot(w, ids), before) || journalCount(w) != journaled || w.dump("scope_links") != links || w.dump("scope_bindings") != bindings {
			t.Errorf("a dry run changed the store")
		}
	})

	t.Run("apply closes only the merged and settled relationship of the project", func(t *testing.T) {
		w, ids := build(t)
		before := snapshot(w, ids)
		code, answer := w.closeMerged("--project", project, "--apply")
		if code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		if !sameIDs(idsOf(t, answer["closed"], ""), []string{ids["merged"]}) {
			t.Fatalf("closed %v, want [%s]", answer["closed"], ids["merged"])
		}
		after := snapshot(w, ids)
		for name := range ids {
			switch {
			case name == "merged":
				if !strings.HasPrefix(after[name], "archived|") {
					t.Errorf("merged is %q, want archived", after[name])
				}
			case after[name] != before[name]:
				t.Errorf("%s changed from %q to %q (status, updated_at and its journal rows must not move)", name, before[name], after[name])
			}
		}
		if got := w.scalar("SELECT COUNT(*) FROM journal WHERE kind = 'status_changed' AND subject = ? AND detail LIKE '%archived%'", ids["merged"]); got != int64(1) {
			t.Errorf("%v status_changed rows for the closed relationship, want 1", got)
		}
		state, err := registry.NewAssignmentView(w.r).State(w.ctx, ids["merged"])
		if err != nil {
			t.Fatal(err)
		}
		if text(field(state, "state")) != "closed" {
			t.Errorf("a closed merged relationship must read closed: %v", decode(t, state))
		}
		if mark := field(state, "mark"); mark == nil {
			t.Errorf("the closed relationship lost its merge mark: %v", decode(t, state))
		}
		_, again := w.closeMerged("--project", project, "--apply")
		if len(idsOf(t, again["closed"], "")) != 0 {
			t.Errorf("a second apply closed %v", again["closed"])
		}
	})

	t.Run("--all also reaches a merged relationship that has no project", func(t *testing.T) {
		w, ids := build(t)
		code, answer := w.closeMerged("--all", "--apply")
		if code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		want := []string{ids["merged"], ids["unscoped"]}
		got := idsOf(t, answer["closed"], "")
		if len(got) != 2 || !strings.Contains(strings.Join(got, " "), want[0]) || !strings.Contains(strings.Join(got, " "), want[1]) {
			t.Fatalf("closed %v, want %v", got, want)
		}
		for _, name := range []string{"unfinished", "verified", "owed"} {
			if got := w.statusNow(ids[name]); got != "active" {
				t.Errorf("%s is %q, want active", name, got)
			}
		}
		if got := w.statusNow(ids["paused"]); got != "paused" {
			t.Errorf("paused is %q, want paused", got)
		}
	})

	t.Run("the project scope leaves a merged relationship with no project alone", func(t *testing.T) {
		w, ids := build(t)
		if code, answer := w.closeMerged("--project", project, "--apply"); code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		if got := w.statusNow(ids["unscoped"]); got != "active" {
			t.Errorf("unscoped is %q, want active", got)
		}
	})

	t.Run("the scope is named exactly once", func(t *testing.T) {
		w, _ := build(t)
		for _, args := range [][]string{{}, {"--project", project, "--all"}} {
			code, answer := w.closeMerged(args...)
			if code != 2 || !strings.Contains(fmt.Sprint(answer["stderr"]), "--project") || !strings.Contains(fmt.Sprint(answer["stderr"]), "--all") {
				t.Errorf("%v: exit %d, want a usage error naming --project and --all: %v", args, code, answer)
			}
		}
	})

	t.Run("a dry run never creates a store", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-state")
		var stdout, stderr bytes.Buffer
		code := dispatch.Execute(t.Context(), "codex-session-relay",
			[]string{"--state", missing, "relationship-close-merged", "--project", project, "--actor", "operator"}, &stdout, &stderr)
		if code == 0 || !strings.Contains(stdout.String()+stderr.String(), "store_absent") {
			t.Errorf("exit %d, want the store_absent refusal: %s %s", code, stdout.String(), stderr.String())
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Errorf("the dry run created %s (%v)", missing, err)
		}
	})
}

// Test_CRW288_settled_boundaries: what keeps a merged relationship open, whichever command asks.
func Test_CRW288_settled_boundaries(t *testing.T) {
	type setup func(w *world, rid string)
	cases := []struct {
		name     string
		setup    setup
		closable bool
	}{
		{"nothing owed", func(w *world, rid string) {}, true},
		{"a queued delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "queued") }, false},
		{"a deferred_busy delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "deferred_busy") }, false},
		{"a withheld_pre_send delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "withheld_pre_send") }, false},
		{"a sending delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "sending") }, false},
		{"a held_uncertain delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "held_uncertain") }, false},
		{"an inbox_only delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "inbox_only") }, false},
		{"an acknowledged delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "acknowledged") }, true},
		{"a dispatched delivery", func(w *world, rid string) { w.delivery(rid, "ev-"+rid+"-g1", "dispatched") }, true},
		{"a queued supervisor message", func(w *world, rid string) { w.message(rid, "queued") }, false},
		{"a deferred_busy supervisor message", func(w *world, rid string) { w.message(rid, "deferred_busy") }, false},
		{"a withheld_pre_send supervisor message", func(w *world, rid string) { w.message(rid, "withheld_pre_send") }, false},
		{"a sending supervisor message", func(w *world, rid string) { w.message(rid, "sending") }, false},
		{"a held_uncertain supervisor message", func(w *world, rid string) { w.message(rid, "held_uncertain") }, false},
		{"a dispatched supervisor message", func(w *world, rid string) { w.message(rid, "dispatched") }, true},
		{"a read supervisor message", func(w *world, rid string) { w.message(rid, "read") }, true},
		{"a plan node whose result was not accepted", func(w *world, rid string) { w.planNode(rid, 1) }, false},
		{"a plan node accepted for this very head", func(w *world, rid string) {
			w.planNode(rid, 1)
			w.accept(rid, "ev-"+rid+"-g1", "hash-"+rid+"-g1", 1)
		}, true},
		{"a plan node accepted only for an earlier generation", func(w *world, rid string) {
			// the correction generation opens, its new result is merged, and the earlier acceptance is still the active one.
			w.planNode(rid, 1)
			w.accept(rid, "ev-"+rid+"-g1", "hash-"+rid+"-g1", 1)
			if _, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{}); err != nil {
				w.t.Fatal(err)
			}
			w.planNode(rid, 2)
			w.mergeAt(rid, 2)
		}, false},
		{"a plan node accepted under criteria that changed and not revalidated", func(w *world, rid string) {
			w.registerCriteria(rid, "new-set")
			w.planNode(rid, 1)
			w.accept(rid, "ev-"+rid+"-g1", "hash-"+rid+"-g1", 1) // accepted under "set"
		}, false},
		{"a plan node whose acceptance was revalidated under the current criteria", func(w *world, rid string) {
			w.registerCriteria(rid, "new-set")
			w.planNode(rid, 1)
			w.accept(rid, "ev-"+rid+"-g1", "hash-"+rid+"-g1", 1)
			w.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id,"+
				" reval_seq, revalidated_by, revalidated_at) VALUES (?,?,?,?,?,?,?,?)",
				"reval-1", "acc-"+rid+"-g1", "new-set", "ev-"+rid+"-g1", "verdict-turn", 1, parent, fakeISO)
		}, true},
		{"a corrected plan node: an older unaccepted execution and an accepted merged correction", func(w *world, rid string) {
			// generation 1 was ruled needs_changes and never accepted; generation 2 is accepted and merged. Only the current
			// acceptance is active, and the older execution row must not hold the relationship open.
			w.planNode(rid, 1)
			if _, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{}); err != nil {
				w.t.Fatal(err)
			}
			w.planNode(rid, 2)
			event, hash := w.mergeAt(rid, 2)
			w.accept(rid, event, hash, 2)
		}, true},
		{"a new generation opened after the mark", func(w *world, rid string) {
			if _, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{}); err != nil {
				w.t.Fatal(err)
			}
		}, false},
		{"criteria that changed after the verdict", func(w *world, rid string) {
			w.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, ack_evidence, recorded_at) VALUES (?,?,?,?,?,?)",
				"ev-"+rid+"-g1", "old-set", "full", "current", "none", fakeISO)
			w.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, set_digest, recorded_at) VALUES (?,?,?,?,?)",
				rid, "c1", "a criterion", "new-set", fakeISO)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.superviseDefault()
			rid := w.assign(1, true)
			w.merge(rid)
			c.setup(w, rid)
			_, answer := w.closeMerged("--project", project)
			closable := idsOf(t, answer["closable"], "relationshipId")
			if c.closable != sameIDs(closable, []string{rid}) {
				t.Fatalf("closable %v, want closable=%v for %s: %v", closable, c.closable, rid, answer)
			}
			if !c.closable {
				kept := answer["kept"].([]any)
				if len(kept) != 1 {
					t.Fatalf("kept %v, want exactly %s with a reason", kept, rid)
				}
				entry := objectOf(t, kept[0])
				if reason, ok := entry["reason"].(string); entry["relationshipId"] != rid || !ok || reason == "" {
					t.Errorf("the kept relationship must be %s and name a reason: %v", rid, entry)
				}
			}
			_, applied := w.closeMerged("--project", project, "--apply")
			if closed := len(idsOf(t, applied["closed"], "")) == 1; closed != c.closable {
				t.Errorf("apply closed %v, want %v", applied["closed"], c.closable)
			}
			want := map[bool]string{true: "archived", false: "active"}[c.closable]
			if got := w.statusNow(rid); got != want {
				t.Errorf("status %q, want %q", got, want)
			}
		})
	}

	t.Run("a generation opened between the dry run and the apply is honoured", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		_, answer := w.closeMerged("--project", project)
		if !sameIDs(idsOf(t, answer["closable"], "relationshipId"), []string{rid}) {
			t.Fatalf("the dry run found nothing to close: %v", answer)
		}
		if _, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{}); err != nil {
			t.Fatal(err)
		}
		_, applied := w.closeMerged("--project", project, "--apply")
		if len(idsOf(t, applied["closed"], "")) != 0 || w.statusNow(rid) != "active" {
			t.Errorf("the apply closed a relationship whose mark no longer counts: %v", applied)
		}
	})
}

// Test_CRW288_reopen: a closed merged relationship comes back only through relationship-resume.
func Test_CRW288_reopen(t *testing.T) {
	t.Run("resume restores the merged reading and generation-open reopens it", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		if code, answer := w.closeMerged("--project", project, "--apply"); code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		if _, err := w.r.Resume(w.ctx, rid, 1, []string{root}, []string{parent}, "test"); err != nil {
			t.Fatalf("resume refused: %v", err)
		}
		if got := w.stateWord(rid); got != "merged" || w.statusNow(rid) != "active" {
			t.Errorf("after resume: state %q status %q, want merged and active", got, w.statusNow(rid))
		}
		if _, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{}); err != nil {
			t.Errorf("generation-open refused on the resumed relationship: %v", err)
		}
	})

	t.Run("a closed relationship cannot be reopened with generation-open alone", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		if code, answer := w.closeMerged("--project", project, "--apply"); code != 0 {
			t.Fatalf("exit %d: %v", code, answer)
		}
		_, err := w.r.OpenGeneration(w.ctx, rid, "dispatch-fix", "needs_changes_revision", sql.NullString{})
		if reasonOf(err) != "relationship_not_active" {
			t.Errorf("got %v, want relationship_not_active", err)
		}
	})

	t.Run("after a handover closed it, resume is refused and the work is re-registered", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.assign(1, true)
		w.merge(rid)
		if _, err := w.handoverParent(nil); err != nil {
			t.Fatal(err)
		}
		_, err := w.r.Resume(w.ctx, rid, 1, []string{root}, []string{parent}, "test")
		if reasonOf(err) != "foreign_scope" {
			t.Fatalf("resume gave %v, want foreign_scope", err)
		}
	})
}

// Test_CRW288_completion_reading_after_close: linkage-completion keeps reading complete_candidate until the closer runs.
func Test_CRW288_completion_reading_after_close(t *testing.T) {
	w := newWorld(t)
	w.superviseDefault()
	rid := w.assign(1, true)
	w.merge(rid)
	read := func() string {
		code, stdout, stderr := runCLI(t, w, "linkage-completion", "--project", project)
		if code != 0 {
			t.Fatalf("exit %d: %s %s", code, stdout, stderr)
		}
		return text(objectOf(t, decodeStdout(t, stdout))["state"])
	}
	if got := read(); got != "complete_candidate" {
		t.Fatalf("before the close: %q, want complete_candidate", got)
	}
	if code, answer := w.closeMerged("--project", project, "--apply"); code != 0 {
		t.Fatalf("exit %d: %v", code, answer)
	}
	if got := read(); got != "unregistered" {
		t.Fatalf("after the close: %q, want unregistered (nothing is live)", got)
	}
}
