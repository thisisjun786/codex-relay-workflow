package childcleanup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

const (
	parent = testsupport.Parent
	stamp  = "2026-10-03T00:00:00.000000Z"
)

func TestJudge(t *testing.T) {
	merged := Facts{RelationshipID: "r", ParentTaskID: parent, ChildTaskID: "child", Status: "active", State: "merged", Mark: &Mark{Event: "e", Generation: 1, Revision: "h"}}
	with := func(edit func(*Facts)) Facts { f := merged; edit(&f); return f }
	for name, c := range map[string]struct {
		facts  Facts
		actor  string
		reason contract.RefusalReason
	}{
		"merged and live":           {merged, parent, ""},
		"merged and closed":         {with(func(f *Facts) { f.Status, f.State = "archived", "closed" }), parent, ""},
		"plan node integrated":      {with(func(f *Facts) { f.PlanNode, f.Integrated = true, true }), parent, ""},
		"another actor":             {merged, "someone-else", contract.RefusalScopeRoleMismatch},
		"paused":                    {with(func(f *Facts) { f.Status = "paused" }), parent, contract.RefusalRelationshipNotActive},
		"superseded":                {with(func(f *Facts) { f.Superseded = true }), parent, contract.RefusalDispositionConflict},
		"no counting merged mark":   {with(func(f *Facts) { f.Mark, f.State = nil, "verified" }), parent, contract.RefusalDispositionConflict},
		"child serves another work": {with(func(f *Facts) { f.OtherLive = "r2" }), parent, contract.RefusalDispositionConflict},
		"something still owed":      {with(func(f *Facts) { f.Owed = "a queued delivery" }), parent, contract.RefusalDispositionConflict},
		"plan node not integrated":  {with(func(f *Facts) { f.PlanNode = true }), parent, contract.RefusalDispositionConflict},
	} {
		err := Judge(c.facts, c.actor)
		var refused *store.RefusedError
		if (c.reason == "") != (err == nil) || (c.reason != "" && (!errors.As(err, &refused) || refused.Reason != string(c.reason))) {
			t.Errorf("%s: err=%v, want %q", name, err, c.reason)
		}
	}
}

// world is a store with the relay's registry, seeded the way the parent's own commands leave it.
type world struct {
	t   *testing.T
	st  *store.Store
	r   *registry.Registry
	dir string
}

func newWorld(t *testing.T, socket string) *world {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &world{t: t, st: st, dir: dir, r: &registry.Registry{Store: st, Now: func() string { return stamp }, Policy: registry.ResolveRolePolicy(map[string]string{})}}
}

func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

func (w *world) assign(issue, childTask string) string {
	w.t.Helper()
	x, err := w.r.Register(context.Background(), registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: testsupport.Host, Cwd: ns("/parent")},
		Child: registry.Endpoint{TaskID: childTask, HostID: testsupport.Host, Cwd: ns("/work")}, IssueKey: issue, ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{parent},
		DispatchRequestID: "dispatch-" + issue, DispatchTurnID: ns("turn-" + issue)})
	if err != nil {
		w.t.Fatal(err)
	}
	return x.ID
}

func (w *world) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.st.DB.ExecContext(context.Background(), query, args...); err != nil {
		w.t.Fatal(err)
	}
}

// verify gives rid a reviewable head and a verified verdict; merge then records the parent's merged mark on it.
func (w *world) verify(rid string) string {
	w.t.Helper()
	event := "ev-" + rid
	w.exec("INSERT OR IGNORE INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, attempt, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", event, rid, 1, "hash-"+rid, "ready_for_review", "child", 1, "thread", "turn-"+event, "completed", "{}", "final", stamp, stamp)
	w.exec("INSERT OR IGNORE INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?,?,?,?,?,?)", event, `{"executionGeneration":1}`, "verified", nil, "verdict-turn", stamp)
	return event
}

