package linkage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Fixture identities, as tests/support.py and test_linkage.py.
const (
	parent          = testsupport.Parent
	child           = testsupport.Child
	issue           = testsupport.Issue
	host            = testsupport.Host
	dispatchTurn    = testsupport.DispatchTurn
	root            = "/work"
	initiative      = "INIT-1"
	project         = "PROJ-1"
	otherProject    = "PROJ-2"
	otherInitiative = "INIT-2"
	supervisorTask  = "01supervisor-task"
	otherSupervisor = "01supervisor-two"
	otherParent     = "01parent-two"
)

var fakeISO = registry.ISO(time.Unix(1_700_000_000, 0))

func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

// world is one scenario's store, registry and recorded steps, as gen_linkage.World.
type world struct {
	t     *testing.T
	ctx   context.Context
	path  string
	s     *store.Store
	r     *registry.Registry
	steps []map[string]any
}

func newWorld(t *testing.T) *world {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &world{t: t, ctx: context.Background(), path: path, s: s,
		r: &registry.Registry{Store: s, Now: func() string { return fakeISO }, Policy: registry.ResolveRolePolicy(map[string]string{})}}
}

// step records one call's outcome the way gen_linkage.World.step does.
func (w *world) step(value any, err error) any {
	w.t.Helper()
	if err != nil {
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			w.t.Fatalf("unexpected error: %v", err)
		}
		w.steps = append(w.steps, map[string]any{"refused": map[string]any{"reason": refused.Reason, "detail": refused.Detail}})
		return nil
	}
	w.steps = append(w.steps, map[string]any{"ok": decode(w.t, value)})
	return value
}

func (w *world) must(value any, err error) any {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return value
}

// rows records a table read as a list of row objects.
func (w *world) rows(query string, args ...any) []store.Row {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, query, args...)
	if err != nil {
		w.t.Fatal(err)
	}
	list := []any{}
	for _, row := range rows {
		o := contract.OrderedObject{}
		for _, c := range row {
			o = append(o, contract.Field{Key: c.Name, Value: c.Value})
		}
		list = append(list, o)
	}
	w.steps = append(w.steps, map[string]any{"ok": decode(w.t, list)})
	return rows
}

func (w *world) conflicts() []store.Row { return w.rows("SELECT * FROM linkage_conflicts ORDER BY id") }

func (w *world) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatal(err)
	}
}

func decode(t *testing.T, value any) any {
	t.Helper()
	var buf bytes.Buffer
	switch v := value.(type) {
	case contract.OrderedObject, []any, nil:
		if err := contract.Emit(&buf, v); err != nil {
			t.Fatal(err)
		}
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
	}
	var out any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func supervisorEP(task string) registry.Endpoint {
	return registry.Endpoint{TaskID: task, HostID: host, Cwd: ns("/supervisor"), CXCSession: ns("cxc-supervisor")}
}

func parentEP(task string) registry.Endpoint {
	return registry.Endpoint{TaskID: task, HostID: host, Cwd: ns("/parent"), CXCSession: ns("cxc-parent")}
}

func bare(task string) registry.Endpoint { return registry.Endpoint{TaskID: task, HostID: host} }

type supervision struct {
	initiative, project string
	supervisor, parent  registry.Endpoint
	kind                string
}

func (w *world) supervision() supervision {
	return supervision{initiative, project, supervisorEP(supervisorTask), parentEP(parent), "execution"}
}

func (w *world) supervise(s supervision) (contract.OrderedObject, error) {
	return w.r.RegisterSupervision(w.ctx, s.initiative, s.project, s.supervisor, s.parent, s.kind)
}

func (w *world) superviseDefault() contract.OrderedObject {
	w.t.Helper()
	link, err := w.supervise(w.supervision())
	if err != nil {
		w.t.Fatal(err)
	}
	return link
}

// register is RelayTestCase.register: the fixture, with both settings recorded.
func (w *world) register(options ...func(*registry.Registration)) string {
	w.t.Helper()
	in := registry.Registration{Parent: parentEP(parent),
		Child:    registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root), CXCSession: ns("cxc-child")},
		IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
		DispatchRequestID: "dispatch-1", DispatchTurnID: ns(dispatchTurn)}
	for _, o := range options {
		o(&in)
	}
	x, err := w.r.Register(w.ctx, in)
	if err != nil {
		w.t.Fatal(err)
	}
	for task, cwd := range map[string]string{parent: "/parent", child: root} {
		if _, err := w.r.RecordSettings(w.ctx, task, testsupport.TaskSettings(cwd), "creation_result", "", registry.Citation{}); err != nil {
			w.t.Fatal(err)
		}
	}
	return x.ID
}

