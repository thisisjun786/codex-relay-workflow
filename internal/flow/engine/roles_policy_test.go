package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// CRW-1196: the role table a flow run resolves calls against is the operator's policy file (agent-roles and
// agent-aliases), and the alias table maps a Claude script's options onto a role without applying its effort.

// policyFile is an execution policy whose executor pair is the argument.
func policyFile(executorModel, executorEffort string) string {
	return `{"agent-roles": {
  "architect": {"model": "claude-opus", "reasoningEffort": "xhigh"},
  "executor": {"model": "` + executorModel + `", "reasoningEffort": "` + executorEffort + `"},
  "executor-t1": {"model": "claude-sonnet", "reasoningEffort": "xhigh"},
  "reviewer": {"model": "gpt-sol", "reasoningEffort": "high"},
  "explorer": {"model": "claude-haiku", "reasoningEffort": "high"}}}`
}

func loadPolicy(t *testing.T, text string) *Policy {
	t.Helper()
	exec, err := execution.FromBytes([]byte(text), "policy.json")
	if err != nil {
		t.Fatal(err)
	}
	table, err := PolicyFromExecution(exec, "/etc/crw/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func fileHarness(t *testing.T, text string) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Policy = loadPolicy(t, text)
	return h
}

type callView struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Handle string `json:"handle"`
	Meta   struct {
		Role           string `json:"role"`
		RequestedPair  Pair   `json:"requestedPair"`
		ConfiguredPair Pair   `json:"configuredPair"`
	} `json:"meta"`
}

func callViews(t *testing.T, res *Result) map[string]callView {
	t.Helper()
	out := map[string]callView{}
	for k, v := range value[map[string]json.RawMessage](t, res) {
		var c callView
		if err := json.Unmarshal(v, &c); err != nil {
			t.Fatal(err)
		}
		out[k] = c
	}
	return out
}

// refusal is the detail and cause the journal keeps for the refused call with the label.
func refusal(t *testing.T, h *harness, label string) map[string]any {
	t.Helper()
	for _, e := range h.callRows(label) {
		if e.Event == EvRefused {
			return e.Data
		}
	}
	t.Fatalf("call %s was not refused", label)
	return nil
}

func TestPolicyFromExecutionTable(t *testing.T) {
	table := loadPolicy(t, policyFile("claude-haiku", "xhigh"))
	if table.Source != "/etc/crw/policy.json" || len(table.Digest) != 64 {
		t.Fatalf("source %q digest %q", table.Source, table.Digest)
	}
	want := map[string]struct{ family, sandbox, model string }{
		"architect": {"architect", SandboxReadOnly, "claude-opus"}, "executor": {"executor", SandboxWorkspaceWrite, "claude-haiku"},
		"executor-t1": {"executor", SandboxWorkspaceWrite, "claude-sonnet"}, "reviewer": {"reviewer", SandboxReadOnly, "gpt-sol"},
		"explorer": {"explorer", SandboxReadOnly, "claude-haiku"},
	}
	for role, w := range want {
		spec := table.Roles[role]
		if spec.Family != w.family || spec.Sandbox != w.sandbox || spec.Pair.Model != w.model {
			t.Errorf("%s: %+v", role, spec)
		}
	}
	// A file written before CRW-1450 still declares reviewer-final: it loads, and the table has no such role (a retired key).
	old := loadPolicy(t, strings.Replace(policyFile("claude-haiku", "xhigh"), `"explorer":`, `"reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"},
  "explorer":`, 1))
	if _, ok := old.Roles["reviewer-final"]; ok || len(old.Roles) != len(execution.AgentRoleNames) {
		t.Errorf("the table of an old file: %+v", old.Roles)
	}
	// A policy without agent-roles is not a flow policy.
	exec, err := execution.FromBytes([]byte(`{"allowed": [{"model": "m", "efforts": ["high"]}]}`), "p.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PolicyFromExecution(exec, "p.json"); err == nil || !strings.Contains(err.Error(), "agent-roles") {
		t.Fatalf("%v", err)
	}
}

