//go:build dev

package laneparity

import (
	"encoding/json"
	"strings"
	"testing"
)

func baseline() (string, string, string, []Want, []Receipt) {
	const run, plugin, binary = "run-1", "plugin-digest-aaaaaaaaaaaa", "binary-digest-bbbbbbbbbbbb"
	payload := json.RawMessage(`{"session_id":"s1","turn_id":"t1","tool_use_id":"c1","tool_name":"spawn_agent","agent_id":"agent-7"}`)
	subject := SubjectOf(payload, "attach $crw:crw-run and $crw:crw-check, then $crw:crw-run again")
	wants := []Want{{Fixture: "f", Step: 0, Leg: "pre-tool-use-attaching-skills", Event: "PreToolUse", Subject: subject}}
	receipts := []Receipt{{
		Run: run, Plugin: plugin, Binary: binary, Fixture: "f", Step: 0, Leg: "pre-tool-use-attaching-skills", Event: "PreToolUse",
		Session: "s1", Turn: "t1", ToolUse: "c1", ToolName: "spawn_agent", Agent: "agent-7", Skills: []string{"crw:crw-check", "crw:crw-run"},
	}}
	return run, plugin, binary, wants, receipts
}

func TestSubjectOf_readsThePayloadAndTheSkillsTheAnswerNames(t *testing.T) {
	_, _, _, wants, _ := baseline()
	s := wants[0].Subject
	if s.Session != "s1" || s.Turn != "t1" || s.ToolUse != "c1" || s.ToolName != "spawn_agent" || s.Agent != "agent-7" {
		t.Errorf("subject %+v", s)
	}
	if strings.Join(s.Skills, ",") != "crw:crw-check,crw:crw-run" {
		t.Errorf("skills %v", s.Skills)
	}
	// A payload given as a JSON text, and a null agent.
	text, _ := json.Marshal(`{"session_id":"s2","agent_id":null}`)
	if got := SubjectOf(text, ""); got.Session != "s2" || got.Agent != "" {
		t.Errorf("text payload: %+v", got)
	}
}

func TestVerifyReceipts_acceptsTheReceiptOfThisRun(t *testing.T) {
	run, plugin, binary, wants, receipts := baseline()
	if problems := VerifyReceipts(run, plugin, binary, wants, receipts); len(problems) != 0 {
		t.Fatalf("problems: %q", problems)
	}
}

// A receipt that is wrong in one respect must not prove the firing.
func TestVerifyReceipts_rejectsAReceiptThatIsWrongInOneRespect(t *testing.T) {
	for _, c := range []struct {
		name    string
		mutate  func(rs []Receipt) []Receipt
		problem string
	}{
		{"an earlier run's receipt", func(rs []Receipt) []Receipt { rs[0].Run = "run-0"; return rs }, "receipt of run run-0"},
		{"another plugin with the same eventName", func(rs []Receipt) []Receipt { rs[0].Plugin = "other-plugin-digest"; return rs }, "another plugin root"},
		{"another build (head)", func(rs []Receipt) []Receipt { rs[0].Binary = "other-build"; return rs }, "another crw build"},
		{"another leg", func(rs []Receipt) []Receipt { rs[0].Leg = "pre-tool-use-guarding-goal-budget"; return rs }, "receipt is for leg"},
		{"wrong event", func(rs []Receipt) []Receipt { rs[0].Event = "Stop"; return rs }, "receipt is for event Stop"},
		{"wrong session", func(rs []Receipt) []Receipt { rs[0].Session = "s9"; return rs }, "session"},
		{"wrong turn", func(rs []Receipt) []Receipt { rs[0].Turn = "t9"; return rs }, "turn"},
		{"wrong tool call", func(rs []Receipt) []Receipt { rs[0].ToolUse = "c9"; return rs }, "tool call"},
		{"wrong agent", func(rs []Receipt) []Receipt { rs[0].Agent = "agent-8"; return rs }, "agent"},
		{"wrong skill", func(rs []Receipt) []Receipt { rs[0].Skills = []string{"crw:crw-run", "crw:crw-define"}; return rs }, "receipt skills"},
		{"a skill missing", func(rs []Receipt) []Receipt { rs[0].Skills = []string{"crw:crw-run"}; return rs }, "receipt skills"},
		{"no receipt (missing hook)", func(rs []Receipt) []Receipt { return nil }, "no receipt"},
		{"a receipt twice", func(rs []Receipt) []Receipt { return append(rs, rs[0]) }, "2 receipts for one firing"},
		{"a receipt nobody asked for", func(rs []Receipt) []Receipt { r := rs[0]; r.Step = 3; return append(rs, r) }, "no wanted firing explains"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, plugin, binary, wants, receipts := baseline()
			problems := VerifyReceipts(run, plugin, binary, wants, c.mutate(receipts))
			if len(problems) == 0 {
				t.Fatal("the receipts were accepted")
			}
			if !strings.Contains(strings.Join(problems, "\n"), c.problem) {
				t.Errorf("problems %q do not say %q", problems, c.problem)
			}
		})
	}
}

func TestStdoutDigest_isTheSha256OfTheText(t *testing.T) {
	if got := StdoutDigest(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("digest of the empty text is %s", got)
	}
}
