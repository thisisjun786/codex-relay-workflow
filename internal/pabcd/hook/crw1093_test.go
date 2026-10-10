package hook

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestCRW1093PromptToProtectedWrite(t *testing.T) {
	cases := []struct {
		prompt string
		allow  bool
	}{
		{"Do not remember this: the task is read-only.", false},
		{"이건 기억하지 마", false},
		{"기억해 둬, 하지만 저장하지 마", false},
		{`"remember this"`, false},
		{"`remember this`", false},
		{"``remember this``", false},
		{`Example: "remember this`, false},
		{"‘remember this’", false},
		{"Do not ever remember this", false},
		{"Task packet:\nRead-only. remember this", false},
		{"````text\nremember this\n````", false},
		{"~~~\nremember this\n~~~", false},
		{"<task_packet>\nRead-only. remember this\n</task_packet>", false},
		{"## Task packet\nRead-only. remember this", false},
		{"> remember this", false},
		{"Explain how to remember this", false},
		{"Remember this but do not store it to memory", false},
		{"remember this", true},
		{"이거 기억해 둬", true},
		{"don't forget this", true},
		{"잊지 마", true},
		{`remember the following: "quoted content"`, true},
		{"- remember this", true},
		{"1. 이거 기억해 둬", true},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				cwd, root, env := gateScene(t)
				promptSubmitAnswer(t, cwd, gateSession, "t1", c.prompt, enabled)
				if got := state.ReadState(cwd, gateSession).MemoryWriteRequested; got != c.allow {
					t.Errorf("marker=%v want %v (enabled=%v)", got, c.allow, enabled)
				}
				raw := gatePayload(t, cwd, map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": root + "/note.md"}})
				out := HandleMemoryWriteGate(raw, env)
				if (out == "") != c.allow {
					t.Errorf("allow=%v want %v (enabled=%v)", out == "", c.allow, enabled)
				}
				gateDeny(t, HandleMemoryWriteGate(raw, env))
			}
		})
	}
}

func TestCRW1093ConcurrentAndNextTurn(t *testing.T) {
	cwd, _, env := gateScene(t)
	promptSubmitAnswer(t, cwd, gateSession, "t1", "remember this", false)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if HandleMemoryWriteGate(gatePayload(t, cwd, nil), env) == "" {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatalf("allowed %d writes", allowed.Load())
	}
	promptSubmitAnswer(t, cwd, gateSession, "t2", "remember this", false)
	next := gatePayload(t, cwd, map[string]any{"turn_id": "t3"})
	gateDeny(t, HandleMemoryWriteGate(next, env))
	if !strings.Contains(gateDeny(t, HandleMemoryWriteGate(next, env)), "MEMORY-WRITE-GATE") {
		t.Fatal("missing policy id")
	}
}
