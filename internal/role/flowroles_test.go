package role

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// CRW-1330 (CRW-1316 S1): the flow's role table comes from the helper role store. Two flow-only keys, executor-t1 and
// reviewer-final, live in the same roles object as the four helper roles; Roles() (the CXC oracle port the GUI and the
// spawn hooks read) does not change.

const sixRoleStore = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "high"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh", "promptOverride": "final"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`

func flowPairs(s FlowSnapshot) map[RoleName][3]string {
	out := map[RoleName][3]string{}
	for _, r := range s.Roles {
		out[r.Role] = [3]string{r.Model, r.Effort, string(r.State)}
	}
	return out
}

func TestCRW1330_FlowSnapshotReadsTheSixRolesFromOneRolesObject(t *testing.T) {
	env, dir := home(t)
	path := writeStore(t, dir, sixRoleStore)
	snap := ReadFlowSnapshot(env)
	if snap.Path != path || !snap.Present || len(snap.Warnings) != 0 {
		t.Fatalf("snapshot %+v", snap)
	}
	var order []RoleName
	for _, r := range snap.Roles {
		order = append(order, r.Role)
	}
	if !reflect.DeepEqual(order, FlowRoles()) || !reflect.DeepEqual(FlowRoles(), []RoleName{Architect, Executor, ExecutorT1, Reviewer, ReviewerFinal, Explorer}) {
		t.Fatalf("order %v", order)
	}
	want := map[RoleName][3]string{
		Architect: {"anthropic/claude-opus-5-5", "xhigh", "store"}, Executor: {"anthropic/claude-haiku-5", "xhigh", "store"},
		ExecutorT1: {"anthropic/claude-sonnet-5-5", "high", "store"}, Reviewer: {"gpt-6.1-sol", "high", "store"},
		ReviewerFinal: {"gpt-6.1-sol", "xhigh", "store"}, Explorer: {"anthropic/claude-haiku-5", "high", "store"},
	}
	if got := flowPairs(snap); !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v", got)
	}
	// The oracle's four roles are what they were: the helper's settings never carry the flow-only keys.
	if !reflect.DeepEqual(Roles(), []RoleName{Explorer, Reviewer, Executor, Architect}) {
		t.Fatalf("Roles() changed: %v", Roles())
	}
	settings := must(ReadSettings(env))
	if _, ok := settings.Roles[ExecutorT1]; ok || len(settings.Roles) != 4 {
		t.Fatalf("settings %+v", settings.Roles)
	}
	if text := string(must(json.Marshal(settings))); strings.Contains(text, "executor-t1") || strings.Contains(text, "reviewer-final") {
		t.Fatalf("the helper's settings answer shows a flow-only key: %s", text)
	}
}

func TestCRW1330_MissingFlowKeysUseTheCompiledDefaultsAndWarn(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`)
	snap := ReadFlowSnapshot(env)
	got := flowPairs(snap)
	// today's agent-roles values, never the executor's or the reviewer's pair
	if got[ExecutorT1] != [3]string{"anthropic/claude-sonnet-5-5", "xhigh", "compiled_default"} || got[ReviewerFinal] != [3]string{"gpt-6.1-sol", "xhigh", "compiled_default"} {
		t.Fatalf("pairs %v", got)
	}
	if len(snap.Warnings) != 2 || !strings.Contains(snap.Warnings[0], "executor-t1") || !strings.Contains(snap.Warnings[1], "reviewer-final") || !strings.Contains(snap.Warnings[0], "compiled default") {
		t.Fatalf("warnings %q", snap.Warnings)
	}
}

func TestCRW1330_ModeDefaultRoleIsUndecided(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {"reviewer": {"mode": "default", "effort": "high"}, "explorer": {"mode": "model", "model": "m-explorer"}}}`)
	snap := ReadFlowSnapshot(env)
	got := flowPairs(snap)
	for _, r := range []RoleName{Architect, Executor, Reviewer} {
		if got[r] != [3]string{"", "", "undecided"} {
			t.Errorf("%s: %v", r, got[r])
		}
	}
	// a model with no effort: the model is decided, the effort is not
	if got[Explorer] != [3]string{"m-explorer", "", "store"} {
		t.Errorf("explorer: %v", got[Explorer])
	}
	joined := strings.Join(snap.Warnings, "\n")
	for _, want := range []string{"architect", "executor", "reviewer", "undecided", "explorer names model m-explorer and no effort"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	// A missing store is the same reading: four undecided roles, two compiled defaults, nothing written.
	env2, dir2 := home(t)
	snap2 := ReadFlowSnapshot(env2)
	if snap2.Present || len(snap2.Warnings) != 6 || flowPairs(snap2)[ExecutorT1][2] != "compiled_default" || flowPairs(snap2)[Explorer][2] != "undecided" {
		t.Fatalf("missing store: %+v", snap2)
	}
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatalf("reading the snapshot made the store directory: %v", err)
	}
}