func TestAliasEffortMismatchRecordedNotRefused(t *testing.T) {
	h := fileHarness(t, policyFile("claude-haiku", "xhigh"))
	res := h.run(`
const alias = await agent.call('build', { model: 'haiku', effort: 'medium', isolation: 'worktree', label: 'alias' })
const explicit = await agent.call('look', { agentType: 'explorer', effort: 'low', label: 'explicit' })
const same = await agent.call('verify', { agentType: 'ocx-gpt-6-1-sol', effort: 'high', label: 'same' })
return { alias, explicit, same }`)
	got := callViews(t, res)
	for label, want := range map[string]struct {
		role      string
		requested Pair
		configure Pair
	}{
		"alias":    {"executor", Pair{"haiku", "medium"}, Pair{"claude-haiku", "xhigh"}},
		"explicit": {"explorer", Pair{"claude-haiku", "low"}, Pair{"claude-haiku", "high"}},
		"same":     {"reviewer", Pair{"gpt-sol", "high"}, Pair{"gpt-sol", "high"}},
	} {
		c := got[label]
		if c.Status != StatusCompleted || c.Meta.Role != want.role || c.Meta.RequestedPair != want.requested || c.Meta.ConfiguredPair != want.configure {
			t.Errorf("%s: %+v, want role %s requested %v configured %v", label, c, want.role, want.requested, want.configure)
		}
	}
	// The effort the script asked for is not what the thread runs: the backend was given the policy's pair.
	for _, label := range []string{"alias", "explicit"} {
		thread, ok := h.fake.Thread("thr_" + h.callRows(label)[0].CallID)
		if !ok || thread.Pair != got[label].Meta.ConfiguredPair {
			t.Errorf("%s: thread %+v", label, thread)
		}
	}
	prepared := h.callRows("alias")[0].Data
	if prepared["requested_effort"] != "medium" || prepared["effort"] != "xhigh" || prepared["sandbox"] != SandboxWorkspaceWrite {
		t.Errorf("the intent row records both: %+v", prepared)
	}
}

func TestExplicitAgentTypeForeignModelRefused(t *testing.T) {
	h := fileHarness(t, policyFile("claude-haiku", "xhigh"))
	res := h.run(`
const foreign = await agent.call('verify', { agentType: 'reviewer', model: 'haiku', label: 'foreign' })
const ownAlias = await agent.call('look', { agentType: 'explorer', model: 'haiku', effort: 'medium', label: 'own-alias' })
const ownModel = await agent.call('verify', { agentType: 'reviewer', model: 'gpt-sol', label: 'own-model' })
const familyAlias = await agent.call('build', { agentType: 'executor-t1', model: 'sonnet', stepDecision: 'step', label: 'family' })
const other = await agent.call('build', { agentType: 'executor', model: 'sonnet', label: 'other' })
return { foreign, ownAlias, ownModel, familyAlias, other }`)
	got := callViews(t, res)
	if c := got["foreign"]; c.Status != StatusRefused || c.Reason != ReasonPairRefused {
		t.Errorf("a reviewer on haiku: %+v", c)
	}
	if c := got["other"]; c.Status != StatusRefused || c.Reason != ReasonPairRefused {
		t.Errorf("an executor on sonnet: %+v", c)
	}
	if c := got["ownAlias"]; c.Status != StatusCompleted || c.Meta.Role != "explorer" || c.Meta.RequestedPair != (Pair{"haiku", "medium"}) || c.Meta.ConfiguredPair != (Pair{"claude-haiku", "high"}) {
		t.Errorf("an explorer on haiku at medium runs on the explorer pair: %+v", c)
	}
	for _, label := range []string{"ownModel", "familyAlias"} {
		if c := got[label]; c.Status != StatusCompleted {
			t.Errorf("%s: %+v", label, c)
		}
	}
	// A refused call starts no thread.
	for _, r := range h.fake.Requests() {
		if r.Kind == "thread" && (r.CallID == h.callRows("foreign")[0].CallID || r.CallID == h.callRows("other")[0].CallID) {
			t.Errorf("a refused call reached the backend: %+v", r)
		}
	}
}