// registerRaw is registry.register with exactly these arguments and no settings.
func (w *world) registerRaw(in registry.Registration) (string, error) {
	x, err := w.r.Register(w.ctx, in)
	return x.ID, err
}

func (w *world) scoped() string {
	w.t.Helper()
	w.superviseDefault()
	rid := w.register()
	w.must(w.r.AttachIssue(w.ctx, rid, project))
	return rid
}

func (w *world) setStatus(rid, status string) {
	w.t.Helper()
	if _, err := w.r.SetStatus(w.ctx, rid, status, "test"); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) resume(rid string) (any, error) {
	_, err := w.r.Resume(w.ctx, rid, 1, []string{root}, []string{parent}, "test")
	return nil, err
}

func (w *world) owner(kind, key string) contract.OrderedObject {
	w.t.Helper()
	o, err := w.r.Owner(w.ctx, kind, key)
	if err != nil {
		w.t.Fatal(err)
	}
	return o
}

func (w *world) ownerStep(kind, key string) any {
	o, err := w.r.Owner(w.ctx, kind, key)
	if o == nil {
		return w.step(nil, err)
	}
	return w.step(o, err)
}

// python is testdata/python_linkage.json.
var python = pythonFile("testdata/python_linkage.json")

// pythonFile loads one recorded oracle file once: scenario name -> steps.
func pythonFile(path string) func() (map[string][]map[string]any, error) {
	return sync.OnceValues(func() (map[string][]map[string]any, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var out map[string][]map[string]any
		return out, json.Unmarshal(raw, &out)
	})
}

// sameAsPython compares every recorded step's whole JSON with Python's.
func (w *world) sameAsPython(name string) []map[string]any {
	w.t.Helper()
	return w.sameAs(python, name)
}

// sameAs compares every recorded step's whole JSON with the named scenario of one oracle file.
func (w *world) sameAs(oracle func() (map[string][]map[string]any, error), name string) []map[string]any {
	w.t.Helper()
	all, err := oracle()
	if err != nil {
		w.t.Fatal(err)
	}
	want, ok := all[name]
	if !ok {
		w.t.Fatalf("no python scenario %q", name)
	}
	if len(w.steps) != len(want) {
		w.t.Fatalf("%s: %d steps, python has %d\n go: %v", name, len(w.steps), len(want), w.steps)
	}
	for i := range want {
		if !reflect.DeepEqual(w.steps[i], want[i]) {
			g, _ := json.MarshalIndent(w.steps[i], "", " ")
			p, _ := json.MarshalIndent(want[i], "", " ")
			w.t.Errorf("%s step %d differs from Python\n go: %s\n py: %s", name, i, g, p)
		}
	}
	return want
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func field(o contract.OrderedObject, key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func reasonOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

func nullString() sql.NullString { return sql.NullString{} }

// runCLI runs one relay command in-process against the world's store, as
// `codex-session-relay --state <dir> <argv...>`.
func runCLI(t *testing.T, w *world, argv ...string) (int, string, string) {
	t.Helper()
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "")
	var stdout, stderr bytes.Buffer
	code := registry.Execute(w.ctx, append([]string{"--state", filepath.Dir(w.path)}, argv...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func decodeStdout(t *testing.T, stdout string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("%v: %q", err, stdout)
	}
	return out
}

// sameJSON reports whether two values encode to the same JSON.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	return reflect.DeepEqual(decode(t, a), decode(t, b))
}
