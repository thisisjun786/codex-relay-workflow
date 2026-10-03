package managed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const scopeProject = "P-SCOPE"

// callRecord is the fake host with a record of every host effect the engine asks for, in order.
// The embedded fake answers; only the two effects (CreateThread, SendMessage) are written down,
// because they are what a refused request must never reach.
type callRecord struct {
	*managedFake
	calls []string
}

func (c *callRecord) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	c.calls = append(c.calls, "CreateThread")
	return c.managedFake.CreateThread(ctx, in)
}

func (c *callRecord) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	c.calls = append(c.calls, "SendMessage")
	return c.managedFake.SendMessage(ctx, in)
}

func (c *callRecord) count(call string) int {
	n := 0
	for _, seen := range c.calls {
		if seen == call {
			n++
		}
	}
	return n
}

// scopeRun is one managed start over a fresh store for a request that names scopeProject.
type scopeRun struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	host  *callRecord
	start *Start
	reg   *registry.Registry
	raw   []byte
}

func newScopeRun(t *testing.T) *scopeRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var request map[string]any
	if err := json.Unmarshal(requestFixture(t), &request); err != nil {
		t.Fatal(err)
	}
	request["projectKey"] = scopeProject
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	fake := &managedFake{operations: map[string]map[string]any{}, settings: pyjson.Map(pyjson.Map(parsed["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	host := &callRecord{managedFake: fake}
	now := func() string { return "2026-09-26T00:00:00.000000+00:00" }
	return &scopeRun{
		t: t, ctx: ctx, store: s, host: host, raw: raw,
		start: &Start{Store: s, Adapter: host, Now: now, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }},
		reg:   &registry.Registry{Store: s, Now: now},
	}
}

// bind registers task as the parent of scopeProject, which is what linkage-bind --role parent does.
func (x *scopeRun) bind(task string) {
	x.t.Helper()
	if _, err := x.reg.BindScopeAs(x.ctx, "parent", scopeProject, registry.Endpoint{TaskID: task, HostID: "host"}, "active"); err != nil {
		x.t.Fatal(err)
	}
}

func (x *scopeRun) unbind() {
	x.t.Helper()
	if _, err := x.store.DB.ExecContext(x.ctx, "DELETE FROM scope_bindings WHERE scope_kind = 'project' AND scope_key = ?", scopeProject); err != nil {
		x.t.Fatal(err)
	}
}

func (x *scopeRun) run() map[string]any {
	x.t.Helper()
	result, err := x.start.Run(x.ctx, x.raw)
	if err != nil {
		x.t.Fatalf("managed start refused: %v", err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	return got
}

func (x *scopeRun) requestRow() store.ManagedStartRequestsRow {
	x.t.Helper()
	row, err := x.store.ManagedStartRequest(x.ctx, "managed-1")
	if err != nil {
		x.t.Fatal(err)
	}
	return row
}

func (x *scopeRun) count(table string) int {
	x.t.Helper()
	var n int
	if err := x.store.DB.QueryRowContext(x.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		x.t.Fatal(err)
	}
	return n
}

// A project with no bound parent is refused as unregistered_scope before the host is asked for
// anything, and the reservation it leaves was never armed, so managed-release can release it.
func TestScopePrecheck_UnboundProjectIsRefusedBeforeCreation(t *testing.T) {
	x := newScopeRun(t)
	_, err := x.start.Run(x.ctx, x.raw)
	reasonIs(t, err, "unregistered_scope")
	if x.host.count("CreateThread") != 0 || x.host.count("SendMessage") != 0 {
		t.Fatalf("a refused request reached the host: %v", x.host.calls)
	}
	row := x.requestRow()
	if row.State != "reserved" || x.count("relationships") != 0 {
		t.Fatalf("refused request left %s, %d relationships", row.State, x.count("relationships"))
	}
	conflicts, err := x.reg.Conflicts(x.ctx, "project", scopeProject)
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("conflicts %v %v", conflicts, err)
	}
	released, err := (Reservation{Store: x.store, Now: x.start.now}).Release(x.ctx, row.RequestID, row.RequestFingerprint, row.Revision, "scope refused")
	if err != nil || released.State != "released" {
		t.Fatalf("the unarmed reservation was not releasable: %v %v", released.State, err)
	}
}

// A project another task is the parent of is refused the same way.
func TestScopePrecheck_ProjectHeldByAnotherParentIsRefusedBeforeCreation(t *testing.T) {
	x := newScopeRun(t)
	x.bind("other-parent")
	_, err := x.start.Run(x.ctx, x.raw)
	reasonIs(t, err, "foreign_scope")
	if len(x.host.calls) != 0 {
		t.Fatalf("a refused request reached the host: %v", x.host.calls)
	}
}

func TestScopePrecheck_BoundProjectIsAdmittedAndReplayed(t *testing.T) {
	x := newScopeRun(t)
	x.bind("parent")
	got := x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 || x.host.count("SendMessage") != 1 {
		t.Fatalf("bound start: %v %v", got, x.host.calls)
	}
	got = x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 || x.host.count("SendMessage") != 1 {
		t.Fatalf("replay: %v %v", got, x.host.calls)
	}
}

// The refusal is not final: once the parent is bound, the same request id creates its one child.
func TestScopePrecheck_RetryAfterBindingCreatesTheChildOnce(t *testing.T) {
	x := newScopeRun(t)
	if _, err := x.start.Run(x.ctx, x.raw); err == nil {
		t.Fatal("unbound project accepted")
	}
	if x.host.count("CreateThread") != 0 {
		t.Fatalf("the refused request created a thread: %v", x.host.calls)
	}
	x.bind("parent")
	got := x.run()
	if got["state"] != "admitted" || got["childTaskId"] != "child-new" || x.host.count("CreateThread") != 1 || x.host.count("SendMessage") != 1 {
		t.Fatalf("retry: %v %v", got, x.host.calls)
	}
}

// A retry that continues an existing child is not an attempt to create one.
func TestScopePrecheck_RetryContinuesTheExistingChild(t *testing.T) {
	x := newScopeRun(t)
	x.bind("parent")
	x.host.standby = "inProgress"
	got := x.run()
	if got["reason"] != "standby_incomplete" || x.host.count("CreateThread") != 1 {
		t.Fatalf("first run: %v %v", got, x.host.calls)
	}
	x.host.standby = "completed"
	got = x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 || x.host.count("SendMessage") != 1 {
		t.Fatalf("retry: %v %v", got, x.host.calls)
	}
}

// An armed request that already has a creation result keeps the answer it had: the early check
// belongs to the moment before a host effect, so removing the binding afterwards changes nothing.
func TestScopePrecheck_ArmedRetryKeepsItsCreationAnswer(t *testing.T) {
	x := newScopeRun(t)
	x.bind("parent")
	x.host.creationEnvironmentChanged = true
	first := x.run()
	if first["reason"] != "creation_settings_unverified" || x.host.count("CreateThread") != 1 {
		t.Fatalf("first run: %v %v", first, x.host.calls)
	}
	x.unbind()
	second := x.run()
	if second["reason"] != "creation_settings_unverified" || x.host.count("CreateThread") != 1 {
		t.Fatalf("retry changed its answer: %v %v", second, x.host.calls)
	}
}

// The check is repeated right before the effect: a binding removed after the reservation was
// armed still stops the creation, and the request stays armed and retryable with its own id.
func TestScopePrecheck_BindingRemovedBeforeCreationStopsIt(t *testing.T) {
	x := newScopeRun(t)
	x.bind("parent")
	removed := false
	x.host.onGetOperation = func(id string) {
		if strings.HasPrefix(id, "managed-create-") && !removed {
			removed = true
			x.unbind()
		}
	}
	_, err := x.start.Run(x.ctx, x.raw)
	reasonIs(t, err, "unregistered_scope")
	if x.host.count("CreateThread") != 0 || x.requestRow().State != "create_armed" {
		t.Fatalf("armed request: %v %s", x.host.calls, x.requestRow().State)
	}
	x.bind("parent")
	got := x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 {
		t.Fatalf("retry after rebinding: %v %v", got, x.host.calls)
	}
}
