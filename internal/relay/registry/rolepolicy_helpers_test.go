package registry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// roleStep is one recorded step of testdata/gen_rolepolicy.py: the step (a small language both
// sides execute) and Python's whole answer, with the run's directory, work root and policy
// digest written as ${DIR}, ${ROOT} and ${DIGEST}.
type roleStep struct {
	Step   []json.RawMessage `json:"step"`
	Result json.RawMessage   `json:"result"`
}

var pythonRolePolicy = sync.OnceValues(func() (map[string][]roleStep, error) {
	raw, err := os.ReadFile("testdata/python_rolepolicy.json")
	if err != nil {
		return nil, err
	}
	var out map[string][]roleStep
	return out, json.Unmarshal(raw, &out)
})

type settingsSpec struct {
	Cwd       string          `json:"cwd"`
	Overrides json.RawMessage `json:"overrides"`
}

// roleRun executes a scenario's steps against a Go registry and returns one answer per step.
type roleRun struct {
	t      *testing.T
	r      *Registry
	dir    string
	root   string
	digest string
	rid    string
}

func newRoleRun(t *testing.T) *roleRun {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx(), filepath.Join(dir, "state", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &roleRun{t: t, r: &Registry{Store: s, Now: func() string { return fakeISO }, Policy: ResolveRolePolicy(map[string]string{})}, dir: dir, root: root}
}

func (x *roleRun) build(raw json.RawMessage) contract.OrderedObject {
	var spec settingsSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		x.t.Fatal(err)
	}
	cwd := strings.NewReplacer("${DIR}", x.dir, "${ROOT}", x.root).Replace(spec.Cwd)
	settings := settingsFixture(cwd)
	overrides, err := decodeJSON(spec.Overrides)
	if err != nil {
		x.t.Fatal(err)
	}
	for _, field := range overrides.(contract.OrderedObject) {
		settings = setField(settings, field.Key, field.Value)
	}
	return settings
}

func arg[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if len(raw) == 0 {
		return v
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func answerOf(value any, err error) any {
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			return map[string]any{"refused": map[string]any{"reason": refused.Reason, "detail": refused.Detail}}
		}
		var host *HostError
		if errors.As(err, &host) {
			return map[string]any{"host": host.Error()}
		}
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"ok": value}
}

func (x *roleRun) policy(text string) {
	path := filepath.Join(x.dir, "execution-policy.json")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(text, "${DIR}", x.dir)), 0o644); err != nil {
		x.t.Fatal(err)
	}
	x.r.Policy = ResolveRolePolicy(map[string]string{execution.EnvPolicy: path})
	x.digest = x.r.Policy.Digest()
}

func (x *roleRun) stored(task string) any {
	var raw string
	err := x.r.Store.DB.QueryRowContext(ctx(), "SELECT settings FROM authorized_settings WHERE task_id = ?", task).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		x.t.Fatal(err)
	}
	decoded, err := decodeJSON([]byte(raw))
	if err != nil {
		x.t.Fatal(err)
	}
	return decoded
}

func (x *roleRun) boundRole(task string) any {
	role, contested, err := boundRole(ctx(), x.r.Store, task)
	if err != nil {
		x.t.Fatal(err)
	}
	if contested != nil {
		return map[string]any{"contested": anyStrings(contested)}
	}
	if role == "" {
		return nil
	}
	return role
}

