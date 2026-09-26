package mergeturn

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Part C: every scenario here is replayed step by step against testdata/python_mergeturn.json,
// which gen_mergeturn.py records from the live Python MergeTurn over the MergeTurnTestCase
// fixture. A step is {"ok": <whole JSON answer>} or {"refused": {"reason", "detail"}}.

const (
	fxRepo = "owner/repo"
	fxBase = "dev"
	fxA    = "PRJ-A"
	fxB    = "PRJ-B"
	fxPost = "base-1"
)

var fxISO = registry.ISO(time.Unix(1_700_000_000, 0))

var pythonC = sync.OnceValues(func() (map[string][]map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", "python_mergeturn.json"))
	if err != nil {
		return nil, err
	}
	var out map[string][]map[string]any
	return out, json.Unmarshal(raw, &out)
})

// fakeTarget is tests/support.FakeTarget: a tip per (repository, base), every read counted.
type fakeTarget struct {
	mu    sync.Mutex
	tips  map[[2]string]string
	reads int
}

func (f *fakeTarget) set(repository, base, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tips[[2]string{repository, base}] = sha
}

func (f *fakeTarget) forget(repository, base string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tips, [2]string{repository, base})
}

func (f *fakeTarget) Tip(_ context.Context, repository, base string) (Tip, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	sha, ok := f.tips[[2]string{repository, base}]
	if !ok {
		return Tip{}, &TargetUnreadable{"the test set no tip for " + pyRepr(base) + " of " + pyRepr(repository)}
	}
	return Tip{SHA: sha, Source: "fake", Reference: "refs/heads/" + base, Repository: repository}, nil
}

type fx struct {
	t      *testing.T
	ctx    context.Context
	s      *store.Store
	r      *registry.Registry
	m      *Service
	target *fakeTarget
	steps  []map[string]any
}

func ep(task, host, cwd string) registry.Endpoint {
	return registry.Endpoint{TaskID: task, HostID: host, Cwd: sql.NullString{String: cwd, Valid: true}}
}

var (
	alpha    = ep("task-alpha", "host-a", "/alpha")
	beta     = ep("task-beta", "host-b", "/beta")
	overseer = ep("task-supervisor", "host-s", "/sup")
)

// newFx is MergeTurnTestCase.setUp.
func newFx(t *testing.T) *fx {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	now := func() string { return fxISO }
	r := &registry.Registry{Store: s, Now: now, Policy: registry.ResolveRolePolicy(map[string]string{})}
	w := &fx{t: t, ctx: ctx, s: s, r: r, target: &fakeTarget{tips: map[[2]string]string{}}}
	w.m = &Service{Store: s, Registry: r, Now: now}
	w.target.set(fxRepo, fxBase, "base-0")
	w.bindParent(fxA, alpha)
	w.bindParent(fxB, beta)
	return w
}

func (w *fx) bindParent(project string, endpoint registry.Endpoint) {
	w.t.Helper()
	if _, err := w.r.BindScope(w.ctx, "parent", project, endpoint); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.r.RegisterSupervision(w.ctx, "INIT-1", project, overseer, endpoint, "execution"); err != nil {
		w.t.Fatal(err)
	}
}

func (w *fx) must(v map[string]any, err error) map[string]any {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

// step records one call's outcome the way gen_mergeturn.World.step does.
func (w *fx) step(value any, err error) any {
	w.t.Helper()
	if err != nil {
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			w.t.Fatalf("unexpected error: %v", err)
		}
		w.steps = append(w.steps, map[string]any{"refused": map[string]any{"reason": refused.Reason, "detail": refused.Detail}})
		return nil
	}
	w.steps = append(w.steps, map[string]any{"ok": jsonValue(w.t, value)})
	return value
}

