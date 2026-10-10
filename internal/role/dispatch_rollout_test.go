package role

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
			go func() { done <- dispatchRolloutNewest(mine) }()
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
