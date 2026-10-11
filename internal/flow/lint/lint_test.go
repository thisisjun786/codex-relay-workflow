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
	"testing/fstest"

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

// A node has its own lifecycle: the phase of one node says nothing about another's.
func TestLintLifecyclePerNode(t *testing.T) {
	const lane = `
async function lane(node) {
  const ex = await agent.call('build', { agentType: 'executor', node, label: 'build:' + node })
  const head = await crw.commit(node, ex)
  const ci = await crw.verify(node, head)
  await crw.review(node, head, ci)
  return crw.land(node, { head })
}
`
	for name, body := range map[string]string{
		"two nodes one after the other": lane + "await lane('a')\nreturn lane('b')",
		"two nodes in one body": `
const a = await agent.call('build a', { agentType: 'executor', node: 'a', label: 'build:a' })
const ha = await crw.commit('a', a)
await crw.verify('a', ha)
await crw.review('a', ha, null)
await crw.land('a', { head: ha })
const b = await agent.call('build b', { agentType: 'executor', node: 'b', label: 'build:b' })
const hb = await crw.commit('b', b)
await crw.verify('b', hb)
await crw.review('b', hb, null)
return crw.land('b', { head: hb })`,
		"interleaved nodes": `
const a = await agent.call('build a', { agentType: 'executor', node: 'a', label: 'build:a' })
const b = await agent.call('build b', { agentType: 'executor', node: 'b', label: 'build:b' })
const ha = await crw.commit('a', a)
const hb = await crw.commit('b', b)
await crw.verify('b', hb)
await crw.verify('a', ha)
await crw.review('a', ha, null)
await crw.review('b', hb, null)
await crw.land('a', { head: ha })
return crw.land('b', { head: hb })`,
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	for name, row := range map[string]struct{ body, rule string }{
		"a land of another node than the reviewed one": {`
const a = await agent.call('build a', { agentType: 'executor', node: 'a', label: 'build:a' })
const b = await agent.call('build b', { agentType: 'executor', node: 'b', label: 'build:b' })
const ha = await crw.commit('a', a)
await crw.verify('a', ha)
await crw.review('a', ha, null)
return crw.land('b', { head: ha })`, RuleLandWithoutReview},
		"a review of another node than the verified one": {`
const a = await agent.call('build a', { agentType: 'executor', node: 'a', label: 'build:a' })
const b = await agent.call('build b', { agentType: 'executor', node: 'b', label: 'build:b' })
const ha = await crw.commit('a', a)
await crw.commit('b', b)
await crw.verify('a', ha)
return crw.review('b', ha, null)`, RuleLifecycleOrder},
		"the second land of one node": {`
const a = await agent.call('build a', { agentType: 'executor', node: 'a', label: 'build:a' })
const b = await agent.call('build b', { agentType: 'executor', node: 'b', label: 'build:b' })
const ha = await crw.commit('a', a)
await crw.verify('a', ha)
await crw.review('a', ha, null)
await crw.land('a', { head: ha })
await crw.land('b', { head: ha })
return crw.land('a', { head: ha })`, RuleLandTwice},
	} {
		r := lintText(t, row.body)
		if !slices.Contains(rules(r), row.rule) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// The trace keeps a phase for each node of a path as well.
	row := func(op, node string) engine.Event {
		return engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: op, Path: "root", Data: map[string]any{"node": node}}
	}
	build := func(node string) engine.Event {
		return engine.Event{Kind: engine.KindCall, Event: engine.EvPrepared, Op: "agent", Path: "root", Label: "build:" + node, Data: map[string]any{"family": "executor", "role": "executor", "node": node}}
	}
	round := func(node string) []engine.Event {
		return []engine.Event{row("crw.commit", node), row("crw.verify", node), row("crw.review", node), row("crw.land", node)}
	}
	for name, row := range map[string]struct {
		events []engine.Event
		want   []string
	}{
		"nodes one after the other": {append(append([]engine.Event{build("a")}, round("a")...), append([]engine.Event{build("b")}, round("b")...)...), nil},
		"a land of another node":    {[]engine.Event{build("a"), build("b"), row("crw.commit", "a"), row("crw.verify", "a"), row("crw.review", "a"), row("crw.land", "b")}, []string{RuleLandWithoutReview}},
	} {
		got := rules(Report{Diagnostics: traceDiagnostics(&engine.Result{Status: engine.RunCompleted}, row.events, nil)})
		if !slices.Equal(got, row.want) {
			t.Errorf("trace, %s: %v, want %v", name, got, row.want)
		}
	}
}

// A return or throw in a try body reaches the finally block with the state it had there.
func TestLintLifecycleThroughFinally(t *testing.T) {
	const prefix = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
`
	if r := lintText(t, prefix+`
try {
  await crw.verify('n', head)
  await crw.review('n', head, null)
  return 'reviewed'
} finally {
  await crw.land('n', { head })
}`); !r.OK() {
		t.Errorf("a land in finally after a completed review: %v", r.Diagnostics)
	}
	r := lintText(t, prefix+`
try {
  await crw.verify('n', head)
  await crw.review('n', head, null)
  await crw.land('n', { head })
  return 'landed'
} finally {
  await crw.land('n', { head })
}`)
	wantRules(t, r, RuleLandTwice)
	// The order after the statement is the order of the paths that reach it.
	r = lintText(t, prefix+`
try {
  await crw.verify('n', head)
  await crw.review('n', head, null)
} finally {
  await crw.land('n', { head })
}
return crw.land('n', { head })`)
	wantRules(t, r, RuleLandTwice)
	// A handler may start after any call of the body.
	if r := lintText(t, prefix+`
try {
  await crw.verify('n', head)
  await crw.review('n', head, null)
  throw new Error('late')
} catch (e) {
  await crw.land('n', { head })
}
return 1`); !r.OK() {
		t.Errorf("%v", r.Diagnostics)
	}
	// A break or continue that leaves through a finally block reaches its loop's exit with the state the block left.
	r = lintText(t, prefix+`
await crw.verify('n', head)
await crw.review('n', head, null)
while (true) {
  try { break } finally { await crw.land('n', { head }) }
}
return crw.land('n', { head })`)
	wantRules(t, r, RuleLandTwice)
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Line != 11 {
		t.Errorf("the second land is after the loop, line 11: %+v", r.Diagnostics)
	}
	for name, body := range map[string]string{
		"a review in the finally block of a break": `
while (true) {
  try { break } finally { await crw.verify('n', head); await crw.review('n', head, null) }
}
return crw.land('n', { head })`,
		"a review in the finally block of a continue": `
for (let i = 0; i < 1; i++) {
  try { continue } finally { await crw.verify('n', head); await crw.review('n', head, null) }
}
return crw.land('n', { head })`,
		"a throw the handler takes": `
try { throw new Error('caught') }
catch (e) { await crw.verify('n', head); await crw.review('n', head, null) }
finally { await crw.land('n', { head }) }
return 1`,
		"a return in the finally block replaces the break": `
await crw.verify('n', head)
await crw.review('n', head, null)
while (true) {
  try { break } finally { await crw.land('n', { head }); return 'landed' }
}
return crw.land('n', { head })`,
	} {
		if r := lintText(t, prefix+body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// A throw no handler takes reaches the finally block with its own state.
	r = lintText(t, prefix+`
await crw.verify('n', head)
await crw.review('n', head, null)
try {
  await crw.land('n', { head })
  throw new Error('after the land')
} finally {
  await crw.land('n', { head })
}`)
	wantRules(t, r, RuleLandTwice)
}

// What a stepDecision reads is the id crw.tierStep returned and still holds when the call runs.
func TestLintTierStepProvenance(t *testing.T) {
	const call = `agent.call('go', { agentType: 'executor-t1', stepDecision: step.id, label: 't1' })`
	for name, body := range map[string]string{
		"overwritten before the call": `let step = await crw.tierStep('n', {})
step = { id: 'not-a-decision' }
return ` + call,
		"assigned after the call": `let step = { id: 'early' }
const out = await ` + call + `
step = await crw.tierStep('n', {})
return out`,
		"only on one branch": `let step = { id: 'x' }
if (args.deep) step = await crw.tierStep('n', {})
return ` + call,
		"declared without a value": `let step
return ` + call,
		"a decision of the parent path in a parallel branch": `const step = await crw.tierStep('n', {})
const out = await parallel([async () => ` + call + `])
return out[0]`,
		"a decision of the parent path in a pipeline stage": `const step = await crw.tierStep('n', {})
const out = await pipeline(['a'], (x) => ` + call + `)
return out[0]`,
	} {
		r := lintText(t, body)
		if !slices.Contains(rules(r), RuleTierStepDecision) {
			t.Errorf("%s: %v", name, r.Diagnostics)
			continue
		}
		static := false
		for _, d := range r.Diagnostics {
			static = static || d.Source == sourceStatic
		}
		if !static {
			t.Errorf("%s: the text shows it: %v", name, r.Diagnostics)
		}
	}
	for name, body := range map[string]string{
		"a decision on every branch": `let step
if (args && args.deep) step = await crw.tierStep('n', { deep: true })
else step = await crw.tierStep('n', {})
return ` + call,
		"overwritten by another decision": `let step = await crw.tierStep('n', {})
step = await crw.tierStep('n', { again: true })
return ` + call,
		"read through a second name": `const first = await crw.tierStep('n', {})
const step = { id: first.id }
return ` + call,
		// A plain function call runs on the caller's path: what a helper reads from its closure or a parameter is for
		// the dry run to judge.
		"a helper that reads the decision from its closure": `const step = await crw.tierStep('n', {})
async function build() { return ` + call + ` }
return build()`,
		"a helper that takes the decision as a parameter": `async function build(step) { return ` + call + ` }
const step = await crw.tierStep('n', {})
return build(step)`,
		"a pipeline stage that decides on its own path": `const out = await pipeline(['a'], async (x) => {
  const step = await crw.tierStep('n', { x })
  return ` + call + `
})
return out[0]`,
	} {
		r := lintText(t, body)
		if slices.Contains(rules(r), RuleTierStepDecision) && r.Diagnostics[0].Source == sourceStatic {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
		if strings.HasPrefix(name, "a helper") && (!r.OK() || r.DryRun != "ran") {
			t.Errorf("%s: the dry run sees the decision on the call's path: %v (%s)", name, r.Diagnostics, r.DryRun)
		}
	}
	// The trace refuses an id that no crw.tierStep returned on the path, however the script came by it.
	prepared := func(path, step string) engine.Event {
		return engine.Event{Kind: engine.KindCall, Event: engine.EvPrepared, Op: "agent", Path: path, Label: "t1", Data: map[string]any{"role": "executor-t1", "family": "executor", "step_decision": step, "explicit_label": true}}
	}
	decided := func(path, step string) engine.Event {
		return engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: "crw.tierStep", Path: path, Data: map[string]any{"step_decision": step}}
	}
	for name, row := range map[string]struct {
		events []engine.Event
		want   []string
	}{
		"an id the path decided":     {[]engine.Event{decided("root", "step_1"), prepared("root", "step_1")}, nil},
		"an id nothing decided":      {[]engine.Event{prepared("root", "made-up")}, []string{RuleTierStepDecision}},
		"an id decided later":        {[]engine.Event{prepared("root", "step_1"), decided("root", "step_1")}, []string{RuleTierStepDecision}},
		"an id of another path":      {[]engine.Event{decided("root", "step_1"), prepared("root/1:0", "step_1")}, []string{RuleTierStepDecision}},
		"no id (the engine refuses)": {[]engine.Event{prepared("root", "")}, nil},
	} {
		got := rules(Report{Diagnostics: traceDiagnostics(&engine.Result{Status: engine.RunCompleted}, row.events, nil)})
		if !slices.Equal(got, row.want) {
			t.Errorf("trace, %s: %v, want %v", name, got, row.want)
		}
	}
}

// A review is independent by the model family of its pair, whatever the role is called, and a continuation keeps the
// pair of its thread.
func TestLintReviewerModelFamily(t *testing.T) {
	// In the placeholder table the executor and the explorer run models of one family, the reviewer another.
	r := lintText(t, `
await agent.call('build', { agentType: 'executor', label: 'build' })
return agent.call('review the change', { agentType: 'explorer', label: 'review:build' })`)
	wantRules(t, r, RuleReviewerFamily)
	if r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Line != 4 {
		t.Errorf("%+v", r.Diagnostics)
	}
	r = lintText(t, `
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
return agent.call('review the change', { resume: ex.handle, label: 'review:build' })`)
	wantRules(t, r, RuleReviewerFamily)
	if r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Line != 4 {
		t.Errorf("a continuation of the author's thread: %+v", r.Diagnostics)
	}
	// A continuation of a reviewer's thread is a reviewer's call; a continuation of a fix round that is not a review
	// is not judged.
	for name, body := range map[string]string{
		"a reviewer continued": `
await agent.call('build', { agentType: 'executor', label: 'build' })
const rv = await agent.call('review', { agentType: 'reviewer', label: 'review:build' })
return agent.call('and again', { resume: rv.handle, label: 'review:again' })`,
		"a fix round": `
const ex = await agent.call('build', { agentType: 'executor', label: 'build' })
return agent.call('fix', { resume: ex.handle, label: 'fix:1' })`,
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// The trace judges the pair each call was configured with: a continuation of the author's thread, and a role of
	// the author's model family under another name.
	call := func(label, role, family, model, resumeOf string) engine.Event {
		d := map[string]any{"role": role, "family": family, "model": model, "explicit_label": true}
		if resumeOf != "" {
			d["resume_of"] = resumeOf
		}
		return engine.Event{Kind: engine.KindCall, Event: engine.EvPrepared, Op: "agent", Path: "root", Label: label, Data: d}
	}
	author := call("build", "executor", "executor", "anthropic/claude-haiku-5", "")
	for name, row := range map[string]struct {
		events []engine.Event
		want   []string
	}{
		"another family":            {[]engine.Event{author, call("review:build", "reviewer", "reviewer", "gpt-6.1-sol", "")}, nil},
		"the explorer's model":      {[]engine.Event{author, call("review:build", "explorer", "explorer", "anthropic/claude-haiku-5", "")}, []string{RuleReviewerFamily}},
		"a continuation of author":  {[]engine.Event{author, call("review:build", "executor", "executor", "anthropic/claude-haiku-5", "c_1")}, []string{RuleReviewerFamily}},
		"a continuation of a judge": {[]engine.Event{author, call("review:build", "reviewer", "reviewer", "gpt-6.1-sol", "c_2")}, nil},
	} {
		got := rules(Report{Diagnostics: traceDiagnostics(&engine.Result{Status: engine.RunCompleted}, row.events, nil)})
		if !slices.Equal(got, row.want) {
			t.Errorf("trace, %s: %v, want %v", name, got, row.want)
		}
	}
}

// A label in a loop is a duplicate by the text only when the text shows the call runs twice.
func TestLintLabelInLoopOnlyWhenShownTwice(t *testing.T) {
	for name, body := range map[string]string{
		"a loop of one round":           "for (let i = 0; i < 1; i++) {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a call behind a condition":     "for (let i = 0; i < 2; i++) {\n  if (i === 0) await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a loop over an unknown list":   "for (const x of args.items || []) {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a while loop":                  "let n = 0\nwhile (n < 1) {\n  n++\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn n",
		"a continue before the call":    "for (let i = 0; i < 2; i++) {\n  if (i === 0) continue\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a counter written in the body": "for (let i = 0; i < 2; i++) {\n  i = 5\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a conditional expression":      "for (let i = 0; i < 2; i++) {\n  i ? await agent('look', { agentType: 'explorer', label: 'look' }) : 0\n}\nreturn 1",
		"a do-while of one round":       "do {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n} while (false)\nreturn 1",
		"a do-while that stops at once": "let n = 0\ndo {\n  n++\n  await agent('look', { agentType: 'explorer', label: 'look' })\n} while (n < 1)\nreturn n",
	} {
		if r := lintText(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// A do-while that runs twice repeats its label; the text does not show it, the dry run does.
	r := lintText(t, "let n = 0\ndo {\n  n++\n  await agent('look', { agentType: 'explorer', label: 'look' })\n} while (n < 2)\nreturn n")
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Rule != RuleLabelDuplicate || r.Diagnostics[0].Source != sourceTrace {
		t.Errorf("a do-while of two rounds: %v", r.Diagnostics)
	}
	// After its first round a do-while's label is used: a call after the loop with the same label repeats it.
	r = lintText(t, "do {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n} while (false)\nreturn agent('again', { agentType: 'explorer', label: 'look' })")
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Rule != RuleLabelDuplicate || r.Diagnostics[0].Source != sourceStatic || r.Diagnostics[0].Line != 5 {
		t.Errorf("a label after a do-while: %v", r.Diagnostics)
	}
	for name, body := range map[string]string{
		"two rounds":        "for (let i = 0; i < 2; i++) {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"an inclusive test": "for (let i = 0; i <= 1; i++) {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a list literal":    "for (const x of ['a', 'b']) {\n  await agent('look', { agentType: 'explorer', label: 'look' })\n}\nreturn 1",
		"a nested loop":     "for (let i = 0; i < 2; i++) {\n  for (const x of args.items || []) {\n    await agent('look', { agentType: 'explorer', label: 'look' })\n  }\n  await agent('again', { agentType: 'explorer', label: 'again' })\n}\nreturn 1",
	} {
		r := lintText(t, body)
		if name == "a nested loop" {
			if len(r.Diagnostics) != 1 || r.Diagnostics[0].Label != "again" || r.Diagnostics[0].Source != sourceStatic {
				t.Errorf("%s: only the call outside the unknown loop is shown twice: %v", name, r.Diagnostics)
			}
			continue
		}
		if len(r.Diagnostics) != 1 || r.Diagnostics[0].Rule != RuleLabelDuplicate || r.Diagnostics[0].Source != sourceStatic {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

// A name the header imports is the script's own binding: a helper imported as Date is not the engine's Date, and the
// static rules read it like a const of the body.
func TestLintImportedNamesAreScriptBindings(t *testing.T) {
	dir := fstest.MapFS{"clock.js": {Data: []byte("export function now() { return 42 }\nexport default function stamp() { return 7 }\n")}}
	check := func(t *testing.T, text string) Report {
		t.Helper()
		return Check(context.Background(), script.Source{Name: "imp.js", Text: []byte(text), Dir: dir}, Options{DryRun: true})
	}
	for name, text := range map[string]string{
		"a named import called Date":    "import { now as Date } from './clock.js'; export const meta = { name: 'local-clock' }; return Date();",
		"a default import named Date":   "import Date from './clock.js'\nexport const meta = { name: 'local-clock' }\nreturn Date()",
		"a namespace import named Math": "import * as Math from './clock.js'\nexport const meta = { name: 'local-math' }\nreturn Math.now()",
		"an import named agent":         "import { now as agent } from './clock.js'\nexport const meta = { name: 'agent-import' }\nreturn agent('x', { agentType: 'nope' })",
	} {
		if r := check(t, text); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// The engine's own Date is still judged when the script imports something else.
	r := check(t, "import { now } from './clock.js'\nexport const meta = { name: 'global-clock' }\nreturn Date() + now()")
	wantRules(t, r, RuleDeterminism)
}

// A break or continue with a label goes to the statement the label names, through any loop between.
func TestLintLabeledJumps(t *testing.T) {
	const build = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
`
	for name, row := range map[string]struct{ body, rule string }{
		"break to an outer endless loop leaves it": {`
outer: for (;;) { for (;;) { break outer } }
return crw.land('n', {})`, RuleLandWithoutReview},
		"break to an outer while(true)": {`
outer: while (true) { while (true) { break outer } }
return crw.land('n', {})`, RuleLandWithoutReview},
		"break to a labeled block": {`
blk: { if (args.x) break blk; await crw.verify('n', head) }
return crw.land('n', {})`, RuleLandWithoutReview},
		"a continue to the outer loop through an inner one that may not run": {`
outer: for (const x of args.xs) { for (const y of args.ys) { continue outer } await crw.verify('n', head); await crw.review('n', head, null) }
return crw.land('n', {})`, ""},
		"a continue to the outer loop out of an endless inner one": {`
outer: for (const x of args.xs) { while (true) { continue outer } await crw.verify('n', head); await crw.review('n', head, null) }
return crw.land('n', {})`, RuleLandWithoutReview},
		"a break to the outer loop through finally": {`
outer: for (;;) { for (;;) { try { break outer } finally { 1 } } }
return crw.land('n', {})`, RuleLandWithoutReview},
		"a break to the inner loop does not leave the outer": {`
outer: for (;;) { for (;;) { break } }
return crw.land('n', {})`, ""},
		"a break outer after the review": {`
await crw.verify('n', head)
outer: for (;;) { for (;;) { await crw.review('n', head, null); break outer } }
return crw.land('n', {})`, ""},
		"labels on a nested label": {`
a: b: for (;;) { for (;;) { break a } }
return crw.land('n', {})`, RuleLandWithoutReview},
	} {
		r := lintText(t, build+row.body)
		if row.rule == "" {
			if !r.OK() {
				t.Errorf("%s: %v", name, r.Diagnostics)
			}
			continue
		}
		if !slices.Contains(rules(r), row.rule) {
			t.Errorf("%s: want %s, got %v", name, row.rule, r.Diagnostics)
		}
	}
}

// Invoking a lifecycle call does not complete it: the node is in the next phase once the call's promise is awaited,
// returned, or used in a way the text cannot rule out.
func TestLintLifecycleCompletionNeedsTheAwait(t *testing.T) {
	const build = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
`
	for name, body := range map[string]string{
		"a review that is dropped": `
crw.review('n', head, null)
return crw.land('n', { head })`,
		"a review stored and never read": `
const pending = crw.review('n', head, null)
return crw.land('n', { head })`,
		"a review assigned and never read": `
let p
p = crw.review('n', head, null)
return crw.land('n', { head })`,
		"a voided review": `
void crw.review('n', head, null)
return crw.land('n', { head })`,
	} {
		r := lintText(t, build+body)
		if !slices.Contains(rules(r), RuleLandWithoutReview) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	for name, body := range map[string]string{
		"an awaited review": `
await crw.review('n', head, null)
return crw.land('n', { head })`,
		"a review stored and then awaited": `
const p = crw.review('n', head, null)
await p
return crw.land('n', { head })`,
		"a review stored and awaited in Promise.all": `
const p = crw.review('n', head, null)
await Promise.all([p])
return crw.land('n', { head })`,
		"a review read in a closure": `
const p = crw.review('n', head, null)
const wait = async () => { await p }
await wait()
return crw.land('n', { head })`,
		"a review passed to Promise.all": `
await Promise.all([crw.review('n', head, null)])
return crw.land('n', { head })`,
		"a review awaited on one path of two": `
const p = crw.review('n', head, null)
if (args.a) { await p }
return crw.land('n', { head })`,
		"a returned land": `
await crw.review('n', head, null)
return crw.land('n', { head })`,
	} {
		if r := lintText(t, build+body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// Each of the four calls is judged alike.
	r := lintText(t, `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
crw.commit('n', ex)
await crw.verify('n', 'h')
return 1`)
	if !slices.Contains(rules(r), RuleLifecycleOrder) {
		t.Errorf("a dropped commit leaves the node built, so verify is out of order: %v", r.Diagnostics)
	}
}

// lintStatic checks a script with the static rules only: what the walker shows, without the dry run.
func lintStatic(t *testing.T, body string) Report {
	t.Helper()
	return Check(context.Background(), script.Source{Name: "inline.js", Text: []byte("export const meta = { name: 'inline' }\n" + body)}, Options{})
}

func TestLintLifecyclePromiseContexts(t *testing.T) {
	const built = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
`
	// The review's promise is discarded wherever the value of the expression around it is: under void, in a sequence
	// but its last part, on the left of &&, in a test, in a comparison or a template, as a read of the name that holds it
	// in such a place.
	for name, body := range map[string]string{
		"a void initializer":            "const ignored = void crw.review('n', head, null)\nreturn crw.land('n', { head })",
		"a void in a returned sequence": "return (void crw.review('n', head, null), crw.land('n', { head }))",
		"a sequence that drops it":      ";(crw.review('n', head, null), 1)\nreturn crw.land('n', { head })",
		"an awaited void":               "await void crw.review('n', head, null)\nreturn crw.land('n', { head })",
		"a void argument":               "await Promise.all([void crw.review('n', head, null)])\nreturn crw.land('n', { head })",
		"the left side of &&":           "await (crw.review('n', head, null) && 1)\nreturn crw.land('n', { head })",
		"an if test":                    "if (crw.review('n', head, null)) { }\nreturn crw.land('n', { head })",
		"a negation":                    "const ok = !crw.review('n', head, null)\nreturn crw.land('n', { head })",
		"a comparison":                  "const same = crw.review('n', head, null) === null\nreturn crw.land('n', { head })",
		"a template":                    "const text = `${crw.review('n', head, null)}`\nreturn crw.land('n', { head })",
		"a held promise only voided":    "const p = crw.review('n', head, null)\nvoid p\nreturn crw.land('n', { head })",
		"an assignment voided":          "let p\nvoid (p = crw.review('n', head, null))\nreturn crw.land('n', { head })",
	} {
		r := lintStatic(t, built+body)
		if !slices.Contains(rules(r), RuleLandWithoutReview) {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
	// Where the value of the expression is awaited, returned or kept, the promise in it is too.
	for name, body := range map[string]string{
		"an awaited assignment":             "let p\nawait (p = crw.review('n', head, null))\nreturn crw.land('n', { head })",
		"an assignment then awaited":        "let p\np = crw.review('n', head, null)\nawait p\nreturn crw.land('n', { head })",
		"a chained assignment, one read":    "let p, q\nq = p = crw.review('n', head, null)\nawait p\nreturn crw.land('n', { head })",
		"a chained assignment, other read":  "let p, q\nq = p = crw.review('n', head, null)\nawait q\nreturn crw.land('n', { head })",
		"an initializer from an assignment": "let p\nconst q = (p = crw.review('n', head, null))\nawait q\nreturn crw.land('n', { head })",
		"an object shorthand":               "const p = crw.review('n', head, null)\nawait ({ p }).p\nreturn crw.land('n', { head })",
		"an object property":                "const p = crw.review('n', head, null)\nawait ({ p: p }).p\nreturn crw.land('n', { head })",
		"a shorthand read in a closure":     "const p = crw.review('n', head, null)\nconst wait = async () => { await ({ p }).p }\nawait wait()\nreturn crw.land('n', { head })",
		"the last part of a sequence":       "await (1, crw.review('n', head, null))\nreturn crw.land('n', { head })",
		"the left side of ||":               "await (crw.review('n', head, null) || 1)\nreturn crw.land('n', { head })",
		"a conditional branch":              "await (args.a ? crw.review('n', head, null) : crw.review('n', head, null))\nreturn crw.land('n', { head })",
		"a then callback":                   "await crw.review('n', head, null).then(() => 1)\nreturn crw.land('n', { head })",
	} {
		if r := lintStatic(t, built+body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}

func TestLintLifecycleCountsTheInvocation(t *testing.T) {
	const reviewed = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
await crw.review('n', head, null)
`
	// A call starts its event whether or not its promise is awaited: a land runs once, and a commit started after the
	// review needs a new round before the land.
	for name, c := range map[string]struct {
		body string
		rule string
	}{
		"a dropped land, then a returned land":  {"crw.land('n', { head })\nreturn crw.land('n', { head })", RuleLandTwice},
		"a held land, then a returned land":     {"const first = crw.land('n', { head })\nreturn crw.land('n', { head })", RuleLandTwice},
		"a voided land, then a returned land":   {"void crw.land('n', { head })\nreturn crw.land('n', { head })", RuleLandTwice},
		"a dropped commit after the review":     {"crw.commit('n', ex)\nreturn crw.land('n', { head })", RuleLandWithoutReview},
		"a held commit after the review":        {"const next = crw.commit('n', ex)\nreturn crw.land('n', { head })", RuleLandWithoutReview},
		"a held commit, awaited, no new review": {"const next = crw.commit('n', ex)\nawait next\nreturn crw.land('n', { head })", RuleLandWithoutReview},
	} {
		if r := lintStatic(t, reviewed+c.body); !slices.Contains(rules(r), c.rule) {
			t.Errorf("%s: want %s, got %v", name, c.rule, r.Diagnostics)
		}
	}
	// A commit held by a name completes where the name is read: the next round goes on from there.
	r := lintStatic(t, reviewed+`
const next = crw.commit('n', ex)
const h2 = await next
await crw.verify('n', h2)
await crw.review('n', h2, null)
return crw.land('n', { head: h2 })`)
	if !r.OK() {
		t.Errorf("a held commit awaited before the next round: %v", r.Diagnostics)
	}
	// One awaited land is clean.
	if r := lintStatic(t, reviewed+"return crw.land('n', { head })"); !r.OK() {
		t.Errorf("one land: %v", r.Diagnostics)
	}
}

func TestLintPendingEventCannotUndoAStartedOne(t *testing.T) {
	const oldReviewPending = `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
const oldReview = crw.review('n', head, null)
await crw.review('n', head, null)
`
	// A review that settles after a land or a commit of its node started belongs to the head before them: it neither
	// takes back the land nor makes the new head reviewed.
	for name, c := range map[string]struct {
		body string
		rule string
	}{
		"a dropped land, then the old review":      {"crw.land('n', { head })\nawait oldReview\nreturn crw.land('n', { head })", RuleLandTwice},
		"an awaited land, then the old review":     {"await crw.land('n', { head })\nawait oldReview\nreturn crw.land('n', { head })", RuleLandTwice},
		"a dropped commit, then the old review":    {"crw.commit('n', ex)\nawait oldReview\nreturn crw.land('n', { head })", RuleLandWithoutReview},
		"a new commit and verify, the old review":  {"const next = await crw.commit('n', ex)\nawait crw.verify('n', next)\nawait oldReview\nreturn crw.land('n', { head: next })", RuleLandWithoutReview},
		"a rebuild, then the old review":           {"await agent.call('rebuild', { agentType: 'executor', node: 'n', label: 'rebuild' })\nawait oldReview\nreturn crw.land('n', { head })", RuleLandWithoutReview},
		"a held land awaited, then the old review": {"const first = crw.land('n', { head })\nawait first\nawait oldReview\nreturn crw.land('n', { head })", RuleLandTwice},
	} {
		if r := lintStatic(t, oldReviewPending+c.body); !slices.Contains(rules(r), c.rule) {
			t.Errorf("%s: want %s, got %v", name, c.rule, r.Diagnostics)
		}
	}
	// Held or awaited calls of the current round complete as before, and an old review that settles after a full new
	// round leaves that round's review in place.
	for name, body := range map[string]string{
		"one land after the old review settles": oldReviewPending + "await oldReview\nreturn crw.land('n', { head })",
		"a full new round, then the old review": oldReviewPending + "const next = await crw.commit('n', ex)\nawait crw.verify('n', next)\nawait crw.review('n', next, null)\nawait oldReview\nreturn crw.land('n', { head: next })",
		"a held commit, verify and review": `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const c = crw.commit('n', ex)
const head = await c
const v = crw.verify('n', head)
await v
const r = crw.review('n', head, null)
await r
const l = crw.land('n', { head })
return await l`,
		"awaited commit, verify, review and land": `
const ex = await agent.call('build', { agentType: 'executor', node: 'n', label: 'build' })
const head = await crw.commit('n', ex)
await crw.verify('n', head)
await crw.review('n', head, null)
return crw.land('n', { head })`,
	} {
		if r := lintStatic(t, body); !r.OK() {
			t.Errorf("%s: %v", name, r.Diagnostics)
		}
	}
}
