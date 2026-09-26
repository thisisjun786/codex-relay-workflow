package mergeturn_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

type wakeHost struct {
	clock      *delivery.FakeClock
	sends      int
	turns      map[string][]delivery.TurnInfo
	items      map[string][]string
	operations map[string]delivery.Obj
}

func newWakeHost(clock *delivery.FakeClock) *wakeHost {
	return &wakeHost{clock: clock, turns: map[string][]delivery.TurnInfo{}, items: map[string][]string{}, operations: map[string]delivery.Obj{}}
}
func (*wakeHost) ReadThread(string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &yes}, nil
}
func (*wakeHost) IsArchived(string, any) (*bool, error) { no := false; return &no, nil }
func (*wakeHost) ReadGoalStatus(string) (any, error)    { return nil, nil }
func (h *wakeHost) ListTurnIDs(thread string, _ int) ([]string, error) {
	ids := []string{}
	for _, v := range h.turns[thread] {
		ids = append(ids, v.TurnID)
	}
	return ids, nil
}
func (h *wakeHost) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	for _, v := range h.turns[thread] {
		if v.TurnID == turn {
			return &v, nil
		}
	}
	return nil, nil
}
func (h *wakeHost) SendMessage(requestID, thread, message string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends++
	id := fmt.Sprintf("turn-%s-%d", thread, h.sends)
	now := h.clock.Now()
	h.turns[thread] = append(h.turns[thread], delivery.TurnInfo{TurnID: id, Status: "inProgress", StartedAt: &now})
	h.items[thread] = append(h.items[thread], message)
	out := delivery.Obj{{Key: "requestId", Value: requestID}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "accepted"}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "turnId", Value: id}}
	h.operations[requestID] = out
	return out, nil
}
func (h *wakeHost) GetOperation(id string) (delivery.Obj, error) { return h.operations[id], nil }
func (h *wakeHost) FindToken(thread, token string, _ int, _ bool) (delivery.TokenScan, error) {
	for _, item := range h.items[thread] {
		if strings.Contains(item, token) {
			return delivery.TokenScan{Found: true}, nil
		}
	}
	return delivery.TokenScan{Exhausted: true}, nil
}
func (h *wakeHost) FindDispatchedTurn(thread, turn string, _ float64) (delivery.TurnPresence, error) {
	for _, v := range h.turns[thread] {
		if v.TurnID == turn {
			return delivery.TurnPresence{Finding: delivery.TurnPresent, Turn: &v}, nil
		}
	}
	return delivery.TurnPresence{Finding: delivery.TurnAbsent}, nil
}
func (h *wakeHost) FindTokenSince(thread, token string, _ []string, _ int) (delivery.TokenScan, error) {
	return h.FindToken(thread, token, 0, true)
}
func (h *wakeHost) FindTokenInTurn(thread, token, turn string, _ int) (delivery.TokenScan, error) {
	return h.FindToken(thread, token, 0, true)
}
func (*wakeHost) RecipientFingerprint(string) (string, error) { return strings.Repeat("0", 64), nil }

type wakeParity struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	clock *delivery.FakeClock
	d     *delivery.Service
	m     *mergeturn.Service
	host  *wakeHost
	out   []map[string]any
}