func jsonValue(t *testing.T, value any) any {
	t.Helper()
	var buf bytes.Buffer
	if err := contract.Emit(&buf, emitted(value)); err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// emitted is what the CLI prints: every map an object with sorted keys, every list []any.
func emitted(value any) any {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := contract.OrderedObject{}
		for _, k := range keys {
			o = append(o, contract.Field{Key: k, Value: emitted(v[k])})
		}
		return o
	case contract.OrderedObject:
		o := contract.OrderedObject{}
		for _, f := range v {
			o = append(o, contract.Field{Key: f.Key, Value: emitted(f.Value)})
		}
		return o
	case []contract.OrderedObject:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = emitted(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = emitted(item)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = emitted(item)
		}
		return out
	case int:
		return int64(v)
	}
	return value
}

func (w *fx) rows(query string, args ...any) []store.Row {
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
	w.steps = append(w.steps, map[string]any{"ok": jsonValue(w.t, list)})
	return rows
}

func (w *fx) reads() {
	w.target.mu.Lock()
	n := w.target.reads
	w.target.mu.Unlock()
	w.steps = append(w.steps, map[string]any{"ok": float64(n)})
}

func (w *fx) turn(id string) { w.step(w.m.Turn(w.ctx, id)) }

func (w *fx) contests() {
	key, _ := TargetKey(fxRepo, fxBase)
	w.step(w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, key))
}

func (w *fx) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *fx) checksRows(turn string) {
	w.rows("SELECT * FROM merge_turn_checks WHERE turn_id = ? ORDER BY recorded_at", turn)
}

func (w *fx) claimOn(endpoint registry.Endpoint, project, head, base string, ready bool) (map[string]any, error) {
	return w.m.Request(w.ctx, fxRepo, base, project, endpoint.TaskID, endpoint.HostID, head, ready)
}

func (w *fx) claim(endpoint registry.Endpoint, project, head string) map[string]any {
	w.t.Helper()
	return w.must(w.claimOn(endpoint, project, head, fxBase, true))
}

// answer is MergeTurnTestCase.answer_grant.
func (w *fx) answer(turn, actor string) {
	w.t.Helper()
	record := w.must(w.m.Turn(w.ctx, turn))
	grant, _ := record["grant"].(map[string]any)
	if grant == nil {
		return
	}
	w.must(w.m.Acknowledge(w.ctx, turn, actor, grant["grantId"].(string), "read the grant and re-checked the record"))
}

func (w *fx) heldOn(endpoint registry.Endpoint, project, head, base string) string {
	w.t.Helper()
	turn := w.must(w.claimOn(endpoint, project, head, base, true))["turnId"].(string)
	w.answer(turn, endpoint.TaskID)
	return turn
}

func (w *fx) held() string { return w.heldOn(alpha, fxA, "head-a", fxBase) }

func runChecks(head string, conclusion any, attempt int, name, run string) []any {
	return []any{contract.OrderedObject{{Key: "runId", Value: run}, {Key: "name", Value: name}, {Key: "headSha", Value: head}, {Key: "conclusion", Value: conclusion}, {Key: "attempt", Value: json.Number(fmt.Sprint(attempt))}}}
}

func green() contract.OrderedObject {
	return contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("1")}, {Key: "threadsSeen", Value: []any{"thread-1"}}, {Key: "unresolved", Value: json.Number("0")}}
}

