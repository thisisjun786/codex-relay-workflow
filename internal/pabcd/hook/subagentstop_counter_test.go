package hook_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1106: one counter reader for the SubagentStop gate and the goal-complete gate (E8), the retry budget reserved under a lock
// of the exact (session, agent, turn), and counters and markers owned by the exact session.

func counterStop(t *testing.T, cwd, session, agent, turn, message string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": session, "agent_type": "executor",
		"agent_id": agent, "turn_id": turn, "last_assistant_message": message})
	if err != nil {
		t.Fatal(err)
	}
	out, err := subagentStopInvoke(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// counterComplete is update_goal status complete through E8 for session: "" allows it.
func counterComplete(t *testing.T, cwd, session string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": session, "cwd": cwd, "tool_name": "update_goal",
		"tool_input": map[string]any{"status": "complete"}})
	if err != nil {
		t.Fatal(err)
	}
	return hook.GoalGateHandlePreToolUseFailClosed(string(raw), os.LookupEnv, false)
}

// counterFiles is every counter file under the attempts directory, at any depth.
func counterFiles(t *testing.T, cwd string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(filepath.Join(cwd, ".crw", evidence.AttemptsSubdir), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func isBlock(out string) bool { return strings.Contains(out, `"decision":"block"`) }

// 32 stops of one child that arrive together spend the budget once: at most three blocks, as three stops in a row do.
func TestSubagentStopCounterReservationIsSerialised(t *testing.T) {
	for _, mode := range []string{"sequential", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			const n = 32
			outs := make([]string, n)
			if mode == "sequential" {
				for i := range outs {
					outs[i] = counterStop(t, cwd, "s1", "a1", "t1", "")
				}
			} else {
				raw, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": "s1", "agent_type": "executor", "agent_id": "a1", "turn_id": "t1"})
				var ready, wg sync.WaitGroup
				start := make(chan struct{})
				ready.Add(n)
				for i := range outs {
					wg.Go(func() {
						ready.Done()
						<-start
						outs[i], _ = subagentStopInvoke(raw)
					})
				}
				ready.Wait()
				close(start)
				wg.Wait()
			}
			blocks := 0
			for _, out := range outs {
				if isBlock(out) {
					blocks++
				}
			}
			if blocks != evidence.MaxAttempts {
				t.Fatalf("%d blocks, want %d", blocks, evidence.MaxAttempts)
			}
			if entries := state.ReadState(cwd, "s1").UnverifiedSubagents; len(entries) != 1 {
				t.Fatalf("verdicts: %+v", entries)
			}
		})
	}
}

// A counter that cannot be read as a count is a terminal unresolved verdict: the child is released without a fresh budget, the
// bytes stay, E8 keeps refusing however often the child stops again, and only a late valid receipt clears both gates.
func TestSubagentStopCorruptCounterStaysTerminal(t *testing.T) {
	for _, bad := range []string{"truncated", "empty", "null", "string", "array", "unreadable"} {
		t.Run(bad, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			for n := 1; n <= 3; n++ {
				subagentStopBlock(t, counterStop(t, cwd, "s1", "a1", "t1", ""), n)
			}
			// The budget is still running (three blocks, no terminal stop yet): corrupt it now.
			files := counterFiles(t, cwd)
			if len(files) != 1 {
				t.Fatalf("counter files: %v", files)
			}
			text := map[string]string{"truncated": "{", "empty": "", "null": "null", "string": `"three"`, "array": "[]", "unreadable": `{"attempts":1}`}[bad]
			subagentStopPut(t, files[0], text)
			if bad == "unreadable" {
				if os.Geteuid() == 0 {
					t.Skip("an unreadable file needs a non-root process")
				}
				if err := os.Chmod(files[0], 0); err != nil {
					t.Fatal(err)
				}
			}
			if counterComplete(t, cwd, "s1") == "" {
				t.Fatal("E8 allowed completion over a corrupt counter")
			}
			for i := 0; i < 3; i++ {
				if out := counterStop(t, cwd, "s1", "a1", "t1", ""); out != "" {
					t.Fatalf("stop %d after corruption: %s", i, out)
				}
				if counterComplete(t, cwd, "s1") == "" {
					t.Fatalf("E8 allowed completion after stop %d without a receipt", i)
				}
			}
			if bad != "unreadable" {
				if raw, _ := os.ReadFile(files[0]); string(raw) != text {
					t.Fatalf("the corrupt counter was overwritten: %q", raw)
				}
			}
			if entries := state.ReadState(cwd, "s1").UnverifiedSubagents; len(entries) != 1 || entries[0].AgentID != "a1" || entries[0].TurnID != "t1" {
				t.Fatalf("no terminal verdict: %+v", entries)
			}
			subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/late.md"), "verified late")
			if out := counterStop(t, cwd, "s1", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/late.md"); out != "" {
				t.Fatal(out)
			}
			if got := counterComplete(t, cwd, "s1"); got != "" {
				t.Fatalf("a late valid receipt did not clear E8: %s", got)
			}
		})
	}
}