func TestCRW1330_UnusableFlowKeyStopsOnlyItself(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {"executor": {"mode": "model", "model": "m-exec", "effort": "high"}, "reviewer-final": {"mode": "weird"}}}`)
	snap := ReadFlowSnapshot(env)
	got := flowPairs(snap)
	if got[ReviewerFinal] != [3]string{"gpt-6.1-sol", "xhigh", "compiled_default"} || got[Executor] != [3]string{"m-exec", "high", "store"} {
		t.Fatalf("pairs %v", got)
	}
	if joined := strings.Join(snap.Warnings, "\n"); !strings.Contains(joined, "reviewer-final") || !strings.Contains(joined, "invalid mode") {
		t.Fatalf("warnings %q", snap.Warnings)
	}
	// the helper's four roles do not see the flow-only key at all
	if _, err := ReadSettings(env); err != nil {
		t.Fatalf("an unusable flow-only key made the helper store unusable: %v", err)
	}
	// A store that cannot be read at all: every role without a compiled default is undecided, with the reason.
	writeStore(t, dir, `{not json`)
	snap = ReadFlowSnapshot(env)
	if !snap.Present || flowPairs(snap)[Executor][2] != "undecided" || flowPairs(snap)[ExecutorT1][2] != "compiled_default" || !strings.Contains(strings.Join(snap.Warnings, "\n"), "cannot be used") {
		t.Fatalf("unreadable store: %+v", snap)
	}
}

func TestCRW1330_DigestCoversTheSixPairsOnly(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, sixRoleStore)
	base := ReadFlowSnapshot(env).Digest()
	if len(base) != 64 {
		t.Fatalf("digest %q", base)
	}
	// prompt override, fallback and member order are not part of the digest
	writeStore(t, dir, strings.Replace(strings.Replace(sixRoleStore, `"promptOverride": "final"`, `"promptOverride": "other", "fallback": {"model": "m2", "effort": "low"}`, 1), `{"roles": {`, `{"extra": 1, "roles": {`, 1))
	if got := ReadFlowSnapshot(env).Digest(); got != base {
		t.Fatalf("a field outside the pairs changed the digest: %s != %s", got, base)
	}
	writeStore(t, dir, strings.Replace(sixRoleStore, `"effort": "high"},
  "reviewer"`, `"effort": "xhigh"},
  "reviewer"`, 1))
	if got := ReadFlowSnapshot(env).Digest(); got == base {
		t.Fatal("a changed pair kept the digest")
	}
}

func TestCRW1330_HelperSetGetResetTheFlowKeys(t *testing.T) {
	env, dir := home(t)
	// get of an absent flow key shows what the flow uses
	view := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"get", "executor-t1"}), env))
	if view["model"] != "anthropic/claude-sonnet-5-5" || view["effort"] != "xhigh" || view["source"] != "compiled_default" {
		t.Fatalf("get of an absent flow key: %v", view)
	}
	set := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"set", "reviewer-final", "--mode", "model", "--model", "gpt-7-sol", "--effort", "xhigh"}), env))
	if set["model"] != "gpt-7-sol" || set["effort"] != "xhigh" || set["mode"] != "model" || set["source"] != "store" {
		t.Fatalf("set: %v", set)
	}
	stored := readText(t, dir+"/"+StoreFile)
	var doc struct {
		Roles map[string]map[string]any `json:"roles"`
	}
	check(t, json.Unmarshal([]byte(stored), &doc))
	if doc.Roles["reviewer-final"]["model"] != "gpt-7-sol" {
		t.Fatalf("the store: %s", stored)
	}
	got := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"get", "reviewer-final"}), env))
	if got["model"] != "gpt-7-sol" || got["source"] != "store" {
		t.Fatalf("get: %v", got)
	}
	if p := flowPairs(ReadFlowSnapshot(env))[ReviewerFinal]; p != [3]string{"gpt-7-sol", "xhigh", "store"} {
		t.Fatalf("the snapshot: %v", p)
	}
	// the list keeps the helper's four roles (the CXC answer)
	cfg := helperCLIDecode[Config](t, RunHelper(ParseHelperArgs(nil), env))
	if len(cfg.Roles) != 4 {
		t.Fatalf("list %+v", cfg)
	}
	reset := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"reset", "reviewer-final"}), env))
	if reset["source"] != "compiled_default" || reset["model"] != "gpt-6.1-sol" {
		t.Fatalf("reset: %v", reset)
	}
	if strings.Contains(readText(t, dir+"/"+StoreFile), "reviewer-final") {
		t.Fatal("reset left the key")
	}
	// A set merges into what the file holds; an unusable value of the role being set is refused, not normalised away.
	writeStore(t, dir, `{"roles": {"executor-t1": {"mode": "weird", "model": "keep"}}}`)
	if r := RunHelper(ParseHelperArgs([]string{"set", "executor-t1", "--effort", "high"}), env); r.Code != 1 || !strings.Contains(r.Output, "invalid mode") {
		t.Fatalf("set over an unusable flow key: %+v", r)
	}
	if r := RunHelper(ParseHelperArgs([]string{"get", "executor-t1"}), env); r.Code != 1 || !strings.Contains(r.Output, "invalid mode") {
		t.Fatalf("get of an unusable flow key: %+v", r)
	}
}
