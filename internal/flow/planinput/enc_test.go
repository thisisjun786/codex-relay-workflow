package planinput

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// TestEncodeKeyRoundTrips (c1, c3): a ref OWNER/NAME#N is written to the compile input as OWNER:NAME:N, a form the relay dag accepts as
// an identifier; DecodeKey is its inverse for every ref, including the case of the letters, '.', '_' and '-' in a name, and the
// numbers. A ':' cannot occur in a GitHub owner or repository name, so the form is unambiguous.
func TestEncodeKeyRoundTrips(t *testing.T) {
	refs := []tracking.Ref{
		{Repo: "o/r", Number: 3},
		{Repo: "thisisjun786/crw-work", Number: 1},
		{Repo: "Org-1/Some.Repo_x", Number: 2147483647},
		{Repo: "a/b", Number: 10},
		{Repo: "A9/.github", Number: 7},
		{Repo: "o/" + strings.Repeat("n", 100), Number: 12},
	}
	seen := map[string]tracking.Ref{}
	for _, r := range refs {
		key, err := EncodeKey(r)
		if err != nil {
			t.Fatalf("EncodeKey(%s): %v", r, err)
		}
		if strings.ContainsAny(key, "/#") || strings.Count(key, ":") != 2 {
			t.Errorf("EncodeKey(%s) = %q; want OWNER:NAME:N", r, key)
		}
		if want := strings.Replace(r.Repo, "/", ":", 1) + fmt.Sprintf(":%d", r.Number); key != want {
			t.Errorf("EncodeKey(%s) = %q; want %q", r, key, want)
		}
		back, err := DecodeKey(key)
		if err != nil || back != r {
			t.Errorf("DecodeKey(%q) = %v, %v; want %s", key, back, err, r)
		}
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s share the key %q", r, other, key)
		}
		seen[key] = r
	}
}

// TestDecodeKeyRefusesWhatEncodeKeyDoesNotMake: the inverse reads exactly the encoded form.
func TestDecodeKeyRefusesWhatEncodeKeyDoesNotMake(t *testing.T) {
	for _, s := range []string{"", "o/r#3", "o:r", "o:r:", "o:r:x", "o:r:0", "o:r:-1", "a:b:c:3", ":r:3", "o::3", "o:r:3 ", "o:.:3", "o:..:3"} {
		if r, err := DecodeKey(s); err == nil {
			t.Errorf("DecodeKey(%q) = %s; want an error", s, r)
		}
	}
}

// TestEncodeKeyEdgeLengths (c1): the relay dag allows at most dag.MaxIDLength (128) characters in an identifier. A key at the limit is
// encoded; one character more is an input error that names the ref and the limit, and is never cut short. The first character must be a
// letter or a digit.
func TestEncodeKeyEdgeLengths(t *testing.T) {
	name := func(n int) string { return strings.Repeat("n", n) }
	// "o:" + name + ":1" is len(name)+4 characters
	atLimit := tracking.Ref{Repo: "o/" + name(dag.MaxIDLength-4), Number: 1}
	key, err := EncodeKey(atLimit)
	if err != nil || len(key) != dag.MaxIDLength {
		t.Fatalf("EncodeKey at the limit = %q (%d), %v", key, len(key), err)
	}
	over := tracking.Ref{Repo: "o/" + name(dag.MaxIDLength-3), Number: 1}
	key, err = EncodeKey(over)
	if err == nil || key != "" {
		t.Fatalf("EncodeKey over the limit = %q, %v; want an error and no key", key, err)
	}
	for _, want := range []string{over.String(), "129", "128"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	for _, r := range []tracking.Ref{{Repo: "_o/r", Number: 1}, {Repo: ".o/r", Number: 1}, {Repo: "-o/r", Number: 1}} {
		if key, err := EncodeKey(r); err == nil || !strings.Contains(err.Error(), r.String()) {
			t.Errorf("EncodeKey(%s) = %q, %v; want an error naming the ref (the first character of an identifier is a letter or a digit)", r, key, err)
		}
	}
}

// replanEdgeID re-plans the order from a to b the way a plan is revised after its edge was retired: compile names the edge again, so
// the id e:FROM:TO is used and the new edge is e:FROM:TO.2. It returns the error compile refuses the last revision with, nil when it
// compiles, and the id the edge got.
func replanEdgeID(t *testing.T, key, from, to string) (edge string, err error) {
	t.Helper()
	line := func(k string, after ...string) compile.Issue {
		return compile.Issue{Key: k, Title: k, Criteria: []string{k}, After: after}
	}
	input := func(withOrder bool) compile.Input {
		later := line(to)
		if withOrder {
			later = line(to, from)
		}
		return compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{{
			Key: key, Target: devTarget, Issues: []compile.Issue{line(from), later},
		}}}
	}
	state := dag.State{PlanID: key}
	for i, withOrder := range []bool{true, false, true} {
		res, cerr := compile.Compile(input(withOrder), compile.Options{Bases: map[string]dag.State{key: state}, DryRun: true})
		if cerr != nil {
			if i < 2 {
				t.Fatalf("revision %d of the plan: %v", i+1, cerr)
			}
			return "", cerr
		}
		plan := res.Plans[0]
		next, _, vs := dag.Apply(state, *plan.Change)
		if len(vs) > 0 {
			t.Fatalf("revision %d does not apply: %v", i+1, vs)
		}
		state = next
	}
	for _, e := range state.Edges {
		if e.RetiredRev == 0 {
			edge = e.EdgeID
		}
	}
	return edge, nil
}