func (x *roleRun) step(raw []json.RawMessage, python json.RawMessage) any {
	t := x.t
	op := arg[string](t, raw[0])
	args := raw[1:]
	parentEndpoint := func(task string) Endpoint { return Endpoint{task, "host-a", ns("/parent"), ns("cxc-parent")} }
	switch op {
	case "roles":
		roles := make([]string, 0, len(roleScope))
		for _, name := range []string{execution.Child, execution.Parent, execution.Supervisor} {
			if _, ok := roleScope[name]; ok {
				roles = append(roles, name)
			}
		}
		return anyStrings(roles)
	case "policy":
		text := string(args[0])
		var py struct {
			Text        string   `json:"text"`
			DigestParts []string `json:"digestParts"`
		}
		if err := json.Unmarshal(python, &py); err != nil {
			t.Fatal(err)
		}
		x.policy(py.Text)
		// The digest is SHA-256 of the file's bytes; where they do not name the run's directory
		// it must be Python's own.
		if !strings.Contains(py.Text, "${DIR}") && strings.Join(py.DigestParts, "") != x.digest {
			t.Fatalf("policy digest %q, python %q", x.digest, strings.Join(py.DigestParts, ""))
		}
		_ = text
		var digest any
		var parts any
		if x.digest != "" {
			digest, parts = x.digest, anyStrings([]string{x.digest[:8], x.digest[8:]})
		}
		if !strings.Contains(py.Text, "${DIR}") {
			return map[string]any{"text": py.Text, "digest": digest, "digestParts": parts}
		}
		return map[string]any{"text": py.Text, "digest": digest, "digestParts": anyStrings(py.DigestParts)}
	case "unset":
		x.r.Policy = ResolveRolePolicy(map[string]string{})
		x.digest = ""
		return nil
	case "resolve":
		env := map[string]string{}
		if arg[map[string]bool](t, args[0])["file"] {
			env[execution.EnvPolicy] = filepath.Join(x.dir, "execution-policy.json")
		}
		p := ResolveRolePolicy(env)
		var detail, expectations any
		if !p.Declared {
			detail = p.Detail
		} else {
			e := map[string]any{}
			for _, role := range []string{"supervisor", "parent", "child"} {
				if exp, ok := p.expectation(role); ok {
					e[role] = map[string]any{"expectation": exp.Expectation, "model": nullText(exp.Model), "reasoningEffort": nullText(exp.Effort)}
				} else {
					e[role] = nil
				}
			}
			expectations = e
		}
		return map[string]any{"declared": p.Declared, "detail": detail, "summary": p.Summary(), "expectations": expectations}
	case "bind":
		return answerOf(x.r.BindScope(ctx(), arg[string](t, args[0]), arg[string](t, args[1]), parentEndpoint(arg[string](t, args[2]))))
	case "record":
		task, source := arg[string](t, args[0]), arg[string](t, args[2])
		role, _ := arg[any](t, args[3]).(string)
		citation := Citation{}
		switch exception := arg[any](t, args[4]).(type) {
		case string:
			if exception == "__CLEAR__" {
				citation.Clear = true
			} else {
				citation = Citation{ID: exception, Set: true}
			}
		}
		return answerOf(x.r.RecordSettings(ctx(), task, x.build(args[1]), source, role, citation))
	case "raw_settings":
		if _, err := x.r.Store.DB.ExecContext(ctx(), "INSERT OR REPLACE INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
			arg[string](t, args[0]), pyDumps(x.build(args[1]), false), "test-raw", fakeISO); err != nil {
			t.Fatal(err)
		}
		return nil
	case "sql":
		if _, err := x.r.Store.DB.ExecContext(ctx(), arg[string](t, args[0])); err != nil {
			t.Fatal(err)
		}
		return nil
	case "gate":
		settings, free, err := x.r.AuthorizedSettings(ctx(), arg[string](t, args[0]))
		if err != nil {
			return answerOf(nil, err)
		}
		return answerOf(contract.OrderedObject{{Key: "settings", Value: settings.Data}, {Key: "settingsFree", Value: free}}, nil)
	case "show":
		return answerOf(x.r.SettingsShow(ctx(), arg[string](t, args[0])))
	case "stored":
		return x.stored(arg[string](t, args[0]))
	case "bound_role":
		return x.boundRole(arg[string](t, args[0]))
	case "bindings":
		rows, err := x.r.Store.All(ctx(), "SELECT binding_id FROM scope_bindings WHERE task_id = ?", arg[string](t, args[0]))
		if err != nil {
			t.Fatal(err)
		}
		out := []any{}
		for _, row := range rows {
			out = append(out, map[string]any{"binding_id": row.Get("binding_id")})
		}
		return out
	case "check_record":
		return objectOrNil(CheckRecord(x.build(args[0]), arg[string](t, args[1]), x.r.Policy))
	case "check_binding":
		return objectOrNil(CheckBinding(arg[any](t, args[0]), arg[string](t, args[1]), x.build(args[2]), x.r.Policy))
	case "unloaded":
		err := CheckUnloadedTransmission(x.build(args[0]), arg[string](t, args[1]), x.r.Policy, "notLoaded")
		if err == nil {
			return nil
		}
		return answerOf(nil, err)
	case "unloaded_stored":
		settings, _ := x.stored(arg[string](t, args[0])).(contract.OrderedObject)
		err := CheckUnloadedTransmission(settings, arg[string](t, args[1]), x.r.Policy, "notLoaded")
		if err == nil {
			return nil
		}
		return answerOf(nil, err)
	case "loaded_mismatch":
		var py struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(python, &py); err != nil {
			t.Fatal(err)
		}
		response, err := decodeJSON(py.Response)
		if err != nil {
			t.Fatal(err)
		}
		findings := TaskSettings{settingsFixture("/parent")}.Mismatches(response, false, false, false)
		list := make([]any, len(findings))
		for i, f := range findings {
			list[i] = f
		}
		return map[string]any{"response": response, "findings": list, "code": SettingsFreeRefusalCode(findings)}
	case "register_cli":
		spec := arg[map[string]json.RawMessage](t, args[0])
		var values contract.OrderedObject
		if raw, ok := spec["child_settings_raw"]; ok {
			decoded, err := decodeJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			values = decoded.(contract.OrderedObject)
		} else {
			values = x.build(spec["child_settings"])
		}
		project, _ := arg[any](t, spec["project"]).(string)
		role, _ := arg[any](t, spec["child_role"]).(string)
		establish := ""
		if project != "" {
			establish = roleChild
		}
		in := Registration{
			Parent: parentEndpoint(parent), Child: Endpoint{child, "host-a", ns(x.root), ns("cxc-child")},
			IssueKey: "ISS-1", ArtifactRoots: []string{x.root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-1", ProjectKey: project,
		}
		return answerOf(x.r.RegisterWithSettings(ctx(), in, []settingsWrite{{task: child, values: values, role: role, establish: establish}}))
	case "count":
		rows, err := x.r.Store.All(ctx(), arg[string](t, args[0]))
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	case "register":
		in := Registration{
			Parent: Endpoint{parent, host, ns("/parent"), ns("cxc-parent")}, Child: Endpoint{child, host, ns(x.root), ns("cxc-child")},
			IssueKey: "REL-1", ArtifactRoots: []string{x.root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-1", DispatchTurnID: ns("turn-dispatch-1"), ProjectKey: arg[string](t, args[0]),
		}
		record, err := x.r.Register(ctx(), in)
		if err != nil {
			return answerOf(nil, err)
		}
		x.rid = record.ID
		return answerOf(record.ContractRecord(), nil)
	case "status":
		record, err := x.r.SetStatus(ctx(), x.rid, arg[string](t, args[0]), "test")
		if err != nil {
			return answerOf(nil, err)
		}
		return answerOf(record.ContractRecord(), nil)
	}
	t.Fatalf("unknown step %q", op)
	return nil
}

// normalise renders a Go answer as Python recorded it: plain JSON, with the run's directory,
// work root and digest written as placeholders.
func (x *roleRun) normalise(value any) any {
	var b strings.Builder
	switch v := value.(type) {
	case contract.OrderedObject, []any:
		if err := contract.Emit(&b, v); err != nil {
			x.t.Fatal(err)
		}
	default:
		raw, err := json.Marshal(orderedToPlain(x.t, v))
		if err != nil {
			x.t.Fatal(err)
		}
		b.Write(raw)
	}
	text := strings.ReplaceAll(strings.ReplaceAll(b.String(), x.root, "${ROOT}"), x.dir, "${DIR}")
	if x.digest != "" {
		text = strings.ReplaceAll(text, x.digest, "${DIGEST}")
	}
	var out any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		x.t.Fatalf("%v: %s", err, text)
	}
	return out
}

// orderedToPlain turns nested OrderedObjects inside plain maps/slices into plain values.
func orderedToPlain(t *testing.T, value any) any {
	switch v := value.(type) {
	case contract.OrderedObject:
		return plain(t, v)
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			out[k] = orderedToPlain(t, item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = orderedToPlain(t, item)
		}
		return out
	}
	return value
}

// sameRoleScenario runs a recorded scenario step by step and compares every answer whole with
// Python's. It returns Go's answers.
func sameRoleScenario(t *testing.T, name string) []any {
	t.Helper()
	all, err := pythonRolePolicy()
	if err != nil {
		t.Fatal(err)
	}
	steps, ok := all[name]
	if !ok {
		t.Fatalf("no python role-policy scenario %q", name)
	}
	x := newRoleRun(t)
	var out []any
	for i, step := range steps {
		got := x.normalise(x.step(step.Step, step.Result))
		var want any
		if err := json.Unmarshal(step.Result, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			t.Fatalf("%s step %d %s differs from Python\nGO %s\nPY %s\n", name, i, step.Step[0], g, w)
		}
		out = append(out, got)
	}
	return out
}