func (w *world) merge(rid string) {
	w.t.Helper()
	if _, err := registry.NewAssignmentView(w.r).Mark(context.Background(), rid, "merged", "landed on dev", "test", w.verify(rid)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) judge(rid string) (Facts, error) {
	w.t.Helper()
	f, err := ReadFacts(context.Background(), w.st, rid)
	if err != nil {
		w.t.Fatal(err)
	}
	return f, Judge(f, parent)
}

func TestReadFactsAndJudgeOverARealStore(t *testing.T) {
	w := newWorld(t, "")
	rid := w.assign("ISSUE-1", "child")
	if _, err := w.judge(rid); err == nil {
		t.Fatal("a child whose work is not even verified was judged clean-able")
	}
	w.verify(rid)
	if f, err := w.judge(rid); err == nil || f.Mark != nil {
		t.Fatalf("verified but not marked merged: facts=%+v err=%v", f, err)
	}
	w.merge(rid)
	if f, err := w.judge(rid); err != nil || f.Mark == nil || f.State != "merged" || f.PlanNode || f.Owed != "" || f.OtherLive != "" {
		t.Fatalf("merged: facts=%+v err=%v", f, err)
	}
	w.exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)", "ev-"+rid, rid, "completion", parent, "thread", "queued", stamp, stamp)
	if f, err := w.judge(rid); err == nil || f.Owed == "" {
		t.Fatalf("a delivery still owed: facts=%+v err=%v", f, err)
	}
	w.exec("DELETE FROM deliveries WHERE relationship_id = ?", rid)
	other := w.assign("ISSUE-2", "child")
	if f, err := w.judge(rid); err == nil || f.OtherLive != other {
		t.Fatalf("the same child serves %s: facts=%+v err=%v", other, f, err)
	}
	// a correction opens a new generation: the merged mark stops counting and the child is not clean-able
	if _, err := w.r.OpenGeneration(context.Background(), rid, "dispatch-correction", "needs_changes_revision", ns("turn-correction")); err != nil {
		t.Fatal(err)
	}
	if f, err := w.judge(rid); err == nil || f.Mark != nil {
		t.Fatalf("a newer generation: facts=%+v err=%v", f, err)
	}
}

// afterListing runs then once, right after the first thread/loaded/list answer.
type afterListing struct {
	Host
	then func()
}

func (h *afterListing) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := h.Host.Call(ctx, method, params)
	if then := h.then; method == "thread/loaded/list" && then != nil {
		h.then = nil
		then()
	}
	return raw, err
}

func TestExecuteCleansAMergedChildAndLeavesTheOthersAlone(t *testing.T) {
	s := newScripted(t, family()...)
	w := newWorld(t, s.srv.SocketPath)
	ctx, merged, open := context.Background(), w.assign("ISSUE-1", "child"), w.assign("ISSUE-9", "other")
	w.merge(merged)
	w.verify(open) // verified, not merged
	if _, err := Execute(ctx, w.st, s.client(t), open, parent, false); err == nil || s.srv.Count("thread/loaded/list") != 0 {
		t.Fatalf("an unmerged child: err=%v, want a refusal before any App Server call", err)
	}
	if result, err := Execute(ctx, w.st, s.client(t), merged, parent, false); err != nil || !result.Complete() || fmt.Sprint(s.archived()) != "[grand sub-2 sub-1 child]" {
		t.Fatalf("err=%v result=%+v archived=%v", err, result, s.archived())
	}
	// cancelled after the threads were read and before the first archive: the second reading refuses and nothing is archived
	w.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", merged)
	s = newScripted(t, family()...)
	again := w.assign("ISSUE-3", "child")
	w.merge(again)
	hook := &afterListing{Host: s.client(t), then: func() { w.exec("UPDATE relationships SET status = 'cancelled' WHERE relationship_id = ?", again) }}
	if _, err := Execute(ctx, w.st, hook, again, parent, false); err == nil || s.srv.Count("thread/loaded/list") == 0 || len(s.archived()) != 0 {
		t.Fatalf("err=%v listings=%d archived=%v, want a refusal after the listing and no archive", err, s.srv.Count("thread/loaded/list"), s.archived())
	}
}

func TestTheCommandIsRegisteredAndAnswersTheReport(t *testing.T) {
	s := newScripted(t, family()...)
	w := newWorld(t, s.srv.SocketPath)
	rid := w.assign("ISSUE-1", "child")
	w.merge(rid)
	call := func(extra ...string) (int, map[string]any) {
		var out, errs bytes.Buffer
		argv := append([]string{"--state", w.dir, "--socket", s.srv.SocketPath, "child-cleanup", "--relationship", rid, "--actor", parent}, extra...)
		code := dispatch.Execute(context.Background(), "codex-session-relay", argv, &out, &errs)
		var answer map[string]any
		if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
			t.Fatalf("exit %d: %q %q", code, out.String(), errs.String())
		}
		return code, answer
	}
	if code, answer := call("--dry-run"); code != 0 || answer["ok"] != true || answer["dry_run"] != true || len(s.archived()) != 0 {
		t.Fatalf("dry run: exit %d %v", code, answer)
	}
	code, answer := call()
	if items, _ := answer["items"].([]any); code != 0 || answer["ok"] != true || answer["complete"] != true || answer["schema"] != "child-cleanup/1" || len(items) != 4 || fmt.Sprint(s.archived()) != "[grand sub-2 sub-1 child]" {
		t.Fatalf("exit %d %v archived=%v", code, answer, s.archived())
	}
	if code, answer = call("--actor", "someone-else"); code != contract.ExitRefused || answer["ok"] == true {
		t.Fatalf("another actor is refused with exit %d: got %d %v", contract.ExitRefused, code, answer)
	}
}