// TestEdgeIDLengthEdges (CRW-1379 P3): compile names an order edge "e:FROM:TO", and "e:FROM:TO.N" (N from 2) when the plan is revised
// after the edge was retired, and the dag holds both to 128 characters. An order whose edge id leaves no room for the ".2" a re-plan adds
// is an input error that names both issues, the length and the suffix, though the first plan would compile; one with room compiles
// again after a re-plan.
func TestEdgeIDLengthEdges(t *testing.T) {
	longRepo := func(n int) string { return "o/" + strings.Repeat("n", n) }
	// from "o:NAME:1" and to "o:NAME:N": the edge id is 2+len(from)+1+len(to), computed, not assumed
	for _, tc := range []struct {
		name    string
		repoLen int
		to      int
		ok      bool
	}{
		{"edge of 125", 57, 2, true},
		{"edge of 126 and 128 with the re-plan suffix", 57, 10, true},
		{"edge of 127 and 129 with the re-plan suffix", 58, 2, false},
		{"edge of 128 and 130 with the re-plan suffix", 58, 10, false},
		{"edge of 129", 59, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := longRepo(tc.repoLen)
			a, b := tracking.Ref{Repo: repo, Number: 1}, tracking.Ref{Repo: repo, Number: tc.to}
			ka, _ := EncodeKey(a)
			kb, _ := EncodeKey(b)
			edge := "e:" + ka + ":" + kb
			ms := tracking.Ref{Repo: repo, Number: 3}
			kms, _ := EncodeKey(ms)
			bodyB := "## Done when\n- b\nBlocked by: #1\n"
			r := &fakeReader{
				ms: tracking.Milestone{Ref: ms, Title: "m", State: tracking.StateOpen},
				issues: []tracking.Issue{
					{Ref: a, Title: "a", Body: "## Done when\n- a\n", State: tracking.StateOpen, Milestone: 3},
					{Ref: b, Title: "b", Body: bodyB, State: tracking.StateOpen, Milestone: 3, BlockedBy: tracking.ParseBlockedBy(repo, bodyB)},
				},
			}
			res, err := Build(bg, r, ms, Options{Target: devTarget})
			if tc.ok {
				if err != nil {
					t.Fatalf("an edge of %d characters: %v", len(edge), err)
				}
				if len(edge)+len(".2") > dag.MaxIDLength {
					t.Fatalf("the test edge is %d characters", len(edge))
				}
				if _, err := compile.Compile(res.Input, compile.Options{DryRun: true}); err != nil {
					t.Errorf("compile of an edge of %d characters: %v", len(edge), err)
				}
				got, err := replanEdgeID(t, kms, ka, kb)
				if err != nil || got != edge+".2" {
					t.Errorf("the re-plan of the order gave the edge %q, %v; want %q", got, err, edge+".2")
				}
				return
			}
			if len(edge)+len(".2") <= dag.MaxIDLength {
				t.Fatalf("the test edge is %d characters", len(edge))
			}
			if err == nil {
				t.Fatalf("an edge of %d characters was built: %+v", len(edge), res.Input)
			}
			wants := []string{a.String(), b.String(), fmt.Sprint(len(edge)), "128"}
			if len(edge) <= dag.MaxIDLength {
				// the first plan would compile, the re-plan would not: the error says why
				wants = append(wants, ".2", fmt.Sprint(len(edge)+len(".2")), "re-plan")
				if _, cerr := compile.Compile(compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{{
					Key: kms, Target: devTarget, Issues: []compile.Issue{
						{Key: ka, Title: "a", Criteria: []string{"a"}},
						{Key: kb, Title: "b", Criteria: []string{"b"}, After: []string{ka}},
					}}}}, compile.Options{DryRun: true}); cerr != nil {
					t.Fatalf("the first plan of an edge of %d characters compiles: %v", len(edge), cerr)
				}
				if _, rerr := replanEdgeID(t, kms, ka, kb); rerr == nil {
					t.Errorf("the re-plan of an edge of %d characters compiled: the suffix check is not needed", len(edge))
				}
			}
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if len(res.Input.Projects) != 0 {
				t.Errorf("an input error returns no input: %+v", res.Input)
			}
		})
	}
}

