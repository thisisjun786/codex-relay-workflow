package supervisor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	_ "unsafe"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// rrOperation is one operation a Python restoration-visibility or correction-form test made: its
// kind and arguments, the clock and the snapshot of the store it ran on.
type rrOperation struct {
	Kind string         `json:"kind"`
	Args map[string]any `json:"args"`
	Now  float64        `json:"now"`
	Pre  string         `json:"pre"`
}

// rrOrdered sorts only the outer capture representation. Stored JSON cells are strings:
// their whitespace, escaping, key order and every rendered message byte stay untouched.
func rrOrdered(value any) any {
	switch v := value.(type) {
	case delivery.Obj:
		m := map[string]any{}
		for _, f := range v {
			m[f.Key] = f.Value
		}
		return rrOrdered(m)
	case map[string]any:
		if v == nil {
			return nil
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := delivery.Obj{}
		for _, k := range keys {
			out = append(out, delivery.F{Key: k, Value: rrOrdered(v[k])})
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = rrOrdered(x)
		}
		return out
	case map[string][]any:
		m := map[string]any{}
		for k, x := range v {
			m[k] = x
		}
		return rrOrdered(m)
	default:
		return value
	}
}

func rrBytes(t *testing.T, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := contract.Emit(&b, rrOrdered(v)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// rrResult is one replayed operation's bytes, each under the name of what it is.
type rrResult struct {
	names  []string
	values [][]byte
}

func (r *rrResult) add(name string, got []byte) {
	r.names = append(r.names, name)
	r.values = append(r.values, got)
}
func rrTables(t *testing.T, s *store.Store) []byte {
	t.Helper()
	tables := map[string]any{}
	for name, rows := range testsupport.TableRows(t, s.DB, "") {
		list := make([]any, len(rows))
		for i, row := range rows {
			list[i] = row
		}
		tables[name] = list
	}
	// The store names its owning runtime: compared as the runtime-neutral owner.
	rows, _ := tables["schema_meta"].([]any)
	testsupport.OwnerNeutralRows(t, rows)
	return rrBytes(t, tables)
}

// rrCapture restores the tree a Python restoration-visibility or correction-form test left (the
// former testdata/res_rcf_capture.py): each operation's pre-N.sqlite3 snapshot and capture.json's
// list of operations.
func rrCapture(t *testing.T, module, method string) (string, []rrOperation) {
	t.Helper()
	root, err := os.MkdirTemp("", "crw-rr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	supervisor.TreeFixture(t, module+" "+method, root)
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Operations []rrOperation `json:"operations"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Operations) == 0 {
		t.Fatal("the capture lists no operation")
	}
	return root, capture.Operations
}
func rrInputs(v any) any {
	switch x := v.(type) {
	case json.Number:
		n, e := x.Int64()
		if e == nil {
			return int(n)
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = rrInputs(y)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, y := range x {
			out[k] = rrInputs(y)
		}
		return out
	default:
		return v
	}
}
func rrObjects(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := []string{}
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := delivery.Obj{}
		for _, k := range keys {
			out = append(out, delivery.F{Key: k, Value: rrObjects(x[k])})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = rrObjects(y)
		}
		return out
	default:
		return v
	}
}
func rrFindings(v any) []any                     { out, _ := rrObjects(v).([]any); return out }
func rrString(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func rrReplay(t *testing.T, root string, op rrOperation, result *rrResult) {
	t.Helper()
	ctx := context.Background()
	// Each operation replays on its own copy of the Python snapshot, in a directory of its own,
	// stamped as Go's own store (Restamp): the tables below include schema_meta, whose owner row
	// rrTables neutralizes.
	path := filepath.Join(t.TempDir(), "go.sqlite3")
	data, err := os.ReadFile(op.Pre)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Restamp(t, path)
	s, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	clock := &delivery.FakeClock{T: op.Now}
	d := delivery.NewService(s, clock)
	ack := delivery.NewAck(d)
	args := rrInputs(op.Args).(map[string]any)
	event := rrString(args, "event")
	var got any
	switch op.Kind {
	case "verdict":
		got, err = ack.RecordVerdict(ctx, event, rrString(args, "verdict"), rrString(args, "verdict_turn_id"), rrFindings(args["criteria"]), rrFindings(args["findings"]), args["reason"], args["expected_criteria_digest"])
	case "restoration":
		got, err = ack.RestorationOf(ctx, event)
	case "render":
		got, err = d.PreviewMessage(ctx, event)
	case "preview":
		got, err = d.PreviewMessage(ctx, event)
	case "tight":
		got, err = d.PreviewReport(ctx, event, rrString(args, "request"), args["budget"].(int))
	case "attempt":
		got, err = d.Attempt(ctx, event, &rrHost{}, nil, "relay")
	case "cli-verdict", "show":
		state := filepath.Join(t.TempDir(), "cli-state")
		if err = os.MkdirAll(state, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(state, "relay.sqlite3"), data, 0600); err != nil {
			t.Fatal(err)
		}
		testsupport.Restamp(t, filepath.Join(state, "relay.sqlite3"))
		argv := []string{"--state", state, "--json"}
		if op.Kind == "show" {
			argv = append(argv, "show", "--event", event)
		} else {
			argv = append(argv, "verdict")
			for _, key := range []string{"event", "verdict", "verdict_turn", "criterion", "finding", "criteria", "restoration", "reason", "expect_criteria_digest"} {
				value := args[key]
				if value == nil {
					continue
				}
				flag := "--" + strings.ReplaceAll(key, "_", "-")
				if list, ok := value.([]any); ok {
					for _, v := range list {
						argv = append(argv, flag, v.(string))
					}
				} else {
					argv = append(argv, flag, value.(string))
				}
			}
		}
		previous := sosDeliveryClock
		sosDeliveryClock = clock
		var stdout, stderr bytes.Buffer
		code := cli.ExecuteAs(ctx, "codex-session-relay", argv, &stdout, &stderr)
		sosDeliveryClock = previous
		result.add("stdout", stdout.Bytes())
		if stderr.Len() != 0 {
			t.Errorf("unexpected stderr (exit %d): %s", code, stderr.Bytes())
		}
		cliStore, e := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		result.add("tables", rrTables(t, cliStore))
		if e = cliStore.Close(); e != nil {
			t.Fatal(e)
		}
		return
	default:
		t.Fatalf("unhandled operation %s", op.Kind)
	}
	output := map[string]any{"value": got}
	if err != nil {
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%s: %v", op.Kind, err)
		}
		output = map[string]any{"error": "refused", "reason": refused.Reason, "detail": refused.Detail}
	}
	result.add("output", rrBytes(t, output))
	result.add("tables", rrTables(t, s))
}
func rrRun(t *testing.T, module string, methods []string) {
	t.Helper()
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			root, ops := rrCapture(t, module, method)
			results := make([]*rrResult, len(ops))
			for i, op := range ops {
				if op.Kind == "report" || op.Kind == "read" {
					// Python's report.record and its read-back are recorded but not replayed: no
					// product path writes a work report (decision 53). Each operation runs on its
					// own snapshot, so the others do not depend on them.
					continue
				}
				t.Run(fmt.Sprintf("%02d_%s", i, op.Kind), func(t *testing.T) {
					result := &rrResult{}
					rrReplay(t, root, op, result)
					results[i] = result
				})
			}
			// Each operation's bytes are compared with the golden in the method's test, so a
			// method keeps one golden file.
			for i, result := range results {
				if result == nil {
					continue
				}
				for j, name := range result.names {
					golden.Check(t, fmt.Sprintf("%02d_%s %s", i, ops[i].Kind, name), result.values[j], supervisor.TreeGolden(t, root)...)
				}
			}
		})
	}
}

// The source fixture's child has no turns before its revision is delivered. This
// adapter preserves FakeHostAdapter's successful send receipt and lifecycle facts.
type rrHost struct{ delivery.Adapter }

func (*rrHost) ReadThread(context.Context, string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &yes}, nil
}
func (*rrHost) IsArchived(context.Context, string, any) (*bool, error)  { v := false; return &v, nil }
func (*rrHost) ReadGoalStatus(context.Context, string) (any, error)     { return nil, nil }
func (*rrHost) ListTurnIDs(context.Context, string, int) ([]any, error) { return []any{}, nil }
func (*rrHost) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "accepted"}, {Key: "threadId", Value: thread}, {Key: "turnId", Value: "turn-" + thread + "-3"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "retrySafe", Value: false}}, nil
}