// A missing counter starts at attempt 1. Sessions own their counters and markers exactly: short-x's spent budget and marker do not
// refuse short's completion. CRW-1108 releases a child with a non-canonical session without writing any alias counter,
// and the parent keeps refusing that session even after a receipt.
func TestSubagentStopCounterSessionOwnership(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, counterStop(t, cwd, "short-x", "a1", "", ""), n)
	}
	if got := counterComplete(t, cwd, "short-x"); got == "" {
		t.Fatal("short-x's own spent budget did not refuse it")
	}
	if got := counterComplete(t, cwd, "short"); got != "" {
		t.Fatalf("short-x's budget refused short: %s", got)
	}
	if err := evidence.WriteUnrecordableMarker(cwd, "s1-x", "a1"); err != nil {
		t.Fatal(err)
	}
	if got := counterComplete(t, cwd, "s1"); got != "" {
		t.Fatalf("s1-x's marker refused s1: %s", got)
	}
	if got := counterComplete(t, cwd, "s1-x"); got == "" {
		t.Fatal("s1-x's own marker did not refuse it")
	}

	fresh := subagentStopWorkspace(t)
	for range 2 {
		if out := counterStop(t, fresh, "a/b", "a1", "t1", ""); out != "" {
			t.Fatalf("non-canonical session blocked a child: %s", out)
		}
	}
	subagentStopBlock(t, counterStop(t, fresh, "a-b", "a1", "t1", ""), 1)
	subagentStopPut(t, filepath.Join(fresh, ".crw/evidence/ok.md"), "ok")
	if out := counterStop(t, fresh, "a-b", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/ok.md"); out != "" {
		t.Fatal(out)
	}
	if got := evidence.ReadAttempts(fresh, "a/b", "a1", "t1"); got != evidence.MaxAttempts {
		t.Fatalf("a non-canonical session read a counter: %d", got)
	}
	if got := counterComplete(t, fresh, "a/b"); got == "" {
		t.Fatal("a non-canonical session passed completion")
	}
}

// A legacy counter (the oracle's file name, from before CRW-1106) whose owner cannot be told keeps refusing every session it may
// belong to; one this tuple names exactly is carried into the new record and then removed.
func TestSubagentStopLegacyCounters(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	legacy := filepath.Join(cwd, ".crw", evidence.AttemptsSubdir, "short-x-a1-00000000000000000000000000000000.json")
	subagentStopPut(t, legacy, "{")
	if got := counterComplete(t, cwd, "short"); got == "" {
		t.Fatal("an unclassifiable legacy counter stopped refusing")
	}
	if raw, err := os.ReadFile(legacy); err != nil || string(raw) != "{" {
		t.Fatal("the legacy counter was not kept")
	}
}

// CRW-1106 post-evaluation round (d2): a stop that cannot get its tuple's lock mutates nothing under that tuple. Its receipt is
// valid, so the child is released, but the counter it would have cleared is left for the holder and for the receipt-carrying
// resolve (`crw pabcd evidence resolve`), which both run under the lock.
func TestSubagentStopLockTimeoutLeavesTheCounterAlone(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	for n := 1; n <= 2; n++ {
		subagentStopBlock(t, counterStop(t, cwd, "s1", "a1", "t1", ""), n)
	}
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/ok.md"), "verified")
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- evidence.WithCounterLock(cwd, "s1", "a1", "t1", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	out := counterStop(t, cwd, "s1", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/ok.md")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("a valid receipt was blocked: %s", out)
	}
	if got := evidence.ReadAttempts(cwd, "s1", "a1", "t1"); got != 2 {
		t.Fatalf("a stop without the lock changed the counter: attempts %d, want 2", got)
	}
	// Under the lock the same receipt clears it.
	if out := counterStop(t, cwd, "s1", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/ok.md"); out != "" {
		t.Fatal(out)
	}
	if got := evidence.ReadCounter(cwd, "s1", "a1", "t1"); got.State != evidence.CounterMissing {
		t.Fatalf("a stop under the lock left the counter: %+v", got)
	}
}

// CRW-1108 supersedes non-canonical-session migration: an old alias counter stays intact, and a receipt does not make a
// non-canonical session pass completion. The canonical twin still owns its budget.
func TestSubagentStopLegacyCounterOfANonCanonicalSession(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	// The oracle's name of the tuple, shared by a/b and a-b: the file a canonical a-b writes is byte for byte the one a/b's old
	// version wrote.
	if !evidence.WriteAttempts(cwd, "a-b", "x", 2, "t") {
		t.Fatal("not written")
	}
	files := counterFiles(t, cwd)
	if len(files) != 1 {
		t.Fatalf("the flat counter: %v", files)
	}
	legacy := files[0]
	if out := counterStop(t, cwd, "a/b", "x", "t", ""); out != "" {
		t.Fatalf("the terminal stop blocked: %s", out)
	}
	if counterComplete(t, cwd, "a/b") == "" {
		t.Fatal("completion allowed over an exhausted budget")
	}
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/ok.md"), "verified")
	if out := counterStop(t, cwd, "a/b", "x", "t", "EVIDENCE_RECORDED: .crw/evidence/ok.md"); out != "" {
		t.Fatal(out)
	}
	if got := counterComplete(t, cwd, "a/b"); got == "" {
		t.Fatal("a receipt lifted the non-canonical session refusal")
	}
	// The flat file is shared with the canonical twin a-b, so a/b's receipt does not remove it.
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("the shared flat counter was deleted: %v", err)
	}
	if got := evidence.ReadAttempts(cwd, "a-b", "x", "t"); got != 2 {
		t.Fatalf("a/b's receipt changed the twin a-b's counter: %d", got)
	}
}
