package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// markerOps runs one list of marker/intent operations through the real Python modules
// (testdata/markerops.py) and through this package over the SAME tree path, one after the
// other, and returns both answer lists. Sharing the path keeps workspace keys, assignment
// directories and every printed path identical, so the lists compare whole.
type markerOp = map[string]any

func runMarkerOps(t *testing.T, env map[string]any, ops []markerOp) (python, golang []any) {
	t.Helper()
	tree := t.TempDir()
	spec, err := json.Marshal(map[string]any{"ops": ops, "env": env})
	mustDo(t, err)
	root := repoRoot(t)
	script, _ := filepath.Abs("testdata/markerops.py")
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, tree)
	cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "CODEX_SESSION_RELAY_MARKER_ROOT=")
	cmd.Stdin = strings.NewReader(string(spec))
	output, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("python marker ops: %v\n%s", err, stderr)
	}
	mustDo(t, json.Unmarshal(output, &python))
	entries, err := os.ReadDir(tree)
	mustDo(t, err)
	for _, entry := range entries {
		mustDo(t, os.RemoveAll(filepath.Join(tree, entry.Name())))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xs"))
	t.Setenv(MarkerEnv, "")
	for name, value := range env {
		setEnv(t, name, value, tree)
	}
	d := &markerDriver{t: t, tree: tree, ctx: context.Background()}
	defer d.close()
	mustDo(t, os.MkdirAll(d.work(), 0o755))
	for _, op := range ops {
		golang = append(golang, d.answer(op))
	}
	return python, golang
}

func setEnv(t *testing.T, name string, value any, tree string) {
	t.Setenv(name, "")
	if value == nil {
		mustDo(t, os.Unsetenv(name))
		return
	}
	mustDo(t, os.Setenv(name, strings.ReplaceAll(value.(string), "<tree>", tree)))
}

type markerDriver struct {
	t    *testing.T
	tree string
	ctx  context.Context
	s    *store.Store
	held *sql.Conn
}

func (d *markerDriver) root() string { return filepath.Join(d.tree, "markers") }
func (d *markerDriver) work() string { return filepath.Join(d.tree, "work") }
func (d *markerDriver) path(v any) string {
	text, _ := v.(string)
	return strings.ReplaceAll(text, "<tree>", d.tree)
}

func (d *markerDriver) close() {
	if d.held != nil {
		_ = d.held.Close()
	}
	if d.s != nil {
		_ = d.s.Close()
	}
}

func (d *markerDriver) store() *store.Store {
	if d.s == nil {
		s, err := store.Open(d.ctx, filepath.Join(d.tree, "state", "relay.sqlite3"), "")
		mustDo(d.t, err)
		d.s = s
	}
	return d.s
}

func opString(op markerOp, key, fallback string) string {
	if v, ok := op[key].(string); ok {
		return v
	}
	return fallback
}

func (d *markerDriver) assignment(op markerOp) any {
	if v, ok := op["assignment"]; ok {
		return v
	}
	return AssignmentID(opString(op, "dispatch", "dispatch-request-1"))
}

func (d *markerDriver) adir(op markerOp) string {
	dir, err := AssignmentDir(d.root(), d.work(), AssignmentID(opString(op, "dispatch", "dispatch-request-1")))
	mustDo(d.t, err)
	return dir
}

// fromJSON turns a decoded JSON value into this package's Obj/[]any/int64 shapes.
func fromJSON(v any) any {
	raw, _ := json.Marshal(v)
	out, _ := loads(string(raw))
	return out
}

func (d *markerDriver) answer(op markerOp) any {
	value, err := d.run(op)
	var refused *store.RefusedError
	var host *hostError
	switch {
	case errors.As(err, &refused):
		return map[string]any{"reason": refused.Reason, "detail": refused.Detail}
	case errors.As(err, &host):
		return map[string]any{"error": host.Error()}
	case err != nil:
		d.t.Fatalf("op %v: %v", op, err)
	}
	return map[string]any{"ok": value}
}