// review is a dict literal in Python's insertion order.
func review(pairs ...any) contract.OrderedObject {
	o := contract.OrderedObject{}
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, contract.Field{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return o
}

type begin struct {
	actor, head, base string
	checks            []any
	review            any
	required          []string
}

func defaults() begin {
	return begin{actor: alpha.TaskID, head: "head-a", base: "base-0", checks: runChecks("head-a", "success", 1, "dev-gate", "run-1"), review: green(), required: []string{"dev-gate"}}
}

func (w *fx) begin(turn string, b begin) (map[string]any, error) {
	return w.m.Check(w.ctx, turn, b.actor, b.head, b.base, b.checks, b.review, b.required, w.target)
}

func (w *fx) check(turn, head, base, actor string) (map[string]any, error) {
	b := defaults()
	b.head, b.base, b.checks = head, base, runChecks(head, "success", 1, "dev-gate", "run-1")
	if actor != "" {
		b.actor = actor
	}
	return w.begin(turn, b)
}

func (w *fx) merging(head string) string {
	w.t.Helper()
	turn := w.heldOn(alpha, fxA, head, fxBase)
	w.must(w.check(turn, head, "base-0", ""))
	return turn
}

func (w *fx) merged(tip string) { w.target.set(fxRepo, fxBase, tip) }

func (w *fx) land(turn, landed, observed, actor string) (map[string]any, error) {
	if actor == "" {
		actor = alpha.TaskID
	}
	return w.m.Land(w.ctx, turn, actor, landed, observed, "merged; the base branch read afterwards", w.target)
}

func (w *fx) landed(head, tip string) string {
	w.t.Helper()
	turn := w.merging(head)
	w.merged(tip)
	w.must(w.land(turn, "merge-1", "", ""))
	return turn
}

func (w *fx) restate(turn, actor, observed, evidence string) (map[string]any, error) {
	if actor == "" {
		actor = alpha.TaskID
	}
	if evidence == "" {
		evidence = "read the branch again"
	}
	return w.m.RestateBase(w.ctx, turn, actor, observed, evidence, w.target)
}

func (w *fx) unknown() string {
	w.t.Helper()
	turn := w.merging("head-a")
	w.must(w.m.Unknown(w.ctx, turn, alpha.TaskID, "lost the connection"))
	return turn
}

func (w *fx) resolve(turn, observed, state, evidence, actor string) (map[string]any, error) {
	if actor == "" {
		actor = overseer.TaskID
	}
	return w.m.Resolve(w.ctx, turn, actor, observed, state, evidence, w.target)
}

func (w *fx) r3Landing(turn string) {
	w.exec("UPDATE merge_turns SET observed_base_sha = checked_base_sha WHERE turn_id = ?", turn)
}

func (w *fx) legacyRow(turn, kind, key, evidenceKind, evidence string, to any) {
	w.exec("INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state, evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		"legacy-"+key, turn, kind, "landed", to, evidenceKind, alpha.TaskID, evidence, key, fxISO)
}

var derivedTargetPattern = regexp.MustCompile(`tgt-[0-9a-f]{32}`)

// stableTargets preserves all target identity relationships across the complete answer.
func stableTargets(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	clean := derivedTargetPattern.ReplaceAllStringFunc(string(raw), func(key string) string {
		if name, ok := names[key]; ok {
			return name
		}
		name := fmt.Sprintf("<target-key-%d>", len(names)+1)
		names[key] = name
		return name
	})
	var out any
	if err := json.Unmarshal([]byte(clean), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameAsPython compares every recorded step's whole JSON with the named Python scenario.
func (w *fx) sameAsPython(name string) []map[string]any {
	w.t.Helper()
	all, err := pythonC()
	if err != nil {
		w.t.Fatal(err)
	}
	want, ok := all[name]
	if !ok {
		w.t.Fatalf("no python scenario %q", name)
	}
	if len(w.steps) != len(want) {
		g, _ := json.MarshalIndent(w.steps, "", " ")
		w.t.Fatalf("%s: %d steps, python has %d\n go: %s", name, len(w.steps), len(want), g)
	}
	steps := stableTargets(w.t, w.steps).([]any)
	for i := range want {
		if !reflect.DeepEqual(steps[i], want[i]) {
			g, _ := json.MarshalIndent(w.steps[i], "", " ")
			p, _ := json.MarshalIndent(want[i], "", " ")
			w.t.Errorf("%s step %d differs from Python\n go: %s\n py: %s", name, i, g, p)
		}
	}
	return want
}

func reasonOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return fmt.Sprint(err)
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// readFixture reads one of the Python suite's shared fixture files.
func readFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "..", "..", "packages", "codex-session-relay", "tests", "fixtures", name))
}
