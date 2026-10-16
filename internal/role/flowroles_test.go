package role

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// CRW-1330 (CRW-1316 S1): the flow's role table comes from the helper role store. The flow-only keys, executor-t1 and
// (CRW-1434) critic, live in the same roles object as the four helper roles; Roles() (the CXC oracle port the GUI and the spawn
// hooks read) does not change. reviewer-final is a retired role key (CRW-1450).

const sixRoleStore = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "high", "promptOverride": "final"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"},
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"}}}`

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
	if !reflect.DeepEqual(order, FlowRoles()) || !reflect.DeepEqual(FlowRoles(), []RoleName{Architect, Executor, ExecutorT1, Reviewer, Explorer, Critic}) {
		t.Fatalf("order %v", order)
	}
	want := map[RoleName][3]string{
		Architect: {"anthropic/claude-opus-5-5", "xhigh", "store"}, Executor: {"anthropic/claude-haiku-5", "xhigh", "store"},
		ExecutorT1: {"anthropic/claude-sonnet-5-5", "high", "store"}, Reviewer: {"gpt-6.1-sol", "high", "store"},
		Explorer: {"anthropic/claude-haiku-5", "high", "store"},
		Critic:   {"gpt-6.1-sol", "xhigh", "store"},
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
	if text := string(must(json.Marshal(settings))); strings.Contains(text, "executor-t1") || strings.Contains(text, "reviewer-final") || strings.Contains(text, `"critic"`) {
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
	if got[ExecutorT1] != [3]string{"anthropic/claude-sonnet-5-5", "xhigh", "compiled_default"} {
		t.Fatalf("pairs %v", got)
	}
	if _, has := got[ReviewerFinal]; has {
		t.Fatalf("the retired key has a pair: %v", got)
	}
	if got[Critic] != [3]string{"gpt-6.1-sol", "xhigh", "compiled_default"} {
		t.Fatalf("critic %v", got[Critic])
	}
	if len(snap.Warnings) != 2 || !strings.Contains(snap.Warnings[0], "executor-t1") || !strings.Contains(snap.Warnings[1], "critic") || !strings.Contains(snap.Warnings[0], "compiled default") {
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
	if snap2.Present || len(snap2.Warnings) != 6 || flowPairs(snap2)[Critic][2] != "compiled_default" || flowPairs(snap2)[ExecutorT1][2] != "compiled_default" || flowPairs(snap2)[Explorer][2] != "undecided" {
		t.Fatalf("missing store: %+v", snap2)
	}
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatalf("reading the snapshot made the store directory: %v", err)
	}
}

func TestCRW1330_UnusableFlowKeyStopsOnlyItself(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {"executor": {"mode": "model", "model": "m-exec", "effort": "high"}, "executor-t1": {"mode": "weird"}}}`)
	snap := ReadFlowSnapshot(env)
	got := flowPairs(snap)
	if got[ExecutorT1] != [3]string{"anthropic/claude-sonnet-5-5", "xhigh", "compiled_default"} || got[Executor] != [3]string{"m-exec", "high", "store"} {
		t.Fatalf("pairs %v", got)
	}
	if joined := strings.Join(snap.Warnings, "\n"); !strings.Contains(joined, "executor-t1") || !strings.Contains(joined, "invalid mode") {
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
	// an entry of the retired key is not in the digest either (CRW-1450)
	writeStore(t, dir, strings.Replace(sixRoleStore, `"critic"`, `"reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"}, "critic"`, 1))
	if got := ReadFlowSnapshot(env).Digest(); got != base {
		t.Fatalf("an entry of the retired key changed the digest: %s != %s", got, base)
	}
	writeStore(t, dir, strings.Replace(sixRoleStore, `"effort": "high", "promptOverride": "final"},
  "reviewer"`, `"effort": "xhigh", "promptOverride": "final"},
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
	set := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"set", "critic", "--mode", "model", "--model", "gpt-7-sol", "--effort", "xhigh"}), env))
	if set["model"] != "gpt-7-sol" || set["effort"] != "xhigh" || set["mode"] != "model" || set["source"] != "store" {
		t.Fatalf("set: %v", set)
	}
	stored := readText(t, dir+"/"+StoreFile)
	var doc struct {
		Roles map[string]map[string]any `json:"roles"`
	}
	check(t, json.Unmarshal([]byte(stored), &doc))
	if doc.Roles["critic"]["model"] != "gpt-7-sol" {
		t.Fatalf("the store: %s", stored)
	}
	got := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"get", "critic"}), env))
	if got["model"] != "gpt-7-sol" || got["source"] != "store" {
		t.Fatalf("get: %v", got)
	}
	if p := flowPairs(ReadFlowSnapshot(env))[Critic]; p != [3]string{"gpt-7-sol", "xhigh", "store"} {
		t.Fatalf("the snapshot: %v", p)
	}
	// the list keeps the helper's four roles (the CXC answer)
	cfg := helperCLIDecode[Config](t, RunHelper(ParseHelperArgs(nil), env))
	if len(cfg.Roles) != 4 {
		t.Fatalf("list %+v", cfg)
	}
	reset := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"reset", "critic"}), env))
	if reset["source"] != "compiled_default" || reset["model"] != "gpt-6.1-sol" {
		t.Fatalf("reset: %v", reset)
	}
	if strings.Contains(readText(t, dir+"/"+StoreFile), "critic") {
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

// CRW-1434 (bundle CRW-1424, D-1, D-2): the design critic is a flow-only key. A store written before it, with the five
// other members, runs the critic on its compiled default (the final reviewer's pair, a model family other than the
// architect's) and warns once, naming the role and the reset that brings the default back; the critic is the last flow role.
func TestFlowSnapshotWithoutACriticEntryUsesTheCompiledDefaultAndWarns(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "high"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`)
	snap := ReadFlowSnapshot(env)
	if got := flowPairs(snap)[Critic]; got != [3]string{"gpt-6.1-sol", "xhigh", "compiled_default"} {
		t.Fatalf("critic: %v", got)
	}
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], "critic") || !strings.Contains(snap.Warnings[0], "crw role helper reset critic") || !strings.Contains(snap.Warnings[0], "compiled default") {
		t.Fatalf("warnings %q", snap.Warnings)
	}
	roles := FlowRoles()
	if len(roles) != 6 || roles[len(roles)-1] != Critic || !reflect.DeepEqual(FlowOnlyRoles(), []RoleName{ExecutorT1, Critic}) {
		t.Fatalf("FlowRoles %v, FlowOnlyRoles %v", roles, FlowOnlyRoles())
	}
	// the helper addresses the key by name and shows what the flow uses; the oracle's list does not change
	view := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"get", "critic"}), env))
	if view["model"] != "gpt-6.1-sol" || view["effort"] != "xhigh" || view["source"] != "compiled_default" {
		t.Fatalf("get critic: %v", view)
	}
	if len(Roles()) != 4 {
		t.Fatalf("Roles() changed: %v", Roles())
	}
}

// CRW-1450: reviewer-final is a retired role key. The helper refuses get and set of it with the notice, and so does the store's
// write path for any writer; reset removes an entry an old store still holds, after which the snapshot no longer names it.
func TestHelperRefusesTheRetiredKeyAndResetRemovesIt(t *testing.T) {
	const notice = "reviewer-final is a retired role key and is ignored"
	env, dir := home(t)
	path := writeStore(t, dir, `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh", "promptOverride": "final"},
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"}}}`)
	for _, args := range [][]string{{"get", "reviewer-final"}, {"set", "reviewer-final", "--mode", "model", "--model", "gpt-7-sol", "--effort", "xhigh"}} {
		if r := RunHelper(ParseHelperArgs(args), env); r.Code != 1 || !strings.Contains(r.Output, notice) {
			t.Errorf("%v: %+v, want a refusal with the notice", args, r)
		}
	}
	if _, err := SetRole(env, ReviewerFinal, RolePatch{Effort: Some(EffortName("high"))}); err == nil || !strings.Contains(err.Error(), notice) {
		t.Errorf("SetRole of the retired key: %v, want a refusal with the notice", err)
	}
	if got := readText(t, path); !strings.Contains(got, `"promptOverride": "final"`) {
		t.Fatalf("a refused write changed the store:\n%s", got)
	}
	snap := ReadFlowSnapshot(env)
	if joined := strings.Join(snap.Warnings, "\n"); strings.Count(joined, notice) != 1 || !strings.Contains(joined, path) || !strings.Contains(joined, "crw role helper reset reviewer-final") {
		t.Fatalf("the snapshot of a store holding the retired key: %q", snap.Warnings)
	}
	for _, r := range snap.Roles {
		if r.Role == ReviewerFinal {
			t.Errorf("the snapshot has a reviewer-final role: %+v", r)
		}
	}
	if r := RunHelper(ParseHelperArgs([]string{"reset", "reviewer-final"}), env); r.Code != 0 || !strings.Contains(r.Output, notice) {
		t.Fatalf("reset of the retired key: %+v", r)
	}
	if got := readText(t, path); strings.Contains(got, "reviewer-final") || !strings.Contains(got, `"critic"`) {
		t.Fatalf("reset left the retired entry or removed another:\n%s", got)
	}
	after := ReadFlowSnapshot(env)
	if strings.Contains(strings.Join(after.Warnings, "\n"), notice) || after.Digest() != snap.Digest() {
		t.Fatalf("after the reset: warnings %q, digest %s (was %s)", after.Warnings, after.Digest(), snap.Digest())
	}
	// a reset of a store that holds no entry for it is a no-op answer, not an error
	if r := RunHelper(ParseHelperArgs([]string{"reset", "reviewer-final"}), env); r.Code != 0 {
		t.Fatalf("a second reset: %+v", r)
	}
}
