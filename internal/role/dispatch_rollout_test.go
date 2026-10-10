package role

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchCorrectionRollout writes a rollout of one child at path holding the events of one turn: started, and ended when end.
func dispatchCorrectionRollout(t *testing.T, path string, end bool) {
	t.Helper()
	lines := []string{`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`}
	if end {
		lines = append(lines, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}`)
	}
	check(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// dispatchCorrectionSwaps are the ways a rollout path can change between the look at it and its open. Each leaves the path
// leading to something that is not the file that was looked at.
var dispatchCorrectionSwaps = map[string]func(t *testing.T, path, other string){
	"symlink to another rollout": func(t *testing.T, path, other string) {
		check(t, os.Remove(path))
		check(t, os.Symlink(other, path))
	},
	"another regular file": func(t *testing.T, path, other string) {
		// The copy exists beside the original, so the two cannot share an inode.
		check(t, os.WriteFile(path+".copy", must(os.ReadFile(other)), 0o600))
		check(t, os.Rename(path+".copy", path))
	},
	"named pipe": func(t *testing.T, path, _ string) {
		check(t, os.Remove(path))
		check(t, syscall.Mkfifo(path, 0o600))
	},
}

// The rollout is evidence only through the file that was looked at: a path replaced between the look and the open, by a link to
// another rollout, another regular file or a named pipe, shows nothing, and the open never blocks.
func TestDispatchRolloutIsTheFileThatWasLookedAt(t *testing.T) {
	for name, swap := range dispatchCorrectionSwaps {
		t.Run("newest turn/"+name, func(t *testing.T) {
			dir := t.TempDir()
			mine, other := filepath.Join(dir, "child-a.jsonl"), filepath.Join(dir, "other.jsonl")
			dispatchCorrectionRollout(t, mine, false)
			dispatchCorrectionRollout(t, other, true)
			dispatchRolloutBeforeOpen = func() { swap(t, mine, other) }
			t.Cleanup(func() { dispatchRolloutBeforeOpen = nil })
			done := make(chan string, 1)
			go func() { done <- dispatchRolloutNewest(context.Background(), mine) }()
			select {
			case got := <-done:
				if got != "" {
					t.Fatalf("a replaced rollout showed the newest turn %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reading a replaced rollout blocked")
			}
		})
		t.Run("spawn result/"+name, func(t *testing.T) {
			env, _ := home(t)
			native := dispatchReceiptNative(t, env)
			dispatchReceiptParent(t, native, dispatchReceiptSpawn{"call-2", "completed", []string{"child-2"}})
			dir, _ := native("CODEX_HOME")
			mine, other := filepath.Join(dir, "rollout-session-test.jsonl"), filepath.Join(dir, "other.jsonl")
			check(t, os.WriteFile(other, []byte(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CollabAgentToolCall","id":"call-1","tool":"spawn_agent","status":"completed","sender_thread_id":"session-test","receiver_thread_ids":["child-a"]}}}`+"\n"), 0o600))
			dispatchRolloutBeforeOpen = func() { swap(t, mine, other) }
			t.Cleanup(func() { dispatchRolloutBeforeOpen = nil })
			type answer struct {
				r   createdSpawnResult
				err error
			}
			done := make(chan answer, 1)
			go func() {
				r, err := createdCheckSpawnResult(context.Background(), native, "session-test", "call-1")
				done <- answer{r, err}
			}()
			select {
			case got := <-done:
				if got.r.Seen {
					t.Fatalf("a replaced parent rollout showed the issued call's result %+v", got.r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reading a replaced parent rollout blocked")
			}
		})
	}
}

// dispatchBoundEvent is one rollout line of the host's event payload, padded by pad bytes inside the payload so its length can
// pass a scan bound while it still parses.
func dispatchBoundEvent(payload map[string]any, pad int) string {
	p := map[string]any{"pad": strings.Repeat("x", pad)}
	for k, v := range payload {
		p[k] = v
	}
	return string(must(json.Marshal(map[string]any{"type": "event_msg", "payload": p})))
}

// dispatchBoundSpawn is the host's completed item of the issued spawn call call-1 naming child-a, padded by pad bytes.
func dispatchBoundSpawn(pad int) string {
	item := map[string]any{"type": "CollabAgentToolCall", "id": "call-1", "tool": "spawn_agent", "status": "completed", "sender_thread_id": "session-test", "receiver_thread_ids": []string{"child-a"}, "prompt": strings.Repeat("x", pad)}
	return dispatchBoundEvent(map[string]any{"type": "item_completed", "thread_id": "session-test", "item": item}, 0)
}