// TestBuildRefusesAKeyTheDagCannotHold (c1): a project key or an issue key over the limit is an input error, whole: nothing is
// built from part of the milestone.
func TestBuildRefusesAKeyTheDagCannotHold(t *testing.T) {
	repo := "o/" + strings.Repeat("n", dag.MaxIDLength-3) // "o:NAME:3" is 129 characters
	ms := tracking.Ref{Repo: repo, Number: 3}
	r := &fakeReader{
		ms:     tracking.Milestone{Ref: ms, Title: "m", State: tracking.StateOpen},
		issues: []tracking.Issue{{Ref: tracking.Ref{Repo: repo, Number: 1}, Title: "a", Body: "## Done when\n- a\n", State: tracking.StateOpen, Milestone: 3}},
	}
	res, err := Build(bg, r, ms, Options{})
	if err == nil {
		t.Fatalf("built %+v", res.Input)
	}
	if !strings.Contains(err.Error(), ms.String()) || !strings.Contains(err.Error(), "128") {
		t.Errorf("the error names the milestone and the limit: %v", err)
	}
}

// TestBuildCarriesTheEncodedKeysAndRemembersTheRefs (c1, c3): the input carries OWNER:NAME:N for the project key, the issue keys and
// after; Result.Refs maps each encoded key back to the OWNER/NAME#N ref; the warnings name the refs; a binding on the ref itself is
// not the plan's key (the scope_key of the parent binding is the encoded form, the one `crw flow start` compares).
func TestBuildCarriesTheEncodedKeysAndRemembersTheRefs(t *testing.T) {
	r := milestone(
		open(1, "## Done when\n- a\n"),
		open(2, "## Done when\n- b\nBlocked by: #1, #9\n"),
	)
	res := build(t, r, Options{Target: devTarget, Binding: &Binding{Thread: "t", Scopes: []string{"o/r#3"}}})
	p := res.Input.Projects[0]
	if p.Key != "o:r:3" || p.Issues[0].Key != "o:r:1" || p.Issues[1].Key != "o:r:2" || !reflect.DeepEqual(p.Issues[1].After, []string{"o:r:1"}) {
		t.Errorf("project = %+v", p)
	}
	want := map[string]string{"o:r:3": "o/r#3", "o:r:1": "o/r#1", "o:r:2": "o/r#2"}
	if !reflect.DeepEqual(res.Refs, want) {
		t.Errorf("Refs = %v; want %v", res.Refs, want)
	}
	byRule := map[string]Warning{}
	for _, w := range res.Warnings {
		byRule[w.Rule] = w
	}
	if w := byRule["blocker_outside_milestone"]; w.Issue != "o/r#2" || !strings.Contains(w.Detail, "o/r#9") || strings.Contains(w.Detail, "o:r:") {
		t.Errorf("the blocker warning shows the refs: %+v", w)
	}
	if w, ok := byRule["project_key_bound"]; !ok || !strings.Contains(w.Detail, "o:r:3") || !strings.Contains(w.Detail, "o/r#3") {
		t.Errorf("a binding on the ref itself is not the plan key o:r:3: %+v", res.Warnings)
	}
	// and the input compiles with no identifier refusal
	plan, err := compile.Compile(res.Input, compile.Options{DryRun: true})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if plan.Plans[0].ProjectKey != "o:r:3" {
		t.Errorf("plan = %+v", plan.Plans[0])
	}
}
