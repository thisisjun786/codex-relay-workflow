// prompt_submit_legs_crw1090_test.go drives the S4 reproduction of CRW-1090 through the wired leg, as
// `crw hook user-prompt-submit --leg user-prompt-submit-checking-pabcd-trigger` runs it.
package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// TestCRW1090S4CompactionReinjectsThePlanDirective is end condition 1: the S4 reproduction (state-before-r5.json, whose
// lastInjectedPhase PostCompact reset, and the rollout as it stood at that UserPromptSubmit) through the wired leg prints
// the full [crw: PLAN] directive and records lastInjectedPhase P. The stage header the compaction removed still sits in
// the compacted record's guardian history inside the 64 KiB tail, which the oracle read as an injected directive.
func TestCRW1090S4CompactionReinjectsThePlanDirective(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "repo")
	const sid, turn = "01a1226a-1810-7731-b382-0fb87228ac8e", "01a1226d-3408-7951-87e9-927278386789"
	before, err := os.ReadFile("testdata/crw1090/state-before-r5.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(state.StatePath(cwd, sid)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(cwd, sid), before, 0o644); err != nil {
		t.Fatal(err)
	}
	transcript, err := filepath.Abs("testdata/crw1090/transcript-r5-at-ups.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_BIN", "{CRW}")
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex-home"))
	raw, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sid, "turn_id": turn, "cwd": cwd,
		"transcript_path": transcript, "prompt": "Reply with one short sentence: what is the first word in notes.txt? No commands."})
	if err != nil {
		t.Fatal(err)
	}
	answer := promptSubmitLeg(t).Handle(harness.Call{Raw: string(raw), PabcdEnabled: true})
	want := hook.WithFooter(hook.PhaseDirective(state.PhaseP, nil), state.PhaseP)
	if got := answerContext(t, answer, "UserPromptSubmit"); got != want {
		t.Errorf("the leg answered\n%q\nwant the full PLAN directive\n%q", got, want)
	}
	s := state.ReadState(cwd, sid)
	if s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseP || !slices.Contains(s.InjectedTurns, turn) {
		t.Errorf("the injection was not recorded: lastInjectedPhase %v, turns %v", s.LastInjectedPhase, s.InjectedTurns)
	}
}
