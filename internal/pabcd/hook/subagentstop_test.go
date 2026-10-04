package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Gate cases from CXC v0.2.40 subagent-evidence.test.ts:52-295,355-408,586-612,700-785,818-971.
// Exercise the registered leg so the tests fail by assertion before its handler is ported.
func subagentStopWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv("CRW_PABCD", "")
	return t.TempDir()
}

func subagentStopPut(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func subagentStopSeed(t *testing.T, cwd string, phase state.Phase, active bool) {
	t.Helper()
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive = phase, active
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

func subagentStopRun(t *testing.T, cwd, role, agent, turn, message string, extra map[string]any) string {
	t.Helper()
	p := map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": "s1", "agent_type": role,
		"agent_id": agent, "turn_id": turn, "last_assistant_message": message}
	for key, value := range extra {
		p[key] = value
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"},
		strings.NewReader(string(raw)), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	return out.String()
}

func subagentStopBlock(t *testing.T, out string, attempt int) {
	t.Helper()
	raw, err := os.ReadFile("../evidence/testdata/directives.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ Verifier []string }
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(golden.Verifier[attempt-1])
	want := `{"decision":"block","reason":` + strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(quoted)) + `}`
	if out != want {
		t.Fatalf("attempt %d: got %q, want %q", attempt, out, want)
	}
}

func TestSubagentStopRolesAndWorkerArming(t *testing.T) {
	for _, role := range []string{"explorer", "default", "", "worker"} {
		t.Run("unarmed_"+role, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			if got := subagentStopRun(t, cwd, role, "a1", "", "", nil); got != "" {
				t.Fatal(got)
			}
			if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
				t.Fatalf("unexpected state: %v", err)
			}
		})
	}
	for _, phase := range []state.Phase{state.PhaseIdle, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC} {
		for _, active := range []bool{false, true} {
			t.Run(string(phase)+map[bool]string{false: "_inactive", true: "_active"}[active], func(t *testing.T) {
				cwd := subagentStopWorkspace(t)
				subagentStopSeed(t, cwd, phase, active)
				got := subagentStopRun(t, cwd, "worker", "a1", "", "", nil)
				if active && (phase == state.PhaseB || phase == state.PhaseC) {
					subagentStopBlock(t, got, 1)
				} else if got != "" {
					t.Fatal(got)
				}
			})
		}
	}
	cwd := subagentStopWorkspace(t)
	path := state.StatePath(cwd, "s1")
	subagentStopPut(t, path, "{ corrupt")
	if out := subagentStopRun(t, cwd, "worker", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{ corrupt" {
		t.Fatal("worker changed corrupt state")
	}
}

func TestSubagentStopPolicyAndExecutor(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, "crw.json"), `{"pabcd":{"enabled":false}}`)
	subagentStopSeed(t, cwd, state.PhaseB, true)
	for _, override := range []string{"", "off", "on"} {
		t.Setenv("CRW_PABCD", override)
		for _, role := range []string{"executor", "worker"} {
			out := subagentStopRun(t, cwd, role, role, "", "", nil)
			if override == "on" {
				subagentStopBlock(t, out, 1)
			} else if out != "" {
				t.Fatal(out)
			}
		}
	}
	fresh := t.TempDir()
	t.Setenv("CRW_PABCD", "")
	subagentStopBlock(t, subagentStopRun(t, fresh, "executor", "a1", "", "", nil), 1)
}

func TestSubagentStopBudgetAndLateReceipt(t *testing.T) {
	for _, role := range []string{"executor", "worker"} {
		t.Run(role, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			subagentStopSeed(t, cwd, state.PhaseB, true)
			for n := 1; n <= 3; n++ {
				subagentStopBlock(t, subagentStopRun(t, cwd, role, "a1", "t1", "private prose", nil), n)
			}
			for n := 0; n < 4; n++ {
				if out := subagentStopRun(t, cwd, role, "a1", "t1", "", nil); out != "" {
					t.Fatal(out)
				}
			}
			s := state.ReadState(cwd, "s1")
			if len(s.UnverifiedSubagents) != 1 || s.UnverifiedSubagents[0].AgentType != role || !s.UnverifiedSubagents[0].Resolvable {
				t.Fatalf("verdicts: %+v", s.UnverifiedSubagents)
			}
			if evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 3 {
				t.Fatal("terminal budget lost")
			}
			raw, _ := os.ReadFile(state.StatePath(cwd, "s1"))
			if bytes.Contains(raw, []byte("private prose")) {
				t.Fatal("prose persisted")
			}
			subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/late.md"), "verified")
			if out := subagentStopRun(t, cwd, role, "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/late.md", nil); out != "" {
				t.Fatal(out)
			}
			if len(state.ReadState(cwd, "s1").UnverifiedSubagents) != 0 || evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 0 {
				t.Fatal("late receipt did not resolve")
			}
		})
	}
}

