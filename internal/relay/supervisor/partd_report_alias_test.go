package supervisor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
)

type rrOperation struct {
	Kind   string         `json:"kind"`
	Args   map[string]any `json:"args"`
	Now    float64        `json:"now"`
	Pre    string         `json:"pre"`
	Output string         `json:"output"`
	Tables string         `json:"tables"`
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
func rrCompare(t *testing.T, field string, got []byte, want string) {
	t.Helper()
	if bytes.Equal(got, []byte(want)) {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	start := max(0, i-120)
	t.Errorf("%s byte comparison diff at offset %d\nGo: %q\nPython: %q", field, i, got[start:min(len(got), i+260)], want[start:min(len(want), i+260)])
}
func rrTables(t *testing.T, s *store.Store) []byte {
	t.Helper()
	ctx := context.Background()
	names, err := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]any{}
	for _, n := range names {
		name := n.Get("name").(string)
		rows, e := s.All(ctx, `SELECT * FROM "`+name+`" ORDER BY rowid`)
		if e != nil {
			t.Fatal(e)
		}
		out := []any{}
		for _, r := range rows {
			m := map[string]any{}
			for _, c := range r {
				m[c.Name] = c.Value
			}
			out = append(out, m)
		}
		tables[name] = out
	}
	rrOwnerNeutral(t, testsupport.Go, tables)
	return rrBytes(t, tables)
}

// rrOwnerNeutral applies the documented runtime-identity rule, testsupport.OwnerNeutral, to the
// schema_meta rows of a table dump of a store writer stamped: the owner row of a store each
// runtime stamped as its own reads "python" in one and "go" in the other, and must read writer's.
// Nothing else in the dump is touched.
func rrOwnerNeutral(t *testing.T, writer testsupport.Runtime, tables map[string]any) {
	t.Helper()
	rows, _ := tables["schema_meta"].([]any)
	for _, row := range rows {
		if cells, ok := row.(map[string]any); ok {
			if key, ok := cells["key"].(string); ok {
				cells["value"] = testsupport.OwnerNeutral(t, writer, key, cells["value"])
			}
		}
	}
}

// rrPythonTables is Python's captured table dump under the same rule. The dump is decoded and
// re-encoded by the emitter that renders Go's, which must first reproduce it byte for byte, so
// the owner row is all that can change.
func rrPythonTables(t *testing.T, raw string) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var tables map[string]any
	if err := decoder.Decode(&tables); err != nil {
		t.Fatal(err)
	}
	if again := rrBytes(t, tables); !bytes.Equal(again, []byte(raw)) {
		rrCompare(t, "re-encoded Python tables", again, raw)
		t.Fatal("Python's table dump does not re-encode byte for byte")
	}
	rrOwnerNeutral(t, testsupport.Python, tables)
	return string(rrBytes(t, tables))
}
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
	// capture.json and every operation's pre-N.sqlite3 snapshot are recorded
	// (supervisor.PythonTree); Python's final store is not, as no operation reads it.
	supervisor.PythonTree(t, module+" "+method, root, func() ([]byte, error) {
		repo, err := filepath.Abs("../../..")
		if err != nil {
			return nil, err
		}
		script := filepath.Join(repo, "internal/relay/supervisor/testdata/res_rcf_capture.py")
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, root, module, method)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "XDG_DATA_HOME="+root, "XDG_CONFIG_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay"))
		if out, e := cmd.CombinedOutput(); e != nil {
			return nil, fmt.Errorf("live Python %s: %v\n%s", method, e, out)
		}
		return nil, supervisor.RemoveStoreFiles(filepath.Join(root, "tree", "state", "relay.sqlite3"))
	})
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Operations []rrOperation `json:"operations"`
		Problems   []string      `json:"problems"`
	}
	// Decode the transport envelope, never the expected output/table byte strings.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Problems) > 0 || len(capture.Operations) == 0 {
		t.Fatalf("invalid capture: %v", capture.Problems)
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

func rrReplay(t *testing.T, root string, op rrOperation) {
	t.Helper()
	ctx := context.Background()
	// Each operation replays on its own copy of the Python snapshot, in a directory of its own,
	// stamped as Go's own store (Restamp): the tables below include schema_meta, which then
	// differs from Python's only in the owner row that rrOwnerNeutral neutralizes.
	path := filepath.Join(t.TempDir(), "go.sqlite3")
	data, err := os.ReadFile(op.Pre)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Restamp(t, path, "go")
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
		testsupport.Restamp(t, filepath.Join(state, "relay.sqlite3"), "go")
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
		rrCompare(t, "stdout", stdout.Bytes(), op.Output)
		if stderr.Len() != 0 {
			t.Errorf("unexpected stderr (exit %d): %s", code, stderr.Bytes())
		}
		cliStore, e := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		rrCompare(t, "tables", rrTables(t, cliStore), rrPythonTables(t, op.Tables))
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
	rrCompare(t, "output", rrBytes(t, output), op.Output)
	rrCompare(t, "tables", rrTables(t, s), rrPythonTables(t, op.Tables))
}
func rrRun(t *testing.T, module string, methods []string) {
	t.Helper()
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			root, ops := rrCapture(t, module, method)
			for i, op := range ops {
				if op.Kind == "report" || op.Kind == "read" {
					// Python's report.record and its read-back are recorded but not replayed: no
					// product path writes a work report (decision 53). Each operation runs on its
					// own snapshot, so the others do not depend on them.
					continue
				}
				t.Run(fmt.Sprintf("%02d_%s", i, op.Kind), func(t *testing.T) { rrReplay(t, root, op) })
			}
		})
	}
}

// The source fixture's child has no turns before its revision is delivered. This
// adapter preserves FakeHostAdapter's successful send receipt and lifecycle facts.
type rrHost struct{ delivery.Adapter }

func (*rrHost) ReadThread(string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &yes}, nil
}
func (*rrHost) IsArchived(string, any) (*bool, error)  { v := false; return &v, nil }
func (*rrHost) ReadGoalStatus(string) (any, error)     { return nil, nil }
func (*rrHost) ListTurnIDs(string, int) ([]any, error) { return []any{}, nil }
func (*rrHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	return delivery.Obj{{Key: "requestId", Value: id}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: "accepted"}, {Key: "threadId", Value: thread}, {Key: "turnId", Value: "turn-" + thread + "-3"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "retrySafe", Value: false}}, nil
}
