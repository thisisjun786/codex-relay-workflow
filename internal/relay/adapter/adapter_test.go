package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	testsupport.Main(m, testsupport.TempDirInRoot, func(root string) (func() error, error) {
		suiteDirectory = root
		return nil, installSuiteBinary(root)
	})
}

type scriptRPC struct {
	mu      sync.Mutex
	answers []map[string]any
	calls   []any
}

func (r *scriptRPC) Call(_ context.Context, method string, params map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, []any{method, params})
	if len(r.answers) == 0 {
		return nil, fmt.Errorf("unexpected RPC %s", method)
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	if raw, exists := answer["rawResponse"]; exists {
		return json.Marshal(raw)
	}
	if rpc, ok := answer["rpcError"].(map[string]any); ok {
		return nil, &appserver.RPCError{Method: method, Message: rpc["message"].(string), Object: rpc}
	}
	if answer["creation"] == true {
		cwd := params["cwd"].(string)
		return json.Marshal(map[string]any{"thread": map[string]any{"id": "thread-created-1"}, "cwd": cwd, "approvalPolicy": "never", "model": "gpt-5.4", "reasoningEffort": "medium", "runtimeWorkspaceRoots": []any{cwd}, "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}})
	}
	if phase, ok := answer["phase"].(string); ok {
		return nil, &appserver.PhaseTimeout{Method: method, Phase: phase, Bound: 250 * time.Millisecond}
	}
	if message, ok := answer["error"].(string); ok {
		return nil, errors.New(message)
	}
	return json.Marshal(answer)
}

type scenario struct {
	page         int
	store        bool
	settingsFree bool
	policy       string
	settings     map[string]any
	answers      []map[string]any
	actions      [][]any
}

func capture(t *testing.T, s scenario) {
	t.Helper()
	root := t.TempDir()
	rpc := &scriptRPC{answers: s.answers, calls: []any{}}
	options := Options{RPC: rpc, Page: s.page, Clock: delivery.NewFakeClock()}
	var db *store.Store
	var err error
	if s.store {
		// The store gets a directory of its own: a directory holds one store's takeover.json.
		db, err = openStore(context.Background(), filepath.Join(root, "go", "go-store.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		options.Store = db
	}
	if s.settings != nil {
		options.Ledger, err = ledger.OpenWithOptions(filepath.Join(root, "go-operations.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.policy != "" {
		p, e := execution.FromBytes([]byte(s.policy), "policy")
		if e != nil {
			t.Fatal(e)
		}
		options.Policy = p
	}
	a := New(options)
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close adapter: %v", err)
		}
	})
	results := []any{}
	receiptBytes := []any{}
	attemptBytes := []any{}
	for _, action := range s.actions {
		var result any
		var actionErr error
		switch action[0] {
		case "begin":
			_, _, actionErr = a.ledger.Begin(context.Background(), action[1].(string), "send_message_to_thread", map[string]any{"threadId": action[2], "message": action[3]}, nil)
		case "no-settings":
			_, actionErr = a.SendMessage(context.Background(), action[1].(string), action[2].(string), action[3].(string), nil)
		case "create":
			in := bridge.CreateThread{RequestID: action[1].(string), CWD: root, Title: action[2].(string), Model: action[3].(string), Effort: "medium", Sandbox: "read-only", Role: action[4].(string)}
			r, e := a.Create(context.Background(), in)
			actionErr = e
			if r != nil {
				o, e := a.receipt(context.Background(), in.RequestID, r["replayed"] == true)
				if e != nil {
					t.Fatal(e)
				}
				result = plain(o)
				receiptBytes = append(receiptBytes, dumps(o, false))
			}
		case "find":
			scan, e := a.FindToken(context.Background(), action[1].(string), action[2].(string), action[3].(int), false)
			actionErr = e
			result = map[string]any{"found": scan.Found, "turn_id": scan.TurnID, "exhausted": scan.Exhausted, "scanned": scan.Scanned, "other_turn": scan.OtherTurn, "other_kind": scan.OtherKind}
		case "archive":
			result, actionErr = a.IsArchived(context.Background(), action[1].(string), action[2])
		case "turn":
			turn, e := a.ReadTurn(context.Background(), action[1].(string), action[2].(string))
			actionErr = e
			if turn != nil {
				result = map[string]any{"turn_id": turn.TurnID, "status": turn.Status, "started_at": turn.StartedAt}
			}
		case "turn-ids":
			result, actionErr = a.ListTurnIDs(context.Background(), action[1].(string), 20)
		case "thread":
			facts, e := a.ReadThread(context.Background(), action[1].(string))
			actionErr = e
			result = map[string]any{"runtime_status": facts.RuntimeStatus, "can_accept_input": facts.CanAcceptInput}
		case "lifecycle-observe":
			observation := delivery.Observe(context.Background(), a, action[1].(string), nil, true)
			e := delivery.RecordLifecycle(context.Background(), db, options.Clock, observation)
			actionErr = e
			if e == nil {
				var task string
				var runtime, archived, goal, accepts, reason any
				var deliverable, detail, observed string
				if e = db.DB.QueryRow("SELECT task_id,runtime_status,archived,goal_status,can_accept_input,deliverable,withhold_reason,detail,observed_at FROM recipient_lifecycle WHERE task_id=?", action[1]).Scan(&task, &runtime, &archived, &goal, &accepts, &deliverable, &reason, &detail, &observed); e != nil {
					t.Fatal(e)
				}
				result = map[string]any{"observation": map[string]any{"task_id": observation.TaskID, "runtime_status": observation.RuntimeStatus, "archived": observation.Archived, "goal_status": observation.GoalStatus, "can_accept_input": observation.CanAcceptInput, "deliverable": observation.Deliverable, "withhold_reason": observation.WithholdReason, "detail": observation.Detail}, "row": map[string]any{"task_id": task, "runtime_status": runtime, "archived": archived, "goal_status": goal, "can_accept_input": accepts, "deliverable": deliverable, "withhold_reason": reason, "detail": detail, "observed_at": observed}}
			}
		case "lifecycle-record":
			facts, e := a.ReadThread(context.Background(), action[1].(string))
			if e != nil {
				actionErr = e
				break
			}
			e = delivery.RecordLifecycle(context.Background(), db, options.Clock, delivery.Lifecycle{TaskID: action[1].(string), RuntimeStatus: facts.RuntimeStatus, CanAcceptInput: facts.CanAcceptInput, Deliverable: "unknown"})
			if e != nil {
				result = map[string]any{"error": e.Error()}
				break
			}
			var runtime, accepts any
			if e := db.DB.QueryRow("SELECT runtime_status,can_accept_input FROM recipient_lifecycle WHERE task_id=?", action[1]).Scan(&runtime, &accepts); e != nil {
				t.Fatal(e)
			}
			result = map[string]any{"runtime_status": runtime, "can_accept_input": accepts}
		case "goal":
			result, actionErr = a.ReadGoalStatus(context.Background(), action[1].(string))
		case "operation":
			op, e := a.GetOperation(context.Background(), action[1].(string))
			actionErr = e
			if op != nil {
				result = plain(op)
				receiptBytes = append(receiptBytes, dumps(op, false))
			}
		case "fingerprint":
			result, actionErr = a.RecipientFingerprint(context.Background(), action[1].(string))
		case "send", "guard":
			var guard Guard
			if action[0] == "guard" {
				decision := action[4]
				guard = func(ctx context.Context) (map[string]any, error) {
					if _, err := a.HostCall(ctx, "thread/goal/get", map[string]any{"threadId": action[2]}); err != nil {
						return nil, err
					}
					if decision == nil {
						return nil, nil
					}
					return decision.(map[string]any), nil
				}
			}
			receipt, e := a.Send(context.Background(), action[1].(string), action[2].(string), action[3].(string), &delivery.TaskSettings{Data: ordered(s.settings).(contract.OrderedObject), SettingsFreeResume: s.settingsFree}, guard, 0)
			actionErr = e
			if receipt != nil {
				result = plain(receipt)
				receiptBytes = append(receiptBytes, dumps(receipt, false))
				if action[0] == "send" {
					record, err := delivery.AttemptRecord(delivery.Classify(receipt), action[1].(string), "ev-1", 1, "01parent", "unknown", "2026-09-22T00:00:00Z", nil)
					if err != nil {
						t.Fatal(err)
					}
					attemptBytes = append(attemptBytes, dumps(record, false))
				}
			}
		default:
			t.Fatalf("unknown action %v", action)
		}
		if actionErr != nil {
			result = map[string]any{"error": actionErr.Error()}
		}
		results = append(results, result)
	}
	cursors := []any{}
	if db != nil {
		for _, name := range []string{"archived", "archived:exec", "unarchived_all", "unarchived_all:exec", "unarchived_cwd"} {
			row, e := db.DiscoveryCursor(context.Background(), "01child-task", name)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				t.Fatal(e)
			}
			var cursor any
			if row.Cursor.Valid {
				cursor = row.Cursor.String
			}
			exhausted := 0
			if row.Exhausted {
				exhausted = 1
			}
			cursors = append(cursors, map[string]any{"task_id": row.TaskID, "listing": row.Listing, "cursor": cursor, "exhausted": exhausted, "scanned": row.Scanned, "updated_at": row.UpdatedAt})
		}
	}
	got := map[string]any{"results": results, "calls": rpc.calls, "cursors": cursors, "receiptBytes": receiptBytes, "attemptBytes": attemptBytes}
	// The golden holds the answer as JSON reads it back.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err := decodeNumbers(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	expectJSON(t, "capture", actual)
}
func item(turn, id, body string) map[string]any {
	return map[string]any{"turnId": turn, "item": map[string]any{"id": id, "text": body}}
}
func page(rows ...map[string]any) map[string]any {
	a := []any{}
	for _, row := range rows {
		a = append(a, row)
	}
	return map[string]any{"data": a, "nextCursor": nil}
}

const worktree = "/workspace/example/codex-session-relay/relay-core"

func authorized() map[string]any {
	return map[string]any{"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, "approvalPolicy": "never", "cwd": worktree, "runtimeWorkspaceRoots": []any{worktree}, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "environments": []any{map[string]any{"environmentId": "local", "cwd": worktree, "runtimeWorkspaceRoots": []any{worktree}}}}
}
func resume() map[string]any {
	r := authorized()
	env := r["environments"]
	delete(r, "environments")
	r["thread"] = map[string]any{"id": "thread-1", "environments": env}
	r["activePermissionProfile"] = nil
	return r
}
func sendScenario(response map[string]any, actions ...[]any) scenario {
	return scenario{settings: authorized(), answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}, response, {"turn": map[string]any{"id": "fake-turn-1"}}}, actions: actions}
}
func Test28_BAD_1_ForwardPagingAndFingerprint(t *testing.T) {
	t.Parallel()
	first := page(item("t1", "i1", "noise"))
	first["nextCursor"] = "1"
	first["backwardsCursor"] = "back-0"
	second := page(item("t2", "i2", "carries del-abc-a1 here"))
	second["backwardsCursor"] = "back-1"
	capture(t, scenario{page: 1, answers: []map[string]any{first, second}, actions: [][]any{{"find", "01child-task", "del-abc-a1", 10}}})
	bounded := page(item("t2", "i2", "more noise"))
	bounded["nextCursor"] = "2"
	capture(t, scenario{page: 1, answers: []map[string]any{first, bounded}, actions: [][]any{{"find", "01child-task", "del-abc-a1", 2}}})
	capture(t, scenario{page: 5, answers: []map[string]any{page(item("t1", "i1", "noise"))}, actions: [][]any{{"find", "01child-task", "missing", 10}}})
	capture(t, scenario{answers: []map[string]any{page(item("t1", "i1", "original")), page(item("t1", "i1", "original plus a delivery token"))}, actions: [][]any{{"fingerprint", "01child-task"}, {"fingerprint", "01child-task"}}})
}
func Test28_BAD_2_TurnLookup(t *testing.T) {
	t.Parallel()
	more := page(map[string]any{"id": "other", "status": "completed"})
	more["nextCursor"] = "keep-going"
	capture(t, scenario{page: 1, answers: []map[string]any{more, more, more, more}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{page: 5, answers: []map[string]any{page(map[string]any{"id": "other", "status": "completed"})}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{answers: []map[string]any{page(map[string]any{"id": "wanted", "status": "completed", "startedAt": 1789420929})}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
}
func Test28HostDecodedValueParity(t *testing.T) {
	t.Parallel()
	capture(t, scenario{store: true, answers: []map[string]any{{"thread": map[string]any{"status": "idle"}}, page(), page(), page(), page(), {"goal": nil}}, actions: [][]any{{"lifecycle-observe", "01child-task"}}})
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": json.Number("7")}, map[string]any{"id": nil}, map[string]any{"id": json.Number("1.5")}, map[string]any{"id": []any{"turn"}}, map[string]any{"id": map[string]any{"turn": 1}}, map[string]any{"id": "turn"}}}}}, actions: [][]any{{"turn-ids", "01child-task"}}})
	for _, value := range []any{[]any{"raw"}, map[string]any{"raw": true}} {
		capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": "wanted", "status": value, "startedAt": value}}, "nextCursor": nil}}}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	}
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": "wanted", "status": json.Number("7"), "startedAt": "bad"}}, "nextCursor": nil}}}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": "wanted", "status": nil, "startedAt": nil}}, "nextCursor": nil}}}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": "wanted", "status": "completed"}, "junk"}, "nextCursor": nil}}}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{"junk", map[string]any{"id": "wanted", "status": "completed"}}, "nextCursor": nil}}}, actions: [][]any{{"turn", "01child-task", "wanted"}}})
	capture(t, scenario{store: true, answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{map[string]any{"id": "01child-task"}, "junk"}, "nextCursor": nil}}}, actions: [][]any{{"archive", "01child-task", nil}}})
	capture(t, scenario{store: true, answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{"junk", map[string]any{"id": "01child-task"}}, "nextCursor": nil}}}, actions: [][]any{{"archive", "01child-task", nil}}})
	capture(t, scenario{answers: []map[string]any{{"rawResponse": map[string]any{"data": []any{json.Number("7"), map[string]any{"turnId": "t", "item": map[string]any{"text": "needle"}}}, "nextCursor": nil}}}, actions: [][]any{{"find", "01child-task", "needle", 10}}})
	// A thread whose status is not an object stops the guarded send after thread/read in both
	// runtimes: the fence's (state.get("thread") or {}).get("status", {}).get("type") raises,
	// and the send settles outcome_unknown instead of resuming on a status nobody could read.
	for _, thread := range []any{map[string]any{"status": nil}, map[string]any{}, nil, map[string]any{"status": "idle"}, map[string]any{"status": map[string]any{"type": nil}}} {
		capture(t, scenario{settings: authorized(), answers: []map[string]any{{"thread": thread}, resume(), {"turn": map[string]any{"id": "fake-turn-1"}}}, actions: [][]any{{"send", "del-800000000000-a1", "thread-1", "hi"}, {"operation", "del-800000000000-a1"}}})
	}
}
func Test28_BAD_3_ThreadGoalAndAbsentOperation(t *testing.T) {
	t.Parallel()
	capture(t, scenario{answers: []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": false}}, {"goal": nil}, {"goal": map[string]any{"status": "budgetLimited"}}}, actions: [][]any{{"thread", "01child-task"}, {"goal", "01child-task"}, {"goal", "01child-task"}, {"operation", "del-aaaaaaaaaaaa-a1"}}})
}
func Test28_BAD_4_ArchiveDiscovery(t *testing.T) {
	t.Parallel()
	found := page(map[string]any{"id": "01child-task"})
	for _, answers := range [][]map[string]any{{page(), page(), page(), page(), found}, {page(), found}, {page(), page(), page(), page(), page()}} {
		capture(t, scenario{store: true, answers: answers, actions: [][]any{{"archive", "01child-task", "/corrected/cwd"}}})
	}
	answers := []map[string]any{page(), page(), page()}
	for i := 0; i < 4; i++ {
		p := page(map[string]any{"id": fmt.Sprintf("other-%d", i)})
		p["nextCursor"] = fmt.Sprint(i + 1)
		answers = append(answers, p)
	}
	answers = append(answers, page(), page(), page())
	for i := 4; i < 6; i++ {
		p := page(map[string]any{"id": fmt.Sprintf("other-%d", i)})
		p["nextCursor"] = fmt.Sprint(i + 1)
		answers = append(answers, p)
	}
	answers = append(answers, found)
	capture(t, scenario{page: 1, store: true, answers: answers, actions: [][]any{{"archive", "01child-task", nil}, {"archive", "01child-task", nil}}})
}
func Test28ArchiveListingErrorMatchesTheGolden(t *testing.T) {
	t.Parallel()
	answers := []map[string]any{{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}, {"error": "listing failed"}, {"error": "listing failed"}, {"error": "listing failed"}, {"error": "listing failed"}, {"goal": nil}}
	capture(t, scenario{store: true, answers: answers, actions: [][]any{{"lifecycle-observe", "01child-task"}}})
}
func Test28_BAD_5_ExecSourceDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source string
		at     int
		cwd    any
	}{{"exec", 2, nil}, {"exec", 1, nil}, {"vscode", 0, "/w"}, {"vscode", 1, "/elsewhere"}, {"cli", 3, nil}, {"appServer", -1, nil}} {
		count := tc.at + 1
		if count == 0 {
			count = 4
		}
		answers := make([]map[string]any, count)
		for i := range answers {
			answers[i] = page()
		}
		if tc.at >= 0 {
			answers[tc.at] = page(map[string]any{"id": "01child-task", "source": tc.source})
		}
		capture(t, scenario{store: true, answers: answers, actions: [][]any{{"archive", "01child-task", tc.cwd}}})
	}
	answers := []map[string]any{page(), page()}
	for i := 0; i < 4; i++ {
		p := page(map[string]any{"id": fmt.Sprintf("exec-%d", i), "source": "exec"})
		p["nextCursor"] = fmt.Sprint(i + 1)
		answers = append(answers, p)
	}
	answers = append(answers, page(), page(), page())
	for i := 4; i < 6; i++ {
		p := page(map[string]any{"id": fmt.Sprintf("exec-%d", i), "source": "exec"})
		p["nextCursor"] = fmt.Sprint(i + 1)
		answers = append(answers, p)
	}
	answers = append(answers, page(map[string]any{"id": "01child-task", "source": "exec"}))
	capture(t, scenario{page: 1, store: true, answers: answers, actions: [][]any{{"archive", "01child-task", nil}, {"archive", "01child-task", nil}}})
}
func Test28_BAD_6_CrossCallerLedger(t *testing.T) {
	t.Parallel()
	capture(t, sendScenario(resume(), []any{"send", "del-bbbbbbbbbbbb-a1", "thread-1", "hello"}, []any{"operation", "del-bbbbbbbbbbbb-a1"}, []any{"operation", "del-cccccccccccc-a9"}))
}
func Test28_BAD_7_GuardedWireShape(t *testing.T) {
	t.Parallel()
	capture(t, sendScenario(resume(), []any{"send", "del-100000000000-a1", "thread-1", "hi"}))
	capture(t, scenario{settings: authorized(), actions: [][]any{{"no-settings", "send-without-settings", "thread-1", "hello"}}})
}
func Test28_BAD_8_SettingsWithhold(t *testing.T) {
	t.Parallel()
	shareGoldens(t)
	for _, kind := range []string{"omitted", "null", "empty"} {
		r := resume()
		if kind == "omitted" {
			delete(r, "runtimeWorkspaceRoots")
		} else if kind == "null" {
			r["runtimeWorkspaceRoots"] = nil
		} else {
			r["runtimeWorkspaceRoots"] = []any{}
		}
		s := sendScenario(r, []any{"send", "empty-roots", "thread-1", "hi"})
		s.settings["runtimeWorkspaceRoots"] = []any{}
		capture(t, s)
	}
	r := resume()
	r["thread"].(map[string]any)["environments"] = []any{map[string]any{"environmentId": "remote", "cwd": worktree, "runtimeWorkspaceRoots": []any{worktree}}}
	capture(t, sendScenario(r, []any{"send", "del-400000000000-a3", "thread-1", "hi"}))
	for _, key := range []string{"model", "reasoningEffort", "cwd", "runtimeWorkspaceRoots", "sandbox", "approvalPolicy"} {
		t.Run(key+"_null", func(t *testing.T) {
			t.Parallel()
			r := resume()
			r[key] = nil
			capture(t, sendScenario(r, []any{"send", "del-200000000000-a1", "thread-1", "hi"}))
		})
	}
	for _, change := range []map[string]any{{"model": "something-else"}, {"reasoningEffort": "low"}, {"cwd": "/somewhere/else"}, {"runtimeWorkspaceRoots": []any{"/somewhere/else"}}, {"sandbox": map[string]any{"type": "dangerFullAccess"}}, {"thread": map[string]any{"id": "thread-1", "environments": nil}}, {"thread": map[string]any{"id": "thread-1", "environments": []any{}}}} {
		r := resume()
		for k, v := range change {
			r[k] = v
		}
		capture(t, sendScenario(r, []any{"send", "del-300000000000-a1", "thread-1", "hi"}))
	}
	for _, change := range []map[string]any{{"networkAccess": true}, {"writableRoots": []any{"/"}}} {
		r := resume()
		p := r["sandbox"].(map[string]any)
		for k, v := range change {
			p[k] = v
		}
		capture(t, sendScenario(r, []any{"send", "del-300000000000-a2", "thread-1", "hi"}))
	}
}
func Test28_BAD_9_ApprovalChannelRefusal(t *testing.T) {
	t.Parallel()
	for _, policy := range []any{"untrusted", map[string]any{"granular": map[string]any{"mcp_elicitations": true, "rules": true, "sandbox_approval": true}}} {
		r := resume()
		r["approvalPolicy"] = policy
		capture(t, sendScenario(r, []any{"send", "del-500000000000-a1", "thread-1", "hi"}))
	}
}
func Test28_BAD_10_RetainedReplay(t *testing.T) {
	t.Parallel()
	concurrencyCase(t, "replay")
	capture(t, scenario{settings: authorized(), actions: [][]any{{"begin", "del-700000000000-a1", "thread-1", "hi"}, {"send", "del-700000000000-a1", "thread-1", "hi"}}})
	capture(t, sendScenario(resume(), []any{"send", "del-600000000000-a1", "thread-1", "hi"}, []any{"send", "del-600000000000-a1", "thread-1", "hi"}, []any{"send", "del-600000000000-a1", "thread-1", "DIFFERENT"}, []any{"send", "del-600000000000-a1", "other-thread", "hi"}))
}