func TestSubagentStopReceiptValidationAndTranscriptSpoofing(t *testing.T) {
	for _, kind := range []string{"outside", "empty", "symlink", "linked_directory", "valid", "context", "exempt", "buried", "readonly"} {
		t.Run(kind, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			subagentStopSeed(t, cwd, state.PhaseB, true)
			receipt := filepath.Join(cwd, ".crw/evidence/proof.md")
			subagentStopPut(t, receipt, "proof")
			message := "EVIDENCE_RECORDED: " + receipt
			extra := map[string]any{}
			switch kind {
			case "outside":
				receipt = filepath.Join(cwd, "outside.md")
				subagentStopPut(t, receipt, "proof")
				message = "EVIDENCE_RECORDED: " + receipt
			case "empty":
				subagentStopPut(t, receipt, "")
			case "symlink":
				link := filepath.Join(cwd, ".crw/evidence/link")
				if err := os.Symlink(receipt, link); err != nil {
					t.Fatal(err)
				}
				message = "EVIDENCE_RECORDED: " + link
			case "linked_directory":
				outside := t.TempDir()
				subagentStopPut(t, filepath.Join(outside, "proof"), "proof")
				if err := os.Symlink(outside, filepath.Join(cwd, ".crw/evidence/link")); err != nil {
					t.Fatal(err)
				}
				message = "EVIDENCE_RECORDED: .crw/evidence/link/proof"
			case "context", "exempt", "buried", "readonly":
				text := map[string]string{"context": "Context compacted", "exempt": "[CXC-EVIDENCE-EXEMPT]", "buried": strings.Repeat("x", 30000) + "[CXC-EVIDENCE-EXEMPT]", "readonly": "[REVIEWER read-only]"}[kind]
				transcript := filepath.Join(cwd, "child.jsonl")
				subagentStopPut(t, transcript, text)
				extra["agent_transcript_path"] = transcript
				message = text
			}
			out := subagentStopRun(t, cwd, "worker", "a1", "", ""+message, extra)
			if kind == "valid" {
				if out != "" {
					t.Fatal(out)
				}
			} else {
				subagentStopBlock(t, out, 1)
			}
		})
	}
}

func TestSubagentStopIdentitiesAndNilFields(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	for _, id := range [][2]string{{"a-b", "c"}, {"a", "b-c"}, {"a/b", ""}, {"a-b", ""}, {"a1", "t1"}, {"a1", "t2"}, {"a1", ""}} {
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, subagentStopRun(t, cwd, "executor", id[0], id[1], "", nil), n)
		}
		if out := subagentStopRun(t, cwd, "executor", id[0], id[1], "", nil); out != "" {
			t.Fatal(out)
		}
	}
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/one.md"), "verified")
	subagentStopRun(t, cwd, "executor", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/one.md", nil)
	if evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 0 || evidence.ReadAttempts(cwd, "s1", "a1", "t2") != 3 || evidence.ReadAttempts(cwd, "s1", "a1", "") != 3 {
		t.Fatal("receipt cleared other turns")
	}
	fresh := t.TempDir()
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, subagentStopRun(t, fresh, "executor", "", "", "", map[string]any{"agent_id": nil, "turn_id": nil, "last_assistant_message": nil}), n)
	}
	subagentStopRun(t, fresh, "executor", "", "", "", nil)
	if entries := state.ReadState(fresh, "s1").UnverifiedSubagents; len(entries) != 1 || entries[0].Resolvable {
		t.Fatalf("nil identity: %+v", entries)
	}
}

func TestSubagentStopPersistenceFailuresAndCorruptCounter(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence-attempts"), "not a directory")
	if out := subagentStopRun(t, cwd, "executor", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	entries := state.ReadState(cwd, "s1").UnverifiedSubagents
	if len(entries) != 1 || entries[0].Attempts != 0 {
		t.Fatalf("failed write must record old count: %+v", entries)
	}
	for _, bad := range []string{"-1", "2.5", "999999", `"three"`} {
		fresh := t.TempDir()
		subagentStopRun(t, fresh, "executor", "a1", "", "", nil)
		dir := filepath.Join(fresh, ".crw/evidence-attempts")
		names, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name.Name(), ".json") {
				subagentStopPut(t, filepath.Join(dir, name.Name()), `{"attempts":`+bad+`}`)
			}
		}
		if out := subagentStopRun(t, fresh, "executor", "a1", "", "", nil); out != "" {
			t.Fatal(out)
		}
		if len(state.ReadState(fresh, "s1").UnverifiedSubagents) != 1 {
			t.Fatal("corrupt counter did not terminate")
		}
	}
	fresh := t.TempDir()
	for n := 0; n < 3; n++ {
		subagentStopRun(t, fresh, "executor", "a1", "", "", nil)
	}
	path := state.StatePath(fresh, "s1")
	subagentStopPut(t, path, "{ corrupt")
	if out := subagentStopRun(t, fresh, "executor", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{ corrupt" {
		t.Fatal("unreadable state overwritten")
	}
	if verdict := evidence.UnrecordableVerdictStatus(fresh, "s1"); !verdict.Present {
		t.Fatalf("missing marker: %+v", verdict)
	}
}

func TestSubagentStopConcurrentTerminalVerdicts(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	for _, agent := range []string{"racer-a", "racer-b"} {
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, subagentStopRun(t, cwd, "executor", agent, "", "", nil), n)
		}
	}
	var wg sync.WaitGroup
	for _, agent := range []string{"racer-a", "racer-b"} {
		wg.Go(func() { subagentStopRun(t, cwd, "executor", agent, "", "", nil) })
	}
	wg.Wait()
	if entries := state.ReadState(cwd, "s1").UnverifiedSubagents; len(entries) != 2 {
		t.Fatalf("lost verdict: %+v", entries)
	}
}