func (d *markerDriver) run(op markerOp) (any, error) {
	const t0 = "2026-01-01T00:00:00+00:00"
	at := opString(op, "at", t0)
	switch op["op"] {
	case "declare":
		decl := IntentDeclaration{Workspace: d.work(), DispatchRequestID: opString(op, "dispatch", "dispatch-request-1"), IssueKey: opString(op, "issue_key", "REL-1"), DeclaredAt: opString(op, "declared_at", t0),
			CriteriaSource: op["criteria_source"], BaselineRevision: op["baseline_revision"], AuthorizedSettings: fromJSON(op["authorized_settings"])}
		if w, ok := op["workspace"]; ok {
			decl.Workspace = d.path(w)
		}
		if p, ok := op["db_path"]; ok {
			decl.DBPath = d.path(p)
		}
		return DeclareIntent(d.root(), decl)
	case "attempt":
		return RecordAttempt(d.root(), d.work(), d.assignment(op), op["outcome"].(string), at, op["task_id"])
	case "claim":
		return PublishClaim(d.root(), d.work(), d.assignment(op), op["session"], opString(op, "claim_dispatch", "dispatch-request-1"), "turn-1", at)
	case "bind":
		return BindIdentity(d.root(), d.work(), d.assignment(op), op["session"], op["task"], at)
	case "open_generation":
		rid := op["relationship_id"].(string)
		generation := int64(1)
		if g, ok := op["generation"]; ok {
			generation = fromJSON(g).(int64)
		}
		current := generation
		c, hasCurrent := op["current"]
		if hasCurrent {
			current = fromJSON(c).(int64)
		}
		s := d.store()
		_, err := execSQL(d.ctx, s, "INSERT OR IGNORE INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			rid, "REL-1", "active", "01parent-task", "host-a", "01child-task", "host-a", current, "[]", "[]", t0, t0)
		mustDo(d.t, err)
		if hasCurrent {
			_, err = execSQL(d.ctx, s, "UPDATE relationships SET execution_generation = ? WHERE relationship_id = ?", current, rid)
			mustDo(d.t, err)
		}
		_, err = execSQL(d.ctx, s, "INSERT OR IGNORE INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, opened_at) VALUES (?,?,?,?,?)", rid, generation, opString(op, "generation_dispatch", "dispatch-request-1"), "bound", t0)
		mustDo(d.t, err)
		return nil, nil
	case "register":
		return RegisterRelationship(d.ctx, d.root(), d.work(), d.assignment(op), op["relationship_id"].(string), opString(op, "register_dispatch", "dispatch-request-1"), t0, d.path(opString(op, "db_path", "<tree>/state/relay.sqlite3")))
	case "hold_begin":
		conn, err := d.store().DB.Conn(d.ctx)
		mustDo(d.t, err)
		_, err = conn.ExecContext(d.ctx, "BEGIN IMMEDIATE")
		mustDo(d.t, err)
		d.held = conn
		return nil, nil
	case "hold_end":
		_, err := d.held.ExecContext(d.ctx, "ROLLBACK")
		mustDo(d.t, err)
		mustDo(d.t, d.held.Close())
		d.held = nil
		return nil, nil
	case "relay_tables":
		s := d.store()
		relationships, err := all(d.ctx, s, "SELECT * FROM relationships")
		mustDo(d.t, err)
		generations, err := all(d.ctx, s, "SELECT * FROM generations")
		mustDo(d.t, err)
		journal, err := one(d.ctx, s, "SELECT COUNT(*) AS n FROM journal")
		mustDo(d.t, err)
		return map[string]any{"relationships": relationships, "generations": generations, "journal": journal.I("n")}, nil
	case "resolution":
		found, _ := ReadAssignment(d.adir(op))
		var entries []Obj
		pool := append(append(markerFactList(found, "attempts"), markerFactList(found, "claims")...), markerFactList(found, "conflicts")...)
		ids, _ := op["facts"].([]any)
		for _, id := range ids {
			for _, raw := range pool {
				fact := raw.(Obj)
				if fieldOf(fact, "factId") == id {
					digest := FactDigest(fact)
					if op["digest"] == "zero" {
						digest = strings.Repeat("0", 64)
					}
					entries = append(entries, Obj{{Key: "factId", Value: id}, {Key: "digest", Value: digest}})
				}
			}
		}
		return PublishResolution(d.root(), d.work(), d.assignment(op), op["task"], op["session"], opOr(op, "reason", "r"), "2026-01-01T00:05:00+00:00", entries)
	case "publish":
		root := ""
		if op["confined"] == true {
			root = d.root()
		}
		return Publish(d.target(op), fromJSON(op["payload"]), root)
	case "write_raw":
		target := d.target(op)
		mustDo(d.t, os.MkdirAll(filepath.Dir(target), 0o700))
		mustDo(d.t, os.WriteFile(target, []byte(op["text"].(string)), 0o600))
		return nil, nil
	case "unlink":
		mustDo(d.t, os.Remove(filepath.Join(d.adir(op), op["path"].(string))))
		return nil, nil
	case "exists":
		_, err := os.Stat(filepath.Join(d.tree, op["path"].(string)))
		return err == nil, nil
	case "exists_in":
		_, err := os.Stat(filepath.Join(d.adir(op), op["path"].(string)))
		return err == nil, nil
	case "listdir":
		entries, err := os.ReadDir(d.path(op["target"]))
		mustDo(d.t, err)
		names := []any{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names, nil
	case "read_file":
		data, err := os.ReadFile(d.path(op["target"]))
		mustDo(d.t, err)
		return loads(string(data))
	case "facts":
		found, unreadable := ReadAssignment(d.adir(op))
		return map[string]any{"facts": found, "unreadable": unreadable}, nil
	case "state":
		found, _ := ReadAssignment(d.adir(op))
		return DeriveAssignmentState(found, opOr(op, "now", "2026-01-01T00:05:00+00:00")), nil
	case "malformed":
		found, _ := ReadAssignment(d.adir(op))
		return noneIfEmpty(Malformed(found)), nil
	case "counters":
		return noneIfEmpty(MalformedCounters(fromJSON(op["value"]))), nil
	case "contested":
		found, _ := ReadAssignment(d.adir(op))
		return IdentityContested(found), nil
	case "covered":
		found, _ := ReadAssignment(d.adir(op))
		for _, raw := range markerFactList(found, "claims") {
			if fieldOf(raw.(Obj), "factId") == op["fact"] {
				return FactCovered(raw.(Obj), resolutionsOf(found)), nil
			}
		}
		d.t.Fatalf("no fact %v", op["fact"])
	case "claimant":
		found, _ := ReadAssignment(d.adir(op))
		for _, raw := range markerFactList(found, "claims") {
			if fieldOf(raw.(Obj), "factId") == op["fact"] {
				return Claimant(raw.(Obj)), nil
			}
		}
		d.t.Fatalf("no fact %v", op["fact"])
	case "correlated":
		found, _ := ReadAssignment(d.adir(op))
		return Correlated(found, op["session"], nil), nil
	case "select":
		workspace := d.work()
		if w, ok := op["workspace"]; ok {
			workspace = d.path(w)
		}
		directory, found, unreadable, err := SelectAssignment(d.root(), workspace, op["session"])
		if err != nil {
			return nil, err
		}
		var name, facts any
		if directory != "" {
			name, facts = filepath.Base(directory), found
		}
		return map[string]any{"assignment": name, "facts": facts, "unreadable": unreadable}, nil
	case "disposition":
		return PublishDisposition(d.root(), d.work(), d.assignment(op), op["session"], op["turn"], op["outcome"].(string), t0)
	case "read_disposition":
		found, readable := ReadDisposition(d.adir(op), op["session"], op["turn"])
		return map[string]any{"found": found, "readable": readable}, nil
	case "digest":
		return FactDigest(fromJSON(op["payload"]).(Obj)), nil
	case "named":
		var out []any
		for _, v := range op["values"].([]any) {
			out = append(out, Named(fromJSON(v)))
		}
		return out, nil
	case "same":
		var out []any
		for _, pair := range op["pairs"].([]any) {
			p := pair.([]any)
			out = append(out, SameIdentity(fromJSON(p[0]), fromJSON(p[1])))
		}
		return out, nil
	case "workspace_key":
		return WorkspaceKey(d.path(op["workspace"]))
	case "symlink":
		mustDo(d.t, os.Symlink(d.path(op["to"]), d.path(op["link"])))
		return nil, nil
	case "marker_root":
		chosen, err := ResolveMarkerRoot(d.path(op["explicit"]))
		if err != nil {
			return nil, err
		}
		return chosen.Record(), nil
	case "set_env":
		setEnv(d.t, op["name"].(string), op["value"], d.tree)
		return nil, nil
	}
	d.t.Fatalf("unknown op %v", op["op"])
	return nil, nil
}

func (d *markerDriver) target(op markerOp) string {
	if p, ok := op["path"].(string); ok {
		return filepath.Join(d.adir(op), p)
	}
	return d.path(op["target"])
}

func opOr(op markerOp, key string, fallback any) any {
	if v, ok := op[key]; ok {
		return v
	}
	return fallback
}

func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// requireSameOps compares the two answer lists entry by entry, and returns Python's.
func requireSameOps(t *testing.T, ops []markerOp, python, golang []any) []any {
	t.Helper()
	if len(python) != len(golang) {
		t.Fatalf("python answered %d ops, go %d", len(python), len(golang))
	}
	for i := range ops {
		p, g := normalizeJSON(t, python[i]), normalizeJSON(t, jsonable(golang[i]))
		p, g = withoutPID(p), withoutPID(g)
		if !reflect.DeepEqual(p, g) {
			pb, _ := json.Marshal(p)
			gb, _ := json.Marshal(g)
			t.Errorf("op %d %v differs from Python\ngo:     %s\npython: %s", i, ops[i], gb, pb)
		}
	}
	return python
}

// withoutPID replaces a conflict's loserProcess (the writer's own pid, str(os.getpid())) with one
// token: the two sides are different processes, and nothing else in the record may differ.
func withoutPID(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if k == "loserProcess" {
				if _, isText := e.(string); isText {
					e = "<pid>"
				}
			}
			out[k] = withoutPID(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = withoutPID(e)
		}
		return out
	}
	return v
}

// ok is the value an op returned on the Python side (Python is the reference).
func ok(t *testing.T, answer any) any {
	t.Helper()
	m, isMap := answer.(map[string]any)
	value, present := m["ok"]
	if !isMap || !present {
		t.Fatalf("op did not return: %v", answer)
	}
	return value
}

func reasonOf(t *testing.T, answer any) string {
	t.Helper()
	m, _ := answer.(map[string]any)
	reason, _ := m["reason"].(string)
	if reason == "" {
		t.Fatalf("op was not refused: %v", answer)
	}
	return reason
}

// sameOps runs ops on both sides, requires every answer to be equal, and returns Python's.
func sameOps(t *testing.T, env map[string]any, ops ...markerOp) []any {
	t.Helper()
	python, golang := runMarkerOps(t, env, ops)
	return requireSameOps(t, ops, python, golang)
}
