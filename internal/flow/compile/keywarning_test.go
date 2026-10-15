package compile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func ruleCounts(ws []Warning) map[string]int {
	out := map[string]int{}
	for _, w := range ws {
		out[w.Rule]++
	}
	return out
}

// TestHandoffWarningsAreCarriedIntoThePlanWarnings (CRW-1379 c1): a warning the handoff carries (what `crw flow plan-input` found when
// it wrote the handoff) is a warning of its project's plan, with the issue it is about as its path, in the dry run and in the real
// compile; it refuses nothing and is no part of the revision (the same request id and changes with and without it).
func TestHandoffWarningsAreCarriedIntoThePlanWarnings(t *testing.T) {
	raw := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"o:r:3","warnings":[` +
		`{"rule":"project_key_bound","detail":"the plan's project key is o:r:3 (milestone o/r#3); thread t holds no live 자식 binding"},` +
		`{"rule":"criteria_missing","issue":"o/r#1","detail":"the title is the one criterion"}],` +
		`"issues":[{"key":"o:r:1","title":"a","criteria":["c"]}]}]}`
	in, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("a handoff with warnings is read: %v", err)
	}
	bad := strings.Replace(raw, `"rule":"criteria_missing"`, `"rule":"criteria_missing","level":"x"`, 1)
	if _, err := Decode([]byte(bad)); err == nil {
		t.Error("an unknown field of a carried warning was read")
	}
	for _, dry := range []bool{true, false} {
		res, err := Compile(in, Options{DryRun: dry})
		if err != nil {
			t.Fatalf("dry=%v: a carried warning refused the handoff: %v", dry, err)
		}
		want := []Warning{
			{Project: "o:r:3", Rule: "project_key_bound", Detail: "the plan's project key is o:r:3 (milestone o/r#3); thread t holds no live 자식 binding"},
			{Project: "o:r:3", Rule: "criteria_missing", Path: "o/r#1", Detail: "the title is the one criterion"},
		}
		if got := res.Plans[0].Warnings; !reflect.DeepEqual(got, want) {
			t.Errorf("dry=%v: plan warnings = %+v; want %+v", dry, got, want)
		}
		out, err := Marshal(res)
		if err != nil || !strings.Contains(string(out), `"rule": "project_key_bound"`) {
			t.Errorf("dry=%v: the output does not hold the warning: %v\n%s", dry, err, out)
		}
	}
	plain := in
	plain.Projects = []Project{in.Projects[0]}
	plain.Projects[0].Warnings = nil
	with, _ := Compile(in, Options{})
	without, _ := Compile(plain, Options{})
	if with.Plans[0].Preview.RequestID != without.Plans[0].Preview.RequestID || string(with.Plans[0].Revision) != string(without.Plans[0].Revision) {
		t.Error("a carried warning changed the revision")
	}
}

// TestCompileComparesTheProjectKeyWithTheBinding (CRW-1379 c1): with a binding, compile compares each project key with the scope keys
// of the owner's live parent bindings, the comparison `crw flow start` and `crw flow plan-input` make: a bound key is silent, another key
// is project_key_bound, no thread or unreadable bindings is project_key_unchecked, and none refuses or changes the revision. What the
// comparison does to the key warnings a handoff carries is TestCompileKeyCheckReplacesTheCarriedKeyWarning.
func TestCompileComparesTheProjectKeyWithTheBinding(t *testing.T) {
	in := project(iss("A"))
	in.Projects[0].Key = "o:r:3"
	in.Projects[0].Target = &Target{Repository: "/srv/checkout", BaseRef: "dev"} // a target of the form a run takes: no target warning
	base, err := Compile(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		b    *Binding
		want map[string]int
		in   string
	}{
		{"no binding compares nothing", nil, map[string]int{}, ""},
		{"bound", &Binding{Thread: "t", Scopes: []string{"x:y:1", "o:r:3"}}, map[string]int{}, ""},
		{"another key", &Binding{Thread: "t", Scopes: []string{"3b46bc6e-uuid"}}, map[string]int{"project_key_bound": 1}, "3b46bc6e-uuid"},
		{"no binding at all", &Binding{Thread: "t"}, map[string]int{"project_key_bound": 1}, "holds no live 자식 binding"},
		{"case only", &Binding{Thread: "t", Scopes: []string{"O:R:3"}}, map[string]int{"project_key_bound": 1}, "only by case"},
		{"no thread", &Binding{}, map[string]int{"project_key_unchecked": 1}, "--thread"},
		{"unreadable", &Binding{Thread: "t", Err: errors.New("no relay store at /x")}, map[string]int{"project_key_unchecked": 1}, "no relay store at /x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Compile(in, Options{Binding: tc.b})
			if err != nil {
				t.Fatalf("a key warning refused the handoff: %v", err)
			}
			p := res.Plans[0]
			if got := ruleCounts(p.Warnings); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("warnings = %+v", p.Warnings)
			}
			for _, w := range p.Warnings {
				if w.Project != "o:r:3" || !strings.Contains(w.Detail, tc.in) || !strings.Contains(w.Detail, "o:r:3") && w.Rule == "project_key_bound" {
					t.Errorf("warning = %+v; want the project and %q", w, tc.in)
				}
			}
			if string(p.Revision) != string(base.Plans[0].Revision) {
				t.Error("a key warning changed the revision")
			}
		})
	}
}

// TestCompileKeyCheckReplacesTheCarriedKeyWarning (CRW-1379 P2): when compile runs its own check of the project key (a binding is given,
// that is a thread is named), its result replaces any key warning the handoff carries (project_key_bound, project_key_unchecked):
// plan-input's reading was earlier and may be of another thread or state, so a carried project_key_unchecked does not outlive a
// successful check, and the two sides never both speak. With no binding (no thread named) the carried key warnings stand. A carried
// warning of any other rule is carried either way.
func TestCompileKeyCheckReplacesTheCarriedKeyWarning(t *testing.T) {
	in := project(iss("A"))
	in.Projects[0].Key = "o:r:3"
	in.Projects[0].Target = &Target{Repository: "/srv/checkout", BaseRef: "dev"} // a target of the form a run takes: no target warning
	unchecked := InputWarning{Rule: "project_key_unchecked", Detail: "from plan-input: no thread"}
	bound := InputWarning{Rule: "project_key_bound", Detail: "from plan-input: another key"}
	other := InputWarning{Rule: "criteria_missing", Issue: "o:r:1", Detail: "the title is the one criterion"}
	holds := &Binding{Thread: "t", Scopes: []string{"x:y:1", "o:r:3"}}
	another := &Binding{Thread: "t", Scopes: []string{"uuid-9"}}
	unreadable := &Binding{Thread: "t", Err: errors.New("no relay store at /x")}
	for _, tc := range []struct {
		name    string
		carried []InputWarning
		b       *Binding
		want    []string // "rule: text the detail holds", in order
	}{
		{"a stale unchecked does not outlive a successful check", []InputWarning{unchecked}, holds, nil},
		{"a bound carried warning does not either", []InputWarning{bound}, holds, nil},
		{"unchecked and another key: compile's bound only", []InputWarning{unchecked}, another, []string{"project_key_bound: uuid-9"}},
		{"bound and another key: compile's bound, not the carried one", []InputWarning{bound}, another, []string{"project_key_bound: uuid-9"}},
		{"bound and unreadable bindings: compile's unchecked only", []InputWarning{bound}, unreadable, []string{"project_key_unchecked: no relay store at /x"}},
		{"unchecked and unreadable bindings: compile's unchecked only", []InputWarning{unchecked}, unreadable, []string{"project_key_unchecked: no relay store at /x"}},
		{"a binding with no thread says unchecked itself", []InputWarning{bound}, &Binding{}, []string{"project_key_unchecked: --thread"}},
		{"no binding: the carried unchecked stands", []InputWarning{unchecked}, nil, []string{"project_key_unchecked: from plan-input: no thread"}},
		{"no binding: the carried bound stands", []InputWarning{bound}, nil, []string{"project_key_bound: from plan-input: another key"}},
		{"another rule is carried and the key check is silent", []InputWarning{other, unchecked}, holds, []string{"criteria_missing: the title"}},
		{"another rule is carried before compile's own", []InputWarning{other, bound}, another, []string{"criteria_missing: the title", "project_key_bound: uuid-9"}},
		{"another rule is carried with no binding", []InputWarning{other, unchecked}, nil, []string{"criteria_missing: the title", "project_key_unchecked: from plan-input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := in
			c.Projects = []Project{in.Projects[0]}
			c.Projects[0].Warnings = tc.carried
			for _, dry := range []bool{true, false} {
				base, err := Compile(in, Options{DryRun: dry})
				if err != nil {
					t.Fatal(err)
				}
				res, err := Compile(c, Options{DryRun: dry, Binding: tc.b})
				if err != nil {
					t.Fatalf("dry=%v: a key warning refused the handoff: %v", dry, err)
				}
				var got []string
				for _, w := range res.Plans[0].Warnings {
					got = append(got, w.Rule+": "+w.Detail)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("dry=%v: warnings = %q; want %d like %q", dry, got, len(tc.want), tc.want)
				}
				for i, w := range got {
					rule, text, _ := strings.Cut(tc.want[i], ": ")
					if !strings.HasPrefix(w, rule+": ") || !strings.Contains(w, text) {
						t.Errorf("dry=%v: warning %d = %q; want %q", dry, i, w, tc.want[i])
					}
				}
				if string(res.Plans[0].Revision) != string(base.Plans[0].Revision) {
					t.Errorf("dry=%v: a key warning changed the revision", dry)
				}
			}
		})
	}
}

// TestCLIComparesTheKeyWithTheNamedThread (CRW-1379 c1): `crw flow compile` reads the live parent bindings of --thread (default
// CODEX_THREAD_ID) from --state and --socket, read-only, and a key the thread is not bound to is a warning on stderr and in the plan's
// warnings, dry run or not. With no thread named nothing is read and nothing is said.
func TestCLIComparesTheKeyWithTheNamedThread(t *testing.T) {
	file := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(file, []byte(`{"schema":"crw-flow-compile-input/1","projects":[{"key":"o:r:3","issues":[{"key":"o:r:1","title":"a","criteria":["c"]}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var asked []string
	d := &Deps{
		Scopes: func(_ context.Context, thread, state, socket string) ([]string, error) {
			asked = []string{thread, state, socket}
			return []string{"uuid-1"}, nil
		},
		Getenv: func(k string) string {
			if k == "CODEX_THREAD_ID" {
				return "thread-env"
			}
			return ""
		},
	}
	run := func(d *Deps, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunWith(context.Background(), d, args, strings.NewReader(""), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	for _, dry := range []bool{true, false} {
		args := []string{file}
		if dry {
			args = []string{"--dry-run", file}
		}
		asked = nil
		code, out, errOut := run(d, args...)
		if code != 0 {
			t.Fatalf("dry=%v: exit %d, stderr %q", dry, code, errOut)
		}
		if !reflect.DeepEqual(asked, []string{"thread-env", "", ""}) {
			t.Errorf("dry=%v: the bindings were read with %v; the thread defaults to CODEX_THREAD_ID", dry, asked)
		}
		for _, want := range []string{"crw flow compile: warning: [project_key_bound] o:r:3:", "uuid-1", "thread-env"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("dry=%v: stderr lacks %q:\n%s", dry, want, errOut)
			}
		}
		var res Result
		if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Plans[0].Warnings) != 1 || res.Plans[0].Warnings[0].Rule != "project_key_bound" {
			t.Errorf("dry=%v: the plan does not record the warning: %v\n%s", dry, err, out)
		}
	}

	// the flags name the thread and the relay state
	asked = nil
	if code, _, errOut := run(d, "--thread", "thread-flag", "--state", "/s", "--socket", "/k.sock", file); code != 0 || !reflect.DeepEqual(asked, []string{"thread-flag", "/s", "/k.sock"}) {
		t.Errorf("exit %d (%s); bindings read with %v", code, errOut, asked)
	}

	// bound: silent
	d.Scopes = func(context.Context, string, string, string) ([]string, error) { return []string{"o:r:3"}, nil }
	if code, out, errOut := run(d, "--dry-run", file); code != 0 || errOut != "" || strings.Contains(out, `"rule": "project_key`) {
		t.Errorf("bound key: exit %d, stderr %q", code, errOut)
	}
	// unreadable: unchecked, never a refusal
	d.Scopes = func(context.Context, string, string, string) ([]string, error) {
		return nil, errors.New("no relay store at /x")
	}
	if code, _, errOut := run(d, "--dry-run", file); code != 0 || !strings.Contains(errOut, "[project_key_unchecked]") || !strings.Contains(errOut, "no relay store at /x") {
		t.Errorf("unreadable bindings: exit %d, stderr %q", code, errOut)
	}
	// no thread: nothing is read, nothing is said
	called := false
	d.Scopes = func(context.Context, string, string, string) ([]string, error) { called = true; return nil, nil }
	d.Getenv = func(string) string { return "" }
	if code, out, errOut := run(d, "--dry-run", file); code != 0 || errOut != "" || called || strings.Contains(out, `"rule": "project_key`) {
		t.Errorf("no thread: exit %d, stderr %q, bindings read %v", code, errOut, called)
	}
}