// dispatchBoundParent gives env a parent session row whose rollout holds lines, and returns the environment.
func dispatchBoundParent(t *testing.T, env host.LookupEnv, lines ...string) host.LookupEnv {
	t.Helper()
	native := dispatchReceiptNative(t, env)
	dispatchReceiptParent(t, native)
	dir, _ := native("CODEX_HOME")
	check(t, os.WriteFile(filepath.Join(dir, "rollout-session-test.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return native
}

// A rollout scan ends with the observation that asked for it: once its context is done, the newest turn and the issued call's
// result stay unseen, however much of the file is left.
func TestDispatchRolloutScanEndsWithItsContext(t *testing.T) {
	started, ended := map[string]any{"type": "task_started", "turn_id": "turn-1"}, map[string]any{"type": "task_complete", "turn_id": "turn-1"}
	t.Run("newest turn", func(t *testing.T) {
		env, _ := home(t)
		env = dispatchHandoffNative(t, env, started, ended)
		if o := dispatchObserve(context.Background(), env, nil, "session-test", "child-a", true); o.Newest != "completed" {
			t.Fatalf("the rollout's newest turn = %+v", o)
		}
		ctx, cancel := context.WithCancel(context.Background())
		dispatchRolloutBeforeOpen = cancel
		t.Cleanup(func() { dispatchRolloutBeforeOpen = nil })
		if o := dispatchObserve(ctx, env, nil, "session-test", "child-a", true); o.Newest != "" {
			t.Fatalf("a scan after its observation ended showed %+v", o)
		}
	})
	t.Run("spawn result", func(t *testing.T) {
		env, _ := home(t)
		env = dispatchBoundParent(t, env, dispatchBoundSpawn(0))
		if r, err := createdCheckSpawnResult(context.Background(), env, "session-test", "call-1"); err != nil || !r.Seen {
			t.Fatalf("the issued call's result = %+v, %v", r, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		dispatchRolloutBeforeOpen = cancel
		t.Cleanup(func() { dispatchRolloutBeforeOpen = nil })
		if r, err := createdCheckSpawnResult(ctx, env, "session-test", "call-1"); err == nil || r.Seen {
			t.Fatalf("a scan after its context ended showed %+v, %v", r, err)
		}
	})
}

// A rollout scan reads at most a bounded line and a bounded file. A line over the line bound is not decoded or held: it is
// damage to the newest turn (the end after it is unseen until a later turn starts and ends whole) and never the issued call's
// result, and the lines after it are read. A file over the read bound shows nothing.
func TestDispatchRolloutScanBoundsWhatItReads(t *testing.T) {
	lineCap, readCap := dispatchRolloutLineCap, dispatchRolloutReadCap
	t.Cleanup(func() { dispatchRolloutLineCap, dispatchRolloutReadCap = lineCap, readCap })
	turn := func(id string, pad int) []map[string]any {
		return []map[string]any{{"type": "task_started", "turn_id": id}, {"type": "task_complete", "turn_id": id, "pad": strings.Repeat("x", pad)}}
	}
	newest := func(t *testing.T, events ...map[string]any) string {
		env, _ := home(t)
		return dispatchObserve(context.Background(), dispatchHandoffNative(t, env, events...), nil, "session-test", "child-a", true).Newest
	}
	// Within the bounds a line longer than the reader's buffer is read whole.
	if got := newest(t, turn("turn-1", 200_000)...); got != "completed" {
		t.Errorf("a long end within the line bound = %q", got)
	}
	dispatchRolloutLineCap = 1024
	if got := newest(t, turn("turn-1", 4096)...); got != "" {
		t.Errorf("an over-long end showed the newest turn %q", got)
	}
	if got := newest(t, append(turn("turn-1", 4096), turn("turn-2", 0)...)...); got != "completed" {
		t.Errorf("a whole turn after an over-long line = %q", got)
	}
	seen := func(t *testing.T, lines ...string) bool {
		env, _ := home(t)
		r, err := createdCheckSpawnResult(context.Background(), dispatchBoundParent(t, env, lines...), "session-test", "call-1")
		return err == nil && r.Seen
	}
	if seen(t, dispatchBoundSpawn(4096)) {
		t.Error("an over-long line was read as the issued call's result")
	}
	if !seen(t, dispatchBoundSpawn(4096), dispatchBoundSpawn(0)) {
		t.Error("the issued call's result after an over-long line was not read")
	}
	dispatchRolloutLineCap, dispatchRolloutReadCap = lineCap, 2048
	if got := newest(t, append(turn("turn-1", 0), turn("turn-2", 3000)...)...); got != "" {
		t.Errorf("a rollout over the read bound showed the newest turn %q", got)
	}
	if seen(t, dispatchBoundEvent(map[string]any{"type": "token_count"}, 3000), dispatchBoundSpawn(0)) {
		t.Error("a parent rollout over the read bound showed the issued call's result")
	}
}
