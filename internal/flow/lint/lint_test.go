package lint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const fixtures = "../../../testdata/flow/lint"

// lintFile checks one fixture, static rules and dry run together.
func lintFile(t *testing.T, name string) Report {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return Check(context.Background(), script.Source{Name: name, Text: text}, Options{DryRun: true, Args: json.RawMessage(`{"node":"n1","plan":"p","nodes":[]}`)})
}

func lintText(t *testing.T, body string) Report {
	t.Helper()
	return Check(context.Background(), script.Source{Name: "inline.js", Text: []byte("export const meta = { name: 'inline' }\n" + body)}, Options{DryRun: true})
}

// rules are the distinct rules of a report, sorted.
func rules(r Report) []string {
	set := map[string]bool{}
	for _, d := range r.Diagnostics {
		set[d.Rule] = true
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantRules(t *testing.T, r Report, want ...string) {
	t.Helper()
	if got := rules(r); !slices.Equal(got, want) {
		t.Errorf("%s: rules %v, want %v\n%v", r.Script, got, want, r.Diagnostics)
	}
}

func clean(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		r := lintFile(t, name)
		if !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

func TestLintLifecycleAutomaton(t *testing.T) {
	clean(t, "ok-lifecycle.js")
	for name, rule := range map[string]string{
		"land-twice.js":                 RuleLandTwice,
		"land-without-review.js":        RuleLandWithoutReview,
		"order-review-before-verify.js": RuleLifecycleOrder,
	} {
		r := lintFile(t, name)
		wantRules(t, r, rule)
		for _, d := range r.Diagnostics {
			if d.Line == 0 || d.Source != sourceStatic {
				t.Errorf("%s: the static finding names its line: %+v", name, d)
			}
		}
	}
	// The three fixtures differ in where the offending call is.
	if r := lintFile(t, "land-twice.js"); r.Diagnostics[0].Line != 8 {
		t.Errorf("the second land is on line 8: %+v", r.Diagnostics[0])
	}
	// A violation is reported only when no path makes the event legal.
	for name, body := range map[string]string{
		"a land on one of two paths": `
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
const head = await crw.commit('n', ex)
const ci = await crw.verify('n', head)
const rv = ci.pass ? await crw.review('n', head, ci) : null
return rv && rv.verdict === 'verified' ? await crw.land('n', head) : null`,
		"a rebuild after a failed round": `
let ex = await agent.call('build', { agentType: 'executor', label: 'build' })
await crw.commit('n', ex)
ex = await agent.call('build again', { agentType: 'executor', label: 'rebuild' })
await crw.commit('n', ex)
return 1`,
		"lands in different branches": `
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
await crw.review('n', head, null)
if (args.a) return crw.land('n', { head })
return crw.land('n', { head, other: true })`,
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// Other illegal orders.
	for name, row := range map[string]struct{ body, rule string }{
		"a commit before the build": {`
await crw.commit('n', null)
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
return ex`, RuleLifecycleOrder},
		"a build after the land": {`
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
await crw.review('n', head, null)
await crw.land('n', head)
return agent.call('build again', { agentType: 'executor', label: 'again' })`, RuleLifecycleOrder},
		"a land in a loop that already landed": {`
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
await crw.review('n', head, null)
await crw.land('n', head)
for (let i = 0; i < 2; i++) { await crw.land('n', head) }
return 1`, RuleLandTwice},
	} {
		r := lintText(t, row.body)
		if !slices.Contains(rules(r), row.rule) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

// The five violations of the design each fail for their own reason.
func TestLintLifecycleViolationFixtures(t *testing.T) {
	for name, rule := range map[string]string{
		"land-twice.js":               RuleLandTwice,
		"land-without-review.js":      RuleLandWithoutReview,
		"same-family-review.js":       RuleReviewerFamily,
		"label-duplicate.js":          RuleLabelDuplicate,
		"t1-without-step-decision.js": RuleTierStepDecision,
	} {
		wantRules(t, lintFile(t, name), rule)
	}
}

func TestLintTierStepRequiresDecision(t *testing.T) {
	clean(t, "t1-with-step-decision.js")
	r := lintFile(t, "t1-without-step-decision.js")
	wantRules(t, r, RuleTierStepDecision)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Label != "retry" {
		t.Errorf("the static finding and the dry run's refusal are one finding: %+v", r.Diagnostics)
	}
	// A decision made on another path is not this path's.
	r = lintFile(t, "t1-step-from-other-path.js")
	wantRules(t, r, RuleTierStepDecision)
	if !strings.Contains(r.Diagnostics[0].Message, "same path") {
		t.Errorf("%+v", r.Diagnostics)
	}
	// A call named by agentType, a continuation, and a decision the call reads through a name all count.
	for _, body := range []string{
		`const step = await crw.tierStep('n', {}); return agent.call('go', { agentType: 'executor-t1', stepDecision: step && step.id, label: 't1' })`,
		`const first = await crw.tierStep('n', {}); const t1 = await agent.call('go', { agentType: 'executor-t1', stepDecision: first.id, label: 't1' }); return agent.call('more', { resume: t1.handle, label: 'more' })`,
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", body, r.Diagnostics)
		}
	}
	if r := lintText(t, `return agent.call('go', { agentType: 'executor-t1', label: 't1' })`); !slices.Contains(rules(r), RuleTierStepDecision) {
		t.Errorf("an executor-t1 agentType without a decision: %v", r.Diagnostics)
	}
	// The same script, not statically decidable, is still caught by the dry run.
	r = lintText(t, "const tier = args && args.tier ? 'executor-t1' : 'executor-t1'\nconst pick = [tier][0]\nreturn agent.call('go', { agentType: pick, label: 'dynamic' })")
	wantRules(t, r, RuleTierStepDecision)
	if r.Diagnostics[0].Source != sourceTrace {
		t.Errorf("%+v", r.Diagnostics)
	}
}

func TestLintReviewerFamily(t *testing.T) {
	r := lintFile(t, "same-family-review.js")
	wantRules(t, r, RuleReviewerFamily)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Line != 4 {
		t.Errorf("%+v", r.Diagnostics)
	}
	for name, body := range map[string]string{
		"a reviewer after an executor": `
await agent.call('build', { agentType: 'executor', label: 'build' })
return agent.call('review the change', { agentType: 'reviewer', label: 'review:build' })`,
		"a reviewer by alias": `
await agent.call('build', { model: 'haiku', isolation: 'worktree', label: 'build' })
return agent.call('review the change', { agentType: 'ocx-gpt-6-1-sol', effort: 'high', label: 'review:build' })`,
		"a second tier author and no review": `
return agent.call('build', { agentType: 'executor', label: 'build' })`,
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// The review call is found by its packet as well as by its label, and the author may be the second tier.
	r = lintText(t, `
const step = await crw.tierStep('n', {})
await agent.call('build', { agentType: 'executor-t1', stepDecision: step.id, label: 'build' })
return agent.call(crw.reviewPacket('n'), { agentType: 'executor', label: 'check' })`)
	if !slices.Contains(rules(r), RuleReviewerFamily) {
		t.Errorf("%v", r.Diagnostics)
	}
	// A review whose role the text cannot tell is judged on the trace.
	r = lintText(t, `
await agent.call('build', { agentType: 'executor', label: 'build' })
const who = ['executor'][0]
return agent.call('review the change', { agentType: who, label: 'review:dynamic' })`)
	wantRules(t, r, RuleReviewerFamily)
	if r.Diagnostics[0].Source != sourceTrace {
		t.Errorf("%+v", r.Diagnostics)
	}
}

func TestLintLabelUniquePerPath(t *testing.T) {
	r := lintFile(t, "label-duplicate.js")
	wantRules(t, r, RuleLabelDuplicate)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Line != 3 || r.Diagnostics[0].Label != "scan" {
		t.Errorf("the static and the trace finding are one: %+v", r.Diagnostics)
	}
	r = lintFile(t, "label-in-loop.js")
	wantRules(t, r, RuleLabelDuplicate)
	clean(t, "labels-ok.js")
	// A loop that can leave early may run its call once: the dry run decides whether it repeats.
	if r := lintText(t, "for (let round = 0; round < 2; round++) {\n  await agent(`find ${round}`, { agentType: 'explorer', label: 'find' })\n  if (!args) break\n}\nreturn 1"); !r.OK() {
		t.Errorf("%v", r.Diagnostics)
	}
	r = lintText(t, "for (let round = 0; round < 2; round++) {\n  await agent(`find ${round}`, { agentType: 'explorer', label: 'find' })\n  if (round === 1) break\n}\nreturn 1")
	if wantRules(t, r, RuleLabelDuplicate); len(r.Diagnostics) != 1 || r.Diagnostics[0].Source != sourceTrace {
		t.Errorf("the repeat is seen by the dry run: %+v", r.Diagnostics)
	}
	// The same label on two paths is fine.
	if r := lintText(t, "const out = await parallel([1, 2].map((x) => async () => agent(`look ${x}`, { agentType: 'explorer', label: 'look' })))\nreturn out.length"); !r.OK() {
		t.Errorf("%v", r.Diagnostics)
	}
	// Calls without a label share the default one and are not a finding.
	if r := lintText(t, "await agent('one', { agentType: 'explorer' })\nreturn agent('two', { agentType: 'explorer' })"); !r.OK() {
		t.Errorf("%v", r.Diagnostics)
	}
	// A label only the dry run can see.
	r = lintText(t, "const name = ['x'][0]\nawait agent('one', { agentType: 'explorer', label: name })\nreturn agent('two', { agentType: 'explorer', label: name })")
	wantRules(t, r, RuleLabelDuplicate)
}

func TestLintClassifyAndResumeTakeCallResult(t *testing.T) {
	clean(t, "classify-ok.js")
	r := lintFile(t, "classify-agent-output.js")
	wantRules(t, r, RuleClassifyArg)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Line != 2 || !strings.Contains(r.Diagnostics[0].Message, "agent() resolves with the output") {
		t.Errorf("one finding with the reason: %+v", r.Diagnostics)
	}
	r = lintFile(t, "resume-output.js")
	wantRules(t, r, RuleResumeArg)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Line != 3 || !strings.Contains(r.Diagnostics[0].Message, "an output") {
		t.Errorf("%+v", r.Diagnostics)
	}
	for name, row := range map[string]struct{ body, rule string }{
		"an output member":              {"const ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nreturn crw.classify(ex.output)", RuleClassifyArg},
		"a call id":                     {"const ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nreturn crw.classify(ex.callId)", RuleClassifyArg},
		"an object literal":             {"return crw.classify({ status: 'completed' })", RuleClassifyArg},
		"null":                          {"return crw.classify(null)", RuleClassifyArg},
		"a resume of a string":          {"return agent.call('x', { resume: 'handle', label: 'a' })", RuleResumeArg},
		"a resume of the output member": {"const ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nreturn agent.call('y', { resume: ex.output, label: 'b' })", RuleResumeArg},
		"a reassigned output":           {"let ex = await agent('x', { agentType: 'executor', label: 'a' })\nex = await agent('z', { agentType: 'executor', label: 'c' })\nreturn agent.call('y', { resume: ex, label: 'b' })", RuleResumeArg},
	} {
		if r := lintText(t, row.body); !slices.Contains(rules(r), row.rule) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// What the text cannot decide is let through; the engine refuses it at run time.
	for name, body := range map[string]string{
		"a call result": "const ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nreturn agent.call('y', { resume: ex, label: 'b' })",
		"a handle":      "const ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nreturn agent.call('y', { resume: ex.handle, label: 'b' })",
		"a parameter":   "async function more(prev) { return agent.call('y', { resume: prev, label: 'b' }) }\nreturn more(args.prev)",
		"a mixed name":  "let ex = await agent.call('x', { agentType: 'executor', label: 'a' })\nex = args.again\nreturn agent.call('y', { resume: ex, label: 'b' })",
	} {
		if r := lintText(t, body); slices.Contains(rules(r), RuleResumeArg) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

func TestLintLaneReadsNoMutableOuterLet(t *testing.T) {
	r := lintFile(t, "lane-mutable-let.js")
	wantRules(t, r, RuleLaneMutableLet)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Line != 4 || !strings.Contains(r.Diagnostics[0].Message, "`design`") || !strings.Contains(r.Diagnostics[0].Message, "line 6") {
		t.Errorf("one finding naming the binding, its read and its reassignment: %+v", r.Diagnostics)
	}
	r = lintFile(t, "on-signal-mutable-let.js")
	wantRules(t, r, RuleLaneMutableLet)
	if len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "onSignal") {
		t.Errorf("%+v", r.Diagnostics)
	}
	clean(t, "lane-reads-const.js")
	// A lane may read an outer let that is never assigned again, and may write its own.
	for name, body := range map[string]string{
		"an unchanged let": "let design = { revision: 0 }\nasync function lane(node) { return design.revision }\nreturn crw.dag(args.plan, lane, {})",
		"a write only":     "let seen = 0\nasync function lane(node) { seen = node; return node }\nreturn crw.dag(args.plan, lane, {})",
		"an inline lane":   "const design = 1\nreturn crw.dag(args.plan, async (node) => { let n = 0; n++; return design + n }, {})",
		"a nested read":    "let n = 0\nfunction bump() { n += 1 }\nasync function lane(node) { return node }\nbump()\nreturn crw.dag(args.plan, lane, {})",
	} {
		if r := lintText(t, body); slices.Contains(rules(r), RuleLaneMutableLet) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// Reads through a nested function, an update expression, a destructuring assignment and a shorthand property count.
	for name, body := range map[string]string{
		"a nested closure": "let design = 0\nasync function lane(node) { return [1].map(() => design) }\ndesign = 1\nreturn crw.dag(args.plan, lane, {})",
		"an update":        "let n = 0\nasync function lane(node) { n++; return node }\nreturn crw.dag(args.plan, lane, {})",
		"a destructuring":  "let design = 0\nasync function lane(node) { return { design } }\n;({ design } = { design: 2 })\nreturn crw.dag(args.plan, lane, {})",
		"an inline lane":   "let design = 0\ndesign = 2\nreturn crw.dag(args.plan, async (node) => design, {})",
	} {
		if r := lintText(t, body); !slices.Contains(rules(r), RuleLaneMutableLet) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

func TestLintAmbiguousHaikuAlias(t *testing.T) {
	r := lintFile(t, "ambiguous-haiku.js")
	wantRules(t, r, RuleAliasAmbiguous)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Line != 2 {
		t.Fatalf("static, once: %+v", r.Diagnostics)
	}
	for _, fix := range []string{"isolation: 'worktree'", "agentType: 'explorer'"} {
		if !strings.Contains(r.Diagnostics[0].Message, fix) {
			t.Errorf("the message does not name the fix %s: %s", fix, r.Diagnostics[0].Message)
		}
	}
	// Both fixes pass.
	for _, body := range []string{
		"return agent.call('build', { model: 'haiku', effort: 'medium', isolation: 'worktree', label: 'build' })",
		"return agent.call('look', { agentType: 'explorer', model: 'haiku', effort: 'medium', label: 'look' })",
		"return agent.call('look', { agentType: 'explorer', label: 'look' })",
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", body, r.Diagnostics)
		}
	}
	// An alias in a const is the alias.
	r = lintText(t, "const m = 'haiku'\nreturn agent.call('build', { model: m, label: 'build' })")
	wantRules(t, r, RuleAliasAmbiguous)
}

func TestLintDeterminismStubs(t *testing.T) {
	r := lintFile(t, "determinism.js")
	wantRules(t, r, RuleDeterminism)
	if len(r.Diagnostics) != 3 {
		t.Errorf("Date.now(), Math.random() and new Date(): %+v", r.Diagnostics)
	}
	// A Date built from arguments works, and a Date a script declares is its own.
	for _, body := range []string{"return new Date(0).getTime()", "return Date.UTC(2026, 9, 11)", "function Date() {}\nreturn Date.now()"} {
		if r := lintText(t, body); slices.Contains(rules(r), RuleDeterminism) && !strings.Contains(body, "function Date") {
			t.Errorf("%s: %v", body, r.Diagnostics)
		}
	}
}

// The dry run's own findings: a refused call, a second outstanding call on one path.
func TestLintDryRunTrace(t *testing.T) {
	r := lintText(t, "return agent.call('x', { agentType: 'gpt-x', label: 'unknown' })")
	wantRules(t, r, RuleCallRefused)
	r = lintText(t, "const p = agent.call('a', { agentType: 'explorer', label: 'a' })\nreturn agent.call('b', { agentType: 'explorer', label: 'b' })")
	wantRules(t, r, RulePathConcurrency)
	r = lintText(t, "throw new Error('script bug')")
	if !r.OK() || !strings.HasPrefix(r.DryRun, "ended script_error") {
		t.Errorf("a script's own error is a note, not a finding: %+v", r)
	}
	if r := Check(context.Background(), script.Source{Name: "bad.js", Text: []byte("const a = 1\n")}, Options{}); len(r.Diagnostics) != 1 || r.Diagnostics[0].Rule != RuleLoad {
		t.Errorf("%+v", r)
	}
	if r := lintText(t, "return 1"); r.DryRun != "ran" {
		t.Errorf("%q", r.DryRun)
	}
}

// The automaton over a trace, from rows a later build journals for the commit, verify, review and land operations.
func TestLintTraceLifecycle(t *testing.T) {
	row := func(op string) engine.Event {
		return engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: op, Path: "root"}
	}
	build := engine.Event{Kind: engine.KindCall, Event: engine.EvPrepared, Op: "agent", Path: "root", Label: "build", Data: map[string]any{"family": "executor", "role": "executor"}}
	for name, row := range map[string]struct {
		events []engine.Event
		want   []string
	}{
		"in order":     {[]engine.Event{build, row("crw.commit"), row("crw.verify"), row("crw.review"), row("crw.land")}, nil},
		"two lands":    {[]engine.Event{build, row("crw.commit"), row("crw.verify"), row("crw.review"), row("crw.land"), row("crw.land")}, []string{RuleLandTwice}},
		"no review":    {[]engine.Event{build, row("crw.commit"), row("crw.verify"), row("crw.land")}, []string{RuleLandWithoutReview}},
		"verify first": {[]engine.Event{build, row("crw.verify")}, []string{RuleLifecycleOrder}},
	} {
		got := rules(Report{Diagnostics: traceDiagnostics(&engine.Result{Status: engine.RunCompleted}, row.events, nil)})
		if !slices.Equal(got, row.want) {
			t.Errorf("%s: %v, want %v", name, got, row.want)
		}
	}
}

func TestLintCLI(t *testing.T) {
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunCLI(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "")
	code, out, errOut := run("lint", filepath.Join(fixtures, "classify-ok.js"), filepath.Join(fixtures, "labels-ok.js"))
	if code != 0 || errOut != "" || strings.Count(out, ": ok (dry run:") != 2 {
		t.Errorf("clean: exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = run("lint", filepath.Join(fixtures, "land-twice.js"))
	if code != 1 || !strings.Contains(out, "land-twice.js: line 8: lifecycle_land_twice:") {
		t.Errorf("finding: exit %d\n%s", code, out)
	}
	code, out, _ = run("lint", "--json", "--no-dry-run", filepath.Join(fixtures, "ambiguous-haiku.js"))
	var reports []Report
	if err := json.Unmarshal([]byte(out), &reports); code != 1 || err != nil || len(reports) != 1 || reports[0].DryRun != "skipped" || reports[0].Diagnostics[0].Rule != RuleAliasAmbiguous {
		t.Errorf("json: exit %d %v\n%s", code, err, out)
	}
	if code, _, errOut = run("lint"); code != 2 || !strings.Contains(errOut, "at least one script") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if code, _, errOut = run("lint", filepath.Join(fixtures, "missing.js")); code != 2 || !strings.Contains(errOut, "no such file") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if code, _, errOut = run("lint", "--args", "{", filepath.Join(fixtures, "classify-ok.js")); code != 2 || !strings.Contains(errOut, "not valid JSON") {
		t.Errorf("exit %d %q", code, errOut)
	}
	// The other flow commands still answer.
	if code, out, _ = run("roles"); code != 0 || !strings.Contains(out, "roles:") {
		t.Errorf("roles: exit %d\n%s", code, out)
	}
}