func TestAliasNonRoleAgentTypeMapsToReviewer(t *testing.T) {
	h := fileHarness(t, policyFile("claude-haiku", "xhigh"))
	res := h.run(`
const high = await agent.call('verify', { agentType: 'ocx-gpt-6-1-sol', effort: 'high', label: 'high' })
const xhigh = await agent.call('verify', { agentType: 'ocx-gpt-6-1-sol', effort: 'xhigh', label: 'xhigh' })
const asModel = await agent.call('verify', { model: 'ocx-gpt-6-1-sol', label: 'as-model' })
const asModelXhigh = await agent.call('verify', { model: 'ocx-gpt-6-1-sol', effort: 'xhigh', label: 'as-model-xhigh' })
const unknown = await agent.call('verify', { agentType: 'gpt-x', label: 'unknown' })
const unknownModel = await agent.call('verify', { model: 'gpt-x', label: 'unknown-model' })
const nothing = await agent.call('verify', { label: 'nothing' })
return { high, xhigh, asModel, asModelXhigh, unknown, unknownModel, nothing }`)
	got := callViews(t, res)
	// ocx-gpt-6-1-sol is the reviewer at any effort: the xhigh row of the retired reviewer-final is gone (CRW-1450).
	for label, role := range map[string]string{"high": "reviewer", "xhigh": "reviewer", "asModel": "reviewer", "asModelXhigh": "reviewer"} {
		if c := got[label]; c.Status != StatusCompleted || c.Meta.Role != role {
			t.Errorf("%s: %+v, want %s", label, c, role)
		}
	}
	if c := got["high"]; c.Meta.RequestedPair != (Pair{"gpt-sol", "high"}) || c.Meta.ConfiguredPair != (Pair{"gpt-sol", "high"}) {
		t.Errorf("the reviewer pair: %+v", c.Meta)
	}
	if c := got["xhigh"]; c.Meta.RequestedPair != (Pair{"gpt-sol", "xhigh"}) || c.Meta.ConfiguredPair != (Pair{"gpt-sol", "high"}) {
		t.Errorf("the reviewer pair of an xhigh call: %+v", c.Meta)
	}
	for _, label := range []string{"unknown", "unknownModel", "nothing"} {
		if c := got[label]; c.Status != StatusRefused || c.Reason != ReasonPolicyRefused {
			t.Errorf("%s: %+v", label, c)
		}
	}
}

func TestAliasHaikuWithoutIsolationRefused(t *testing.T) {
	h := fileHarness(t, policyFile("claude-haiku", "xhigh"))
	res := h.run(`
const bare = await agent.call('build it', { model: 'haiku', effort: 'medium', label: 'bare' })
const fixed = await agent.call('build it', { model: 'haiku', effort: 'medium', isolation: 'worktree', label: 'fixed' })
const research = await agent.call('look', { agentType: 'explorer', model: 'haiku', effort: 'medium', label: 'research' })
return { bare, fixed, research }`)
	got := callViews(t, res)
	if c := got["bare"]; c.Status != StatusRefused || c.Reason != ReasonPolicyRefused || c.Handle != "" {
		t.Fatalf("%+v", c)
	}
	data := refusal(t, h, "bare")
	if data["cause"] != "alias_ambiguous" {
		t.Errorf("the refusal's detail kind: %+v", data)
	}
	detail, _ := data["detail"].(string)
	for _, fix := range []string{"isolation: 'worktree'", "agentType: 'explorer'"} {
		if !strings.Contains(detail, fix) {
			t.Errorf("the guidance does not name the fix %s: %s", fix, detail)
		}
	}
	// Both fixes run: the build as the executor, the investigation as the explorer.
	if got["fixed"].Meta.Role != "executor" || got["research"].Meta.Role != "explorer" {
		t.Errorf("fixed %+v research %+v", got["fixed"], got["research"])
	}
	// The first call reached the backend with nothing: no thread was requested for it.
	bareID := h.callRows("bare")[0].CallID
	for _, r := range h.fake.Requests() {
		if r.CallID == bareID {
			t.Errorf("the refused call reached the backend: %+v", r)
		}
	}
}