func wakeValue(t *testing.T, v any) any {
	t.Helper()
	var convert func(any) any
	convert = func(x any) any {
		switch y := x.(type) {
		case contract.OrderedObject:
			m := map[string]any{}
			for _, f := range y {
				m[f.Key] = convert(f.Value)
			}
			return m
		case []any:
			a := make([]any, len(y))
			for i, z := range y {
				a[i] = convert(z)
			}
			return a
		case map[string]any:
			m := map[string]any{}
			for k, z := range y {
				m[k] = convert(z)
			}
			return m
		}
		return x
	}
	v = convert(v)
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	var value any
	if e = json.Unmarshal(raw, &value); e != nil {
		t.Fatal(e)
	}
	return value
}
func (w *wakeParity) step(v any, err error) any {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	w.out = append(w.out, map[string]any{"ok": wakeValue(w.t, v)})
	return v
}
func (w *wakeParity) must(v map[string]any, err error) map[string]any {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}
func (w *wakeParity) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.store.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatal(err)
	}
}
func (w *wakeParity) compare(name string) {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "python_mergeturn.json"))
	if err != nil {
		w.t.Fatal(err)
	}
	var all map[string][]map[string]any
	if err = json.Unmarshal(raw, &all); err != nil {
		w.t.Fatal(err)
	}
	want, ok := all[name]
	if !ok {
		w.t.Fatal("missing oracle", name)
	}
	if len(w.out) != len(want) {
		w.t.Fatalf("%s steps: Go %d Python %d", name, len(w.out), len(want))
	}
	encoded, err := json.Marshal(w.out)
	if err != nil {
		w.t.Fatal(err)
	}
	names := map[string]string{}
	clean := regexp.MustCompile(`tgt-[0-9a-f]{32}`).ReplaceAllStringFunc(string(encoded), func(key string) string {
		if name, ok := names[key]; ok {
			return name
		}
		name := fmt.Sprintf("<target-key-%d>", len(names)+1)
		names[key] = name
		return name
	})
	var steps []map[string]any
	if err := json.Unmarshal([]byte(clean), &steps); err != nil {
		w.t.Fatal(err)
	}
	for i := range want {
		if !reflect.DeepEqual(steps[i], want[i]) {
			g, _ := json.MarshalIndent(w.out[i], "", " ")
			p, _ := json.MarshalIndent(want[i], "", " ")
			w.t.Errorf("%s step %d:\nGo %s\nPython %s", name, i, g, p)
		}
	}
}
func newWakeParity(t *testing.T) *wakeParity {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	clock := delivery.NewFakeClock()
	r := &registry.Registry{Store: s, Now: clock.ISO}
	w := &wakeParity{t: t, ctx: ctx, store: s, clock: clock, host: newWakeHost(clock)}
	w.d = delivery.NewService(s, clock)
	w.d.RoleGate = func(context.Context, *store.Store, string, *delivery.TaskSettings) error { return nil }
	w.m = &mergeturn.Service{Store: s, Registry: r, Now: clock.ISO, Delivery: mergeturn.StoreDelivery{Store: s}}
	for _, p := range []struct{ project, task, host string }{{"PRJ-A", "task-alpha", "host-a"}, {"PRJ-B", "task-beta", "host-b"}} {
		if _, err = r.BindScope(ctx, "parent", p.project, registry.Endpoint{TaskID: p.task, HostID: p.host, Cwd: sql.NullString{String: "/alpha", Valid: true}}); err != nil {
			t.Fatal(err)
		}
		if _, err = r.RegisterSupervision(ctx, "INIT-1", p.project, registry.Endpoint{TaskID: "task-supervisor", HostID: "host-s", Cwd: sql.NullString{String: "/sup", Valid: true}}, registry.Endpoint{TaskID: p.task, HostID: p.host, Cwd: sql.NullString{String: "/alpha", Valid: true}}, "execution"); err != nil {
			t.Fatal(err)
		}
	}
	w.exec(`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "rel-a", "ISS-1", "active", "task-alpha", "host-a", "task-child", "host-child", 3, "[]", `["task-alpha"]`, clock.ISO(), clock.ISO())
	w.exec(`INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,?,?,?,?,?,?,?)`, "rel-a", 3, "dispatch-1", "bound", "turn-dispatch-1", "initial_assignment", clock.ISO(), clock.ISO())
	settings := `{"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false},"approvalPolicy":"never","cwd":"/alpha","runtimeWorkspaceRoots":["/alpha"],"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"/alpha","runtimeWorkspaceRoots":["/alpha"]}]}`
	w.exec("INSERT INTO authorized_settings (task_id,settings,source,recorded_at) VALUES (?,?,?,?)", "task-alpha", settings, "creation_result", clock.ISO())
	return w
}
func (w *wakeParity) promote() (string, string) {
	held := w.must(w.m.Request(w.ctx, "owner/repo", "dev", "PRJ-B", "task-beta", "host-b", "head-b", true))
	waiter := w.must(w.m.Request(w.ctx, "owner/repo", "dev", "PRJ-A", "task-alpha", "host-a", "head-a", true, mergeturn.ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.must(w.m.Release(w.ctx, held["turnId"].(string), "task-beta", "returned", "done", ""))
	turn := waiter["turnId"].(string)
	grant := w.must(w.m.Turn(w.ctx, turn))["grant"].(map[string]any)
	return turn, grant["wake"].(map[string]any)["eventId"].(string)
}

func (w *wakeParity) grant(turn string) string {
	return w.must(w.m.Turn(w.ctx, turn))["grant"].(map[string]any)["grantId"].(string)
}
func (w *wakeParity) attempt(event string) any {
	result, err := w.d.Attempt(w.ctx, event, w.host, nil, "")
	if err != nil {
		w.t.Fatal(err)
	}
	if result == nil {
		w.step(nil, nil)
		return nil
	}
	return w.step(result, nil)
}
func (w *wakeParity) phase(event string) {
	item, err := w.d.SnapshotItem(w.ctx, event)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, field := range item {
		if field.Key == "phase" {
			w.step(field.Value, nil)
			return
		}
	}
	w.t.Fatal("snapshot has no phase", item)
}
func Test26_MTW_3_python_second_dispatch(t *testing.T) {
	w := newWakeParity(t)
	first, _ := w.promote()
	old := w.grant(first)
	w.must(w.m.Acknowledge(w.ctx, first, "task-alpha", old, "read it"))
	w.must(w.m.Release(w.ctx, first, "task-alpha", "returned", "handing it back", ""))
	rival := w.must(w.m.Request(w.ctx, "owner/repo", "dev", "PRJ-B", "task-beta", "host-b", "head-b2", true))
	waiter := w.must(w.m.Request(w.ctx, "owner/repo", "dev", "PRJ-A", "task-alpha", "host-a", "head-a2", true, mergeturn.ClaimOptions{Relationship: sql.NullString{String: "rel-a", Valid: true}}))
	w.must(w.m.Release(w.ctx, rival["turnId"].(string), "task-beta", "returned", "done", ""))
	current := w.must(w.m.Turn(w.ctx, waiter["turnId"].(string)))["grant"].(map[string]any)
	event := current["wake"].(map[string]any)["eventId"].(string)
	w.attempt(event)
	w.step(len(w.host.turns["task-alpha"]), nil)
	text := w.host.items["task-alpha"][len(w.host.items["task-alpha"])-1]
	w.step(map[string]any{"newGrant": strings.Contains(text, current["grantId"].(string)), "notOldGrant": !strings.Contains(text, old)}, nil)
	w.compare("mtw3_second_dispatch")
}
func Test26_MTW_3_python_whole_output(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	grant := w.grant(turn)
	w.attempt(event)
	w.step(len(w.host.turns["task-alpha"]), nil)
	text := w.host.items["task-alpha"][len(w.host.items["task-alpha"])-1]
	w.step(map[string]any{"grant": strings.Contains(text, grant), "granted": strings.Contains(text, "merge turn granted"), "repository": strings.Contains(text, "owner/repo"), "acknowledge": strings.Contains(text, "merge-turn-acknowledge"), "noAckProof": !strings.Contains(text, "ack-proof")}, nil)
	w.phase(event)
	w.step(w.m.Acknowledge(w.ctx, turn, "task-alpha", grant, "read it"))
	w.phase(event)
	w.attempt(event)
	w.step(w.host.sends, nil)
	w.compare("mtw3_dispatch")
}
func Test26_MTW_5_python_whole_output(t *testing.T) {
	w := newWakeParity(t)
	w.exec("UPDATE relationships SET allowed_recipients=? WHERE relationship_id=?", `["task-alpha","task-child"]`, "rel-a")
	settings := `{"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false},"approvalPolicy":"never","cwd":"/child","runtimeWorkspaceRoots":["/child"],"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"/child","runtimeWorkspaceRoots":["/child"]}]}`
	w.exec("INSERT INTO authorized_settings (task_id,settings,source,recorded_at) VALUES (?,?,?,?)", "task-child", settings, "creation_result", w.clock.ISO())
	event := strings.Repeat("f", 32)
	receipt := fmt.Sprintf(`{"eventId":"%s","relationshipId":"rel-a","executionGeneration":3,"kind":"revision_request","criteria":[]}`, event)
	w.exec(`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,attempt,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at,observation_count) VALUES (?,?,?,?,?,?,NULL,?,?,?,?,'final',?,?,1)`, event, "rel-a", 3, strings.Repeat("0", 64), delivery.Revision, "relay", "task-alpha", "turn-verdict-1", "completed", receipt, w.clock.ISO(), w.clock.ISO())
	if _, err := w.d.Enqueue(w.ctx, event, delivery.Revision, "task-child"); err != nil {
		t.Fatal(err)
	}
	w.promote()
	reason, err := w.d.SupersessionReason(w.ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		w.step(nil, nil)
	} else {
		w.step(reason, nil)
	}
	rows, err := w.store.All(w.ctx, "SELECT reason FROM delivery_supersession WHERE event_id=?", event)
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		w.step([]any{}, nil)
	} else {
		w.step(rows, nil)
	}
	w.attempt(event)
	w.step(len(w.host.turns["task-child"]), nil)
	w.compare("mtw5_correction")
}
func (w *wakeParity) snapshot(event string) {
	item, err := w.d.SnapshotItem(w.ctx, event)
	if err != nil {
		w.t.Fatal(err)
	}
	w.step(item, nil)
}
func Test26_MTW_8_python_states(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	grant := w.grant(turn)
	w.attempt(event)
	w.snapshot(event)
	w.must(w.m.Acknowledge(w.ctx, turn, "task-alpha", grant, "read it"))
	w.snapshot(event)
	w.must(w.m.Release(w.ctx, turn, "task-alpha", "returned", "done", ""))
	w.snapshot(event)
	w.compare("mtw8_states")
}
func Test26_MTW_8_python_damage(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	sent, err := w.d.Attempt(w.ctx, event, w.host, nil, "")
	if err != nil || sent == nil {
		t.Fatal(sent, err)
	}
	w.exec("DELETE FROM merge_turns WHERE turn_id=?", turn)
	w.snapshot(event)
	w.exec("UPDATE events SET receipt=? WHERE event_id=?", "not a grant", event)
	w.snapshot(event)
	w.compare("mtw8_damage")
}
func Test26_MTW_8_every_status_row_matches_python(t *testing.T) {
	cases := []string{"delivered", "regranted", "returned", "landed", "queued_regranted", "uncertain_regranted", "absent", "unreadable", "answered_queued", "uncertain_answered", "suppressed_answered", "suppressed_regranted"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWakeParity(t)
			turn, event := w.promote()
			grant := w.grant(turn)
			switch name {
			case "delivered", "regranted", "returned", "landed", "absent", "unreadable":
				w.attempt(event)
			}
			if name == "uncertain_regranted" || name == "uncertain_answered" {
				w.exec("UPDATE deliveries SET state=? WHERE event_id=?", "held_uncertain", event)
			}
			switch name {
			case "regranted", "queued_regranted", "uncertain_regranted", "suppressed_regranted":
				w.must(w.m.Ready(w.ctx, turn, "task-alpha", true, "head-a2", ""))
			}
			switch name {
			case "landed", "answered_queued", "uncertain_answered", "suppressed_answered":
				w.must(w.m.Acknowledge(w.ctx, turn, "task-alpha", grant, "read it"))
			}
			if name == "landed" {
				checks := []any{contract.OrderedObject{{Key: "runId", Value: "run-1"}, {Key: "name", Value: "dev-gate"}, {Key: "headSha", Value: "head-a"}, {Key: "conclusion", Value: "success"}, {Key: "attempt", Value: json.Number("1")}}}
				review := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("1")}, {Key: "threadsSeen", Value: []any{"thread-1"}}, {Key: "unresolved", Value: json.Number("0")}}
				reader := &wakeTarget{}
				w.must(w.m.Check(w.ctx, turn, "task-alpha", "head-a", "base-0", checks, review, []string{"dev-gate"}, reader))
				reader.tip = "base-1"
				w.must(w.m.Land(w.ctx, turn, "task-alpha", "merge-1", "base-1", "merged; the base branch read afterwards", reader))
			}
			if name == "returned" {
				w.must(w.m.Release(w.ctx, turn, "task-alpha", "returned", "cannot land it", ""))
			}
			if name == "absent" {
				w.exec("DELETE FROM merge_turns WHERE turn_id=?", turn)
			}
			if name == "unreadable" {
				w.exec("UPDATE events SET receipt=? WHERE event_id=?", "not a grant", event)
			}
			if name == "suppressed_answered" || name == "suppressed_regranted" {
				w.attempt(event)
			}
			w.snapshot(event)
			w.compare("mtw8_row_" + name)
		})
	}
}

type wakeTarget struct{ tip string }

func (w wakeTarget) Tip(context.Context, string, string) (mergeturn.Tip, error) {
	tip := w.tip
	if tip == "" {
		tip = "base-0"
	}
	return mergeturn.Tip{SHA: tip, Source: "fake", Reference: "refs/heads/dev", Repository: "owner/repo"}, nil
}
func Test26_MTW_9_every_notice_row_matches_python(t *testing.T) {
	cases := []string{"red_gate", "preview", "quoted", "resubmitted", "different_events", "headless_latest", "none_required", "no_report", "no_handoff", "other_head", "other_repo", "other_base", "no_base", "mixed_handoff", "disagree"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWakeParity(t)
			add := func(event string, submission int, head any, required []string, repo string, base any) {
				w.exec(`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event, submission, "rel-a", 3, "rev", repo, base, head, "done", "r", "1", "s", "n", w.clock.ISO())
				if required != nil {
					value, _ := json.Marshal(required)
					w.exec(`INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, event, submission, 0, string(value), "[]", "{}", "[]", w.clock.ISO())
				}
			}
			entry := func(event string, submission int, head any, required []string) {
				add(event, submission, head, required, "owner/repo", "dev")
			}
			gate := []string{"dev-gate"}
			switch name {
			case "red_gate", "preview":
				entry("event-a", 1, "head-a", gate)
			case "quoted":
				entry("event-a", 1, "head-a", []string{"--check", "build linux", "dev-gate", "lint'; echo x"})
			case "resubmitted":
				entry("event-a", 1, "head-old", []string{"old-gate"})
				entry("event-a", 2, "head-a", gate)
			case "different_events":
				entry("event-a", 1, "head-a", gate)
				entry("event-a", 2, "head-a", []string{"optional-lint"})
				entry("event-b", 1, "head-a", gate)
			case "headless_latest":
				entry("event-a", 1, "head-a", gate)
				entry("event-a", 2, nil, nil)
				entry("event-b", 1, "head-a", []string{"optional-lint"})
			case "none_required":
				entry("event-a", 1, "head-a", []string{})
			case "no_handoff":
				entry("event-a", 1, "head-a", nil)
			case "other_head":
				entry("event-a", 1, "head-old", gate)
			case "other_repo":
				add("event-a", 1, "head-a", gate, "owner/other", "dev")
			case "other_base":
				add("event-a", 1, "head-a", gate, "owner/repo", "main")
			case "no_base":
				add("event-a", 1, "head-a", []string{}, "owner/repo", nil)
			case "mixed_handoff":
				entry("event-a", 1, "head-a", []string{})
				entry("event-b", 1, "head-a", nil)
			case "disagree":
				entry("event-a", 1, "head-a", gate)
				entry("event-b", 1, "head-a", []string{"other-gate"})
			}
			turn, event := w.promote()
			reading, err := supervisor.RequiredForCandidate(w.ctx, w.store, "rel-a", "owner/repo", "dev", "head-a")
			if err != nil {
				t.Fatal(err)
			}
			if reading.Known {
				w.step(map[string]any{"required": reading.Required, "eventId": reading.EventID, "submissionNo": reading.SubmissionNo}, nil)
			} else {
				w.step(map[string]any{"required": nil, "reason": reading.Reason}, nil)
			}
			text, err := w.d.PreviewMessage(w.ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			w.step(text, nil)
			w.attempt(event)
			w.step(w.host.items["task-alpha"][len(w.host.items["task-alpha"])-1], nil)
			if name == "red_gate" {
				w.must(w.m.Acknowledge(w.ctx, turn, "task-alpha", w.grant(turn), "read it"))
				checks := []any{contract.OrderedObject{{Key: "runId", Value: "run-1"}, {Key: "name", Value: "dev-gate"}, {Key: "headSha", Value: "head-a"}, {Key: "conclusion", Value: "failure"}, {Key: "attempt", Value: json.Number("1")}}, contract.OrderedObject{{Key: "runId", Value: "run-lint"}, {Key: "name", Value: "optional-lint"}, {Key: "headSha", Value: "head-a"}, {Key: "conclusion", Value: "success"}, {Key: "attempt", Value: json.Number("1")}}}
				review := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("1")}, {Key: "threadsSeen", Value: []any{"thread-1"}}, {Key: "unresolved", Value: json.Number("0")}}
				_, err = w.m.Check(w.ctx, turn, "task-alpha", "head-a", "base-0", checks, review, gate, &wakeTarget{})
				w.refusal(err)
				checks = checks[:1]
				checks[0] = contract.OrderedObject{{Key: "runId", Value: "run-1"}, {Key: "name", Value: "dev-gate"}, {Key: "headSha", Value: "head-a"}, {Key: "conclusion", Value: "success"}, {Key: "attempt", Value: json.Number("1")}}
				w.step(w.m.Check(w.ctx, turn, "task-alpha", "head-a", "base-0", checks, review, gate, &wakeTarget{}))
			}
			w.compare("mtw9_row_" + name)
		})
	}
}
func (w *wakeParity) refusal(err error) {
	w.t.Helper()
	var r *store.RefusedError
	if !errors.As(err, &r) {
		w.t.Fatalf("expected refusal: %v", err)
	}
	w.out = append(w.out, map[string]any{"refused": map[string]any{"reason": r.Reason, "detail": r.Detail}})
}
func Test26_MTW_9_python_notice(t *testing.T) {
	w := newWakeParity(t)
	_, event := w.promote()
	message, err := w.d.PreviewMessage(w.ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	w.step(map[string]any{"notRecorded": strings.Contains(message, "requiredDeclared: not recorded ("), "mergeEvidence": strings.Contains(message, "merge-evidence --repository owner/repo"), "requiredPlaceholder": strings.Contains(message, " --required=<")}, nil)
	w.attempt(event)
	sent := w.host.items["task-alpha"][len(w.host.items["task-alpha"])-1]
	w.step(map[string]any{"notRecorded": strings.Contains(sent, "requiredDeclared: not recorded ("), "mergeEvidence": strings.Contains(sent, "merge-evidence --repository owner/repo")}, nil)
	w.compare("mtw9_notice")
}
func Test26_MTW_8_python_whole_output(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	row, err := w.store.One(w.ctx, "SELECT receipt FROM events WHERE event_id=?", event)
	if err != nil {
		t.Fatal(err)
	}
	receipt := row.Get("receipt").(string)
	reason, err := mergeturn.GrantSupersessionFor(w.ctx, w.store, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		w.step(nil, nil)
	} else {
		w.step(reason, nil)
	}
	w.snapshot(event)
	w.must(w.m.Ready(w.ctx, turn, "task-alpha", true, "head-a2", ""))
	reason, err = mergeturn.GrantSupersessionFor(w.ctx, w.store, receipt)
	if err != nil {
		t.Fatal(err)
	}
	w.step(reason, nil)
	w.snapshot(event)
	w.compare("mtw8_currency")
}
func Test26_MTW_4_python_restated(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	w.must(w.m.Ready(w.ctx, turn, "task-alpha", true, "head-a2", ""))
	w.attempt(event)
	w.step(w.host.sends, nil)
	w.compare("mtw4_restated")
}
func Test26_MTW_4_python_generation(t *testing.T) {
	w := newWakeParity(t)
	_, event := w.promote()
	w.exec("UPDATE relationships SET execution_generation=4 WHERE relationship_id='rel-a'")
	w.exec("INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,reason,opened_at) VALUES (?,?,?,?,?,?)", "rel-a", 4, "revision-1", "pending", "needs_changes_revision", w.clock.ISO())
	w.attempt(event)
	w.phase(event)
	w.compare("mtw4_generation")
}
func Test26_MTW_4_python_whole_output(t *testing.T) {
	w := newWakeParity(t)
	turn, event := w.promote()
	w.must(w.m.Acknowledge(w.ctx, turn, "task-alpha", w.grant(turn), "read it"))
	w.attempt(event)
	w.step(w.host.sends, nil)
	w.compare("mtw4_answered")
}
