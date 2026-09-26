package capacity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fixture of test_capacity.py's CapacityTestCase: two parents, each under one supervisor
// of INIT-1, on a FakeClock at 1_700_000_000.
const (
	projectA   = "PRJ-A"
	projectB   = "PRJ-B"
	assignment = "assignment"
	alpha      = "task-alpha"
	beta       = "task-beta"
	supervisor = "task-supervisor"
)

type clock struct{ now time.Time }

func (c *clock) iso() string { return registry.ISO(c.now) }

type env struct {
	t     *testing.T
	store *store.Store
	clock *clock
	cap   *Capacity
	steps []map[string]any
}

func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

func newEnv(t *testing.T) *env {
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
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	e := &env{t: t, store: s, clock: c, cap: &Capacity{Store: s, Now: c.iso}}
	r := &registry.Registry{Store: s, Now: c.iso}
	ctx := context.Background()
	for _, b := range []struct{ role, key, task, host, cwd string }{
		{"parent", projectA, alpha, "host-a", "/alpha"}, {"parent", projectB, beta, "host-b", "/beta"},
		{"supervisor", "INIT-1", supervisor, "host-s", "/sup"},
	} {
		if _, err := r.BindScope(ctx, b.role, b.key, registry.Endpoint{TaskID: b.task, HostID: b.host, Cwd: ns(b.cwd)}); err != nil {
			t.Fatal(err)
		}
	}
	// linkage.register_supervision's link row (todo 26 ports the method itself).
	for _, p := range [][2]string{{projectA, alpha}, {projectB, beta}} {
		sum := sha256.Sum256([]byte("execution|initiative|INIT-1|project|" + p[0]))
		if err := s.InsertScopeLink(ctx, store.ScopeLinksRow{
			LinkID: "lnk-" + hex.EncodeToString(sum[:])[:32], LinkKind: "execution", UpperKind: "initiative", UpperKey: "INIT-1",
			UpperTaskID: supervisor, LowerKind: "project", LowerKey: p[0], LowerTaskID: p[1], Status: "active", Revision: 1,
			CreatedAt: c.iso(), UpdatedAt: c.iso(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func ctx() context.Context { return context.Background() }

// step records one answer as gen_capacity.py does: the json.dumps(indent=2) text or the refusal.
func (e *env) step(value any, err error) any {
	e.t.Helper()
	if err != nil {
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			e.t.Fatalf("unexpected error: %v", err)
		}
		e.steps = append(e.steps, map[string]any{"refused": map[string]any{"reason": refused.Reason, "detail": refused.Detail}})
		return nil
	}
	var b bytes.Buffer
	if err := contract.Emit(&b, value); err != nil {
		e.t.Fatal(err)
	}
	e.steps = append(e.steps, map[string]any{"ok": strings.TrimSuffix(b.String(), "\n")})
	return value
}

func (e *env) take(subject string, who ...string) any {
	endpoint, project := alpha, projectA
	if len(who) == 2 {
		endpoint, project = who[0], who[1]
	}
	return e.step(e.cap.Reserve(ctx(), Reservation{SubjectKind: assignment, SubjectKey: subject, ParentTask: endpoint, Project: project, ReservedBy: endpoint}))
}

func (e *env) giveBack(subject, reason, by string, tenure int64) any {
	named := sql.NullInt64{Int64: tenure, Valid: tenure != 0}
	return e.step(e.cap.Release(ctx(), Release{SubjectKind: assignment, SubjectKey: subject, ReleasedBy: by, Reason: reason, Tenure: named}))
}

func (e *env) ceiling(dimension string, amount float64, kind, scope, unit string, enforce bool, by string) any {
	if by == "" {
		by = supervisor
		if kind == "project" {
			by = alpha
		}
	}
	return e.step(e.cap.DeclareLimit(ctx(), Limit{ScopeKind: kind, ScopeKey: scope, Dimension: dimension, Unit: unit, Ceiling: amount,
		DeclaredBy: by, Source: "operator", Enforce: enforce}))
}

func (e *env) observe(dimension string, value float64, kind, scope, by, method string) any {
	return e.step(e.cap.Observe(ctx(), Observation{ScopeKind: kind, ScopeKey: scope, Dimension: dimension, Observed: value, ObservedBy: by, Method: method}))
}

func (e *env) report() any { return e.step(e.cap.Report(ctx(), ReportFilter{})) }

func (e *env) headroom(kind, scope string) any { return e.step(e.cap.Headroom(ctx(), kind, scope)) }

func (e *env) conflicts(subject string) any {
	return e.step(Conflicts(ctx(), e.store, domainExecution, subject))
}

// rows is [dict(row) for row in store.all(sql)], column order kept.
func (e *env) rows(query string, args ...any) any {
	all, err := e.store.All(ctx(), query, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []any{}
	for _, row := range all {
		object := contract.OrderedObject{}
		for _, column := range row {
			object = append(object, contract.Field{Key: column.Name, Value: column.Value})
		}
		out = append(out, object)
	}
	return e.step(out, nil)
}

var python = sync.OnceValues(func() (map[string][]map[string]any, error) {
	raw, err := os.ReadFile("testdata/python_capacity.json")
	if err != nil {
		return nil, err
	}
	var out map[string][]map[string]any
	return out, json.Unmarshal(raw, &out)
})

// sameAsPython compares every recorded step, whole, with Python's text for the same call.
func (e *env) sameAsPython(name string) {
	e.t.Helper()
	all, err := python()
	if err != nil {
		e.t.Fatal(err)
	}
	want, ok := all[name]
	if !ok {
		e.t.Fatalf("no python scenario %q", name)
	}
	if len(e.steps) != len(want) {
		e.t.Fatalf("%s: %d steps, python has %d", name, len(e.steps), len(want))
	}
	for i := range want {
		g, _ := json.Marshal(e.steps[i])
		w, _ := json.Marshal(want[i])
		if !bytes.Equal(g, w) {
			e.t.Errorf("%s step %d differs from Python\n go: %v\n py: %v", name, i, e.steps[i], want[i])
		}
	}
}

// field reads one key of an answer the test just recorded.
func field(value any, key string) any {
	for _, f := range value.(contract.OrderedObject) {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func (e *env) reason(i int) string {
	e.t.Helper()
	refused, ok := e.steps[i]["refused"].(map[string]any)
	if !ok {
		e.t.Fatalf("step %d was not refused: %v", i, e.steps[i])
	}
	return refused["reason"].(string)
}

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }

func reasonOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// openAgain is Store(self.store.path): a second Store on the same database, closed at cleanup.
func openAgain(t *testing.T, e *env) *Capacity {
	t.Helper()
	s, err := store.Open(context.Background(), e.store.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &Capacity{Store: s, Now: e.clock.iso}
}