// resumeScript makes one fresh call and continues the thread of the handle in args.
const resumeScript = `
for (let i = 0; i < args.pad; i++) await crw.clock()
const fresh = await agent.call('build again', { isolation: 'worktree', model: 'haiku', label: 'fresh' })
const next = await agent.call('keep going', { resume: args.handle, label: 'continued' })
return { fresh, next }`

func TestPolicyRepinnedOnResumeWithNewDigest(t *testing.T) {
	old, changed := policyFile("claude-haiku", "xhigh"), policyFile("claude-haiku-2", "high")
	h := fileHarness(t, old)
	h.cfg.RunID = "run-pin"
	first := h.run(`
const built = await agent.call('build', { isolation: 'worktree', model: 'haiku', label: 'first' })
return { handle: built.handle }`)
	handle := value[struct{ Handle string }](t, first).Handle
	if handle == "" {
		t.Fatal("the first run left no handle")
	}
	// The run record names the policy file and its digest.
	started := h.journal.Events()[0]
	oldTable := loadPolicy(t, old)
	if started.Event != "started" || started.Data["policy_source"] != "/etc/crw/policy.json" || started.Data["policy_digest"] != oldTable.Digest {
		t.Fatalf("started row %+v", started.Data)
	}
	repinned := func() (rows []Event) {
		for _, e := range h.journal.Events() {
			if e.Event == EvPolicyRepinned {
				rows = append(rows, e)
			}
		}
		return rows
	}
	if len(repinned()) != 0 {
		t.Fatal("a first run repins nothing")
	}

	// The same file read again changes nothing.
	// A resumed run replays nothing in this build, so its calls take positions after the first run's: the clock reads
	// stand in for the positions the replay would have restored.
	h.cfg.Args = mustJSON(map[string]any{"handle": handle, "pad": 1})
	if res := h.run(resumeScript); res.Status != RunCompleted {
		t.Fatalf("%s %s", res.ErrorKind, res.Error)
	}
	if len(repinned()) != 0 {
		t.Fatalf("an unchanged digest is not a repin: %+v", repinned())
	}

	// The file now read has another digest.
	h.cfg.Policy = loadPolicy(t, changed)
	h.cfg.Args = mustJSON(map[string]any{"handle": handle, "pad": 4})
	before := len(h.journal.Events())
	res := h.run(resumeScript)
	rows := repinned()
	if len(rows) != 1 || rows[0].Data["old"] != oldTable.Digest || rows[0].Data["new"] != h.cfg.Policy.Digest || rows[0].Seq <= before {
		t.Fatalf("policy_repinned rows %+v", rows)
	}
	got := callViews(t, res)
	// Only the new call takes the new pair; the continuation keeps the thread's.
	if c := got["fresh"]; c.Status != StatusCompleted || c.Meta.ConfiguredPair != (Pair{"claude-haiku-2", "high"}) {
		t.Errorf("a new call takes the new pair: %+v", c)
	}
	if c := got["next"]; c.Status != StatusCompleted || c.Meta.ConfiguredPair != (Pair{"claude-haiku", "xhigh"}) {
		t.Errorf("a continuation keeps the handle's pair: %+v", c)
	}
	// The event is written before any live call of the resumed run.
	for _, e := range h.journal.Events() {
		if e.Seq > rows[0].Seq && e.Kind == KindCall && e.Event == EvPrepared {
			break
		}
		if e.Seq > before && e.Kind == KindCall && e.Op == "agent" && e.Seq < rows[0].Seq {
			t.Errorf("an agent call row precedes the repin: %+v", e)
		}
	}
	// The continuation sent its turn to the thread the first run started, which was never touched by the new pair.
	_, callID, _ := ParseHandle(handle)
	if thread, ok := h.fake.Thread("thr_" + callID); !ok || thread.Pair != (Pair{"claude-haiku", "xhigh"}) || len(thread.Turns) != 3 {
		t.Errorf("the first thread: %+v", thread)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyLoadedFromEnvironmentIsRecordedInTheRun(t *testing.T) {
	// CRW-1330: the execution policy file is still loaded and returned, but the role table is the helper role store's
	// (roles_source_test.go); under the test isolation with no store it is the placeholder table.
	withRoleStore(t, "")
	dir := t.TempDir()
	path := dir + "/policy.json"
	writeFile(t, path, policyFile("claude-haiku", "xhigh"))
	t.Setenv(execution.EnvPolicy, path)
	table, exec, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if exec.Digest() != loadPolicy(t, policyFile("claude-haiku", "xhigh")).Digest || table.Source != "" || table.Roles["executor"].Pair.Model != "claude-fake-haiku" {
		t.Fatalf("%q %q %+v", exec.Digest(), table.Source, table.Roles["executor"])
	}
	// A file that does not load is refused, never replaced by placeholders.
	writeFile(t, path, `{"allowed": []}`)
	if _, _, err := LoadPolicy(); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("%v", err)
	}
	// No variable: the fake backend's placeholders.
	t.Setenv(execution.EnvPolicy, "")
	if table, _, err = LoadPolicy(); err != nil || table.Source != "" || table.Roles["executor"].Pair.Model != "claude-fake-haiku" {
		t.Fatalf("%v %+v", err, table)
	}
}

func TestFlowRolesShowsPolicyAndDigest(t *testing.T) {
	// CRW-1330: the source and the digest are the helper role store's path and the canonical digest of its six pairs.
	store := withRoleStore(t, roleStore("anthropic/claude-haiku-5", "gpt-6.1-sol"))
	dir := t.TempDir()
	path := dir + "/policy.json"
	writeFile(t, path, policyFile("claude-haiku", "xhigh"))
	t.Setenv(execution.EnvPolicy, path)
	digest := snapshotDigest(t)

	code, out, errOut := runFlow(t, "roles", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d %q", code, errOut)
	}
	var report PolicyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Source == nil || *report.Source != store || *report.Digest != digest || report.Placeholder || report.Mode != "presence_only" {
		t.Fatalf("%+v", report)
	}
	if len(report.Roles) != len(execution.AgentRoleNames) || report.Roles[len(report.Roles)-1].Role != "critic" || report.Roles[0].Role != "architect" || report.Roles[1].Sandbox != SandboxWorkspaceWrite || len(report.Aliases) != 5 {
		t.Fatalf("%+v", report)
	}
	code, out, _ = runFlow(t, "roles")
	if code != 0 || !strings.Contains(out, "digest: "+digest) || !strings.Contains(out, "haiku -> refused (alias_ambiguous)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	withRoleStore(t, "")
	if code, out, _ = runFlow(t, "roles"); code != 0 || !strings.Contains(out, "placeholder") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	t.Setenv(execution.EnvPolicy, dir+"/missing.json")
	if code, _, errOut = runFlow(t, "roles"); code != 2 || !strings.Contains(errOut, "cannot read") {
		t.Fatalf("exit %d %q", code, errOut)
	}
}

func TestScriptFlagDefaultsToPlanOnly(t *testing.T) {
	t.Setenv(execution.EnvPolicy, "")
	code, out, errOut := runFlow(t, "run", "--script", "export const meta = { name: 'adhoc' }\nreturn await agent('hi', { agentType: 'explorer' })")
	if code != 0 || errOut != "" || !strings.Contains(out, `"planOnly": true`) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// Executing needs a backend named; a script file is never plan-only.
	if code, _, errOut = runFlow(t, "run", "--script", "return 1", "--plan-only=false"); code != 2 || !strings.Contains(errOut, "--backend is required") {
		t.Fatalf("exit %d %q", code, errOut)
	}
	code, out, _ = runFlow(t, "run", "--script", "export const meta = { name: 'adhoc' }\nreturn 1", "--plan-only=false", "--backend", "fake")
	if code != 0 || strings.Contains(out, "planOnly") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, _, errOut = runFlow(t, "run", "--plan-only", helloScript); code != 2 || !strings.Contains(errOut, "applies to --script") {
		t.Fatalf("exit %d %q", code, errOut)
	}
	if code, _, errOut = runFlow(t, "run", "--script", "return 1", helloScript); code != 2 || !strings.Contains(errOut, "exclude each other") {
		t.Fatalf("exit %d %q", code, errOut)
	}
}

func TestTierStepJournalsADecisionTheExecutorT1CallCarries(t *testing.T) {
	h := fileHarness(t, policyFile("claude-haiku", "xhigh"))
	res := h.run(`
const step = await crw.tierStep('node-1', { evidence: ['a', 'b'] })
const t1 = await agent.call('retry', { model: 'sonnet', stepDecision: step.id, label: 't1' })
const bare = await agent.call('retry', { model: 'sonnet', label: 'bare' })
return { step, t1, bare }`)
	var v struct {
		Step struct {
			ID         string  `json:"id"`
			Checkpoint *string `json:"checkpoint"`
		} `json:"step"`
	}
	if err := json.Unmarshal(res.Value, &v); err != nil || !strings.HasPrefix(v.Step.ID, "step_") || v.Step.Checkpoint != nil {
		t.Fatalf("%s %v", res.Value, err)
	}
	got := callViews(t, res)
	if c := got["t1"]; c.Status != StatusCompleted || c.Meta.Role != "executor-t1" {
		t.Errorf("%+v", c)
	}
	if c := got["bare"]; c.Status != StatusRefused || c.Reason != ReasonMissingStepDecision {
		t.Errorf("%+v", c)
	}
	prepared := h.callRows("t1")[0].Data
	if prepared["step_decision"] != v.Step.ID {
		t.Errorf("the call carries the decision: %+v", prepared)
	}
}

// A pair's autoCompactTokenLimit is part of the role table: the policy snapshot, the thread specification the backend
// receives and the roles report carry it, and a pair without one carries none.
func TestAutoCompactTokenLimitReachesTheThreadAndTheReport(t *testing.T) {
	text := strings.Replace(policyFile("claude-haiku", "xhigh"),
		`"executor": {"model": "claude-haiku", "reasoningEffort": "xhigh"}`,
		`"executor": {"model": "claude-haiku", "reasoningEffort": "xhigh", "autoCompactTokenLimit": 550000}`, 1)
	if text == policyFile("claude-haiku", "xhigh") {
		t.Fatal("the fixture did not change")
	}
	table := loadPolicy(t, text)
	if got := table.Roles["executor"].AutoCompactTokenLimit; got == nil || *got != 550000 {
		t.Fatalf("the role table drops the limit: %v", got)
	}
	if table.Roles["reviewer"].AutoCompactTokenLimit != nil {
		t.Fatal("a pair without a limit carries none")
	}
	h := newHarness(t)
	h.cfg.Policy = table
	specs := map[string]ThreadSpec{}
	h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
		specs[spec.Role] = spec
		return ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}, nil
	}
	h.run(`
await agent.call('build', { agentType: 'executor', label: 'build' })
await agent.call('review', { agentType: 'reviewer', label: 'review' })
return 1`)
	if got := specs["executor"].AutoCompactTokenLimit; got == nil || *got != 550000 {
		t.Errorf("the executor thread is created without the limit: %+v", specs["executor"])
	}
	if specs["reviewer"].AutoCompactTokenLimit != nil {
		t.Errorf("the reviewer thread has no limit: %+v", specs["reviewer"])
	}
	exec, err := execution.FromBytes([]byte(text), "policy.json")
	if err != nil {
		t.Fatal(err)
	}
	report := DescribePolicy(table, exec)
	var seen bool
	for _, r := range report.Roles {
		if r.Role == "executor" {
			seen = r.AutoCompactTokenLimit != nil && *r.AutoCompactTokenLimit == 550000
		} else if r.AutoCompactTokenLimit != nil {
			t.Errorf("%s reports a limit it does not have", r.Role)
		}
	}
	if !seen || !strings.Contains(report.Text(), "autoCompactTokenLimit 550000") {
		t.Errorf("the roles report omits the limit:\n%s", report.Text())
	}
}
