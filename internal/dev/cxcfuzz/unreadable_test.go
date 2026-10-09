//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// readingTarget is a campaign subject whose inputs are numbered 1 to n: Reading calls an input unreadable when the number is in
// unreadable, and calls the Go answer a refusal when the number is in refused.
func readingTarget(t *testing.T, unreadable, refused map[int]bool) Target {
	t.Helper()
	count := 0
	target := helperTarget(t, func(rng *rand.Rand, size int) any {
		count++
		return pyjson.Object{{Key: "n", Value: count}}
	})
	target.Reading = func(input any, env Env, goOut any) (bool, bool) {
		n, _ := field(input, "n")
		number := 0
		switch v := n.(type) {
		case int:
			number = v
		case int64:
			number = int(v)
		case float64:
			number = int(v)
		case json.Number:
			parsed, err := strconv.Atoi(v.String())
			if err != nil {
				t.Errorf("input n = %q", v)
			}
			number = parsed
		default:
			t.Errorf("input n has type %T", n)
		}
		return unreadable[number], refused[number]
	}
	return target
}

// TestCampaignCountsUnreadableCasesAndTheRefusedOnes: the summary counts the cases the reader could not read, the ones among them
// the Go side refused, and the ones it did not; the field Refused (a scenario the harness declined) is not touched.
func TestCampaignCountsUnreadableCasesAndTheRefusedOnes(t *testing.T) {
	target := readingTarget(t, map[int]bool{1: true, 3: true, 4: true, 6: true}, map[int]bool{1: true, 3: true, 5: true})
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 8, Seed: 3, Workers: 1, Out: out, Env: helperEnvFor()})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cases != 8 || summary.UnreadableCases != 4 || summary.UnreadableRefused != 2 || summary.UnreadableNotRefused != 2 {
		t.Fatalf("summary %+v, want 8 cases, 4 unreadable, 2 refused, 2 not refused", summary)
	}
	if summary.Refused != 0 {
		t.Fatalf("the scenario Refused field = %d, want 0: the unreadable counts are separate fields", summary.Refused)
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"unreadableCases": 4`, `"unreadableRefused": 2`, `"unreadableNotRefused": 2`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("summary.json lacks %s:\n%s", key, raw)
		}
	}
}

// TestCampaignWithoutAReadingCountsNoUnreadableCases: a target that does not read commands (echo, doctor) counts nothing.
func TestCampaignWithoutAReadingCountsNoUnreadableCases(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return "x" })
	summary, err := Campaign(Config{Target: target, Cases: 3, Seed: 1, Workers: 1, Out: t.TempDir(), Env: helperEnvFor()})
	if err != nil {
		t.Fatal(err)
	}
	if summary.UnreadableCases != 0 || summary.UnreadableRefused != 0 || summary.UnreadableNotRefused != 0 {
		t.Fatalf("summary %+v", summary)
	}
}

// TestCampaignFailsWhenAnUnreadableCaseWasNotRefused: every case agrees with the oracle, yet the run is a failure because one
// case the reader could not read was not refused; a run whose unreadable cases were all refused succeeds.
func TestCampaignFailsWhenAnUnreadableCaseWasNotRefused(t *testing.T) {
	failing := campaignFailed(Summary{Cases: 3, Same: 3, UnreadableCases: 2, UnreadableRefused: 1, UnreadableNotRefused: 1})
	if !failing {
		t.Error("an unreadable case that was not refused did not fail the run")
	}
	if campaignFailed(Summary{Cases: 3, Same: 3, UnreadableCases: 2, UnreadableRefused: 2}) {
		t.Error("a run whose unreadable cases were all refused failed")
	}
	if !campaignFailed(Summary{Cases: 3, Same: 2, Differ: 1}) {
		t.Error("a divergence no longer fails the run")
	}
	if !campaignFailed(Summary{Cases: 1, Timeouts: 1}) || !campaignFailed(Summary{Cases: 1, Refused: 1}) {
		t.Error("a timeout or a refused scenario no longer fails the run")
	}
	if campaignFailed(Summary{Cases: 3, Same: 3}) {
		t.Error("an all-agreeing run failed")
	}
}

// The three command-gate targets measure criterion c2g: an input whose command the shared reader cannot read is unreadable, and the
// Go answer for it must be a refusal. Each target is driven with a readable and an unreadable command, with the real Go function
// and with an answer that lets the unreadable one through.
func TestShellWriteReadingCountsAnUnreadableCommand(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	env := RootEnv(root)
	for _, c := range []struct {
		command             string
		unreadable, refused bool
	}{
		{"$CMD x", true, true},
		{"eval \"$(x)\"", true, true},
		{"echo hi > a.txt", false, false},
		{"rm -rf x", false, false},
	} {
		input := pyjson.Object{{Key: "command", Value: c.command}}
		goOut, err := shellWriteGo(input, env)
		if err != nil {
			t.Fatal(err)
		}
		unreadable, refused := shellWriteReading(input, env, goOut)
		if unreadable != c.unreadable || refused != c.refused {
			t.Errorf("%q: unreadable=%v refused=%v, want %v %v", c.command, unreadable, refused, c.unreadable, c.refused)
		}
	}
	if unreadable, refused := shellWriteReading(pyjson.Object{{Key: "command", Value: "$CMD x"}}, env, []any{}); !unreadable || refused {
		t.Errorf("an unreadable command answered with no destination: unreadable=%v refused=%v, want true false", unreadable, refused)
	}
}

func TestMemoryGateReadingCountsAnUnreadableCommand(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	env := RootEnv(root)
	payload := func(tool, event, command string) pyjson.Object {
		return pyjson.Object{{Key: "payload", Value: pyjson.Object{
			{Key: "hook_event_name", Value: event}, {Key: "session_id", Value: "s1"}, {Key: "turn_id", Value: "t1"},
			{Key: "cwd", Value: env.Root}, {Key: "tool_name", Value: tool},
			{Key: "tool_input", Value: pyjson.Object{{Key: "command", Value: command}}},
		}}}
	}
	for _, c := range []struct {
		name                string
		input               pyjson.Object
		unreadable, refused bool
	}{
		{"an unreadable Bash command", payload("Bash", "PreToolUse", "$CMD x"), true, true},
		{"an unreadable exec_command", payload("exec_command", "PreToolUse", "eval \"$(x)\""), true, true},
		{"a readable Bash command", payload("Bash", "PreToolUse", "echo hi"), false, false},
		{"an unreadable command of a tool that is no shell", payload("Write", "PreToolUse", "$CMD x"), false, false},
		{"an unreadable command in another event", payload("Bash", "PostToolUse", "$CMD x"), false, false},
	} {
		goOut, err := memoryGateGo(c.input, env)
		if err != nil {
			t.Fatal(err)
		}
		unreadable, refused := memoryGateReading(c.input, env, goOut)
		if unreadable != c.unreadable || refused != c.refused {
			t.Errorf("%s: unreadable=%v refused=%v, want %v %v", c.name, unreadable, refused, c.unreadable, c.refused)
		}
	}
	if unreadable, refused := memoryGateReading(payload("Bash", "PreToolUse", "$CMD x"), env, ""); !unreadable || refused {
		t.Errorf("an unreadable command the gate allowed: unreadable=%v refused=%v, want true false", unreadable, refused)
	}
}

func TestWorktreeDelReadingCountsAnUnreadableCommand(t *testing.T) {
	for _, c := range []struct {
		command             string
		unreadable, refused bool
	}{
		{"$CMD x", true, true},
		{"eval \"$(x)\"", true, true},
		{"git status", false, false},
		{"rm -rf /tmp/unrelated", false, false},
	} {
		root := t.TempDir()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		input := worktreeDelCaseInput(c.command)
		if _, err := Scenarios(root, input); err != nil {
			t.Fatal(err)
		}
		env := RootEnv(root)
		goOut, err := worktreeDelGo(input, env)
		if err != nil {
			t.Fatal(err)
		}
		unreadable, refused := worktreeDelReading(input, env, goOut)
		if unreadable != c.unreadable || refused != c.refused {
			t.Errorf("%q: unreadable=%v refused=%v, want %v %v", c.command, unreadable, refused, c.unreadable, c.refused)
		}
		if c.unreadable {
			if unreadable, refused := worktreeDelReading(input, env, worktreeDelAllow()); !unreadable || refused {
				t.Errorf("%q answered allow: unreadable=%v refused=%v, want true false", c.command, unreadable, refused)
			}
		}
	}
	// Outside a managed checkout the guard judges nothing, so an unreadable command is no case of the measure.
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput("$CMD x").Set("env", pyjson.Object{{Key: "CODEX_HOME", Value: "home/elsewhere/codex"}})
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	env := RootEnv(root)
	goOut, err := worktreeDelGo(input, env)
	if err != nil {
		t.Fatal(err)
	}
	if unreadable, _ := worktreeDelReading(input, env, goOut); unreadable {
		t.Error("an unreadable command outside a managed checkout was counted")
	}
}

// TestReadingsReadTheCommandAsTheGuardDoes: each measure reads a command the way its gate does. The worktree guard reads with no
// environment (an unset variable is unknown), so $HOME/tool, a program the case environment would name, is unreadable to it and
// counted; the case environment only decides whether the cwd is a managed checkout. The memory gate refuses a command that either
// of its readings (with the environment, and without one) cannot read. A relative cd and an argument word that is a variable stay
// readable (CRW-1028 verifier round 3, finding 5).
func TestReadingsReadTheCommandAsTheGuardDoes(t *testing.T) {
	for _, c := range []struct {
		command             string
		unreadable, refused bool
	}{
		{"$HOME/tool", true, true},
		{"$CRW_HOME/bin/tool --flag", true, true},
		{"\"$TMPDIR\"/tool", true, true},
		{"cd deep && $HOME/tool", true, true},
		{"cd deep && git status", false, false},
		{"cd ../other && ls", false, false},
		{"echo \"$HOME\"", false, false},
	} {
		root := t.TempDir()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		input := worktreeDelCaseInput(c.command)
		if _, err := Scenarios(root, input); err != nil {
			t.Fatal(err)
		}
		env := RootEnv(root)
		goOut, err := worktreeDelGo(input, env)
		if err != nil {
			t.Fatal(err)
		}
		unreadable, refused := worktreeDelReading(input, env, goOut)
		if unreadable != c.unreadable || refused != c.refused {
			t.Errorf("worktreedel %q: unreadable=%v refused=%v, want %v %v", c.command, unreadable, refused, c.unreadable, c.refused)
		}
	}
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	env := RootEnv(root)
	for _, c := range []struct {
		command             string
		unreadable, refused bool
	}{
		{"$HOME/tool", true, true},
		{"$CODEX_HOME/bin/tool", true, true},
		{"cd sub && $HOME/tool", true, true},
		{"cd sub && ls", false, false},
		{"echo \"$HOME\"", false, false},
	} {
		input := pyjson.Object{{Key: "payload", Value: pyjson.Object{
			{Key: "hook_event_name", Value: "PreToolUse"}, {Key: "session_id", Value: "s1"}, {Key: "turn_id", Value: "t1"},
			{Key: "cwd", Value: env.Root}, {Key: "tool_name", Value: "Bash"},
			{Key: "tool_input", Value: pyjson.Object{{Key: "command", Value: c.command}}},
		}}}
		goOut, err := memoryGateGo(input, env)
		if err != nil {
			t.Fatal(err)
		}
		unreadable, refused := memoryGateReading(input, env, goOut)
		if unreadable != c.unreadable || refused != c.refused {
			t.Errorf("memorygate %q: unreadable=%v refused=%v, want %v %v", c.command, unreadable, refused, c.unreadable, c.refused)
		}
	}
}

// The registered command-gate targets carry the measure; the others do not.
func TestOnlyTheCommandGateTargetsCarryAReading(t *testing.T) {
	for _, name := range []string{"shellwrite", "memorygate", "worktreedel"} {
		target, ok := Lookup(name)
		if !ok || target.Reading == nil {
			t.Errorf("target %s has no Reading", name)
		}
	}
	for _, name := range []string{"echo", "doctor", "spawn"} {
		if target, ok := Lookup(name); ok && target.Reading != nil {
			t.Errorf("target %s carries a Reading it does not need", name)
		}
	}
}
