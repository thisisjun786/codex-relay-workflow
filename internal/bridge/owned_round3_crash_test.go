package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// The child half of the crash test: it launches against the parent's fakehost and is killed
// there while the host holds turn/start. It never returns on its own.
func Test_round3_crash_child(t *testing.T) {
	socket, state := os.Getenv("CRW15_CRASH_SOCKET"), os.Getenv("CRW15_CRASH_STATE")
	if socket == "" {
		t.Skip("child half of Test_round3_a_process_killed_while_the_prompt_is_in_flight_leaves_it_unknown")
	}
	var input CreateWorktree
	input.RequestID, input.Source, input.Revision, input.Destination = "crash", os.Getenv("CRW15_CRASH_SOURCE"), os.Getenv("CRW15_CRASH_REVISION"), os.Getenv("CRW15_CRASH_DESTINATION")
	input.Mode, input.Sandbox, input.Policy = "bridge-managed-retained", "read-only", map[string]any{"type": "readOnly", "networkAccess": false}
	input.Model, input.Effort, input.Prompt = "explicit-model", "high", "work"
	client, err := appserver.Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = New(client, store, executionPolicy()).CreateWorktreeThread(context.Background(), input)
	t.Fatal("the parent should have killed this process mid-dispatch")
}

func Test_round3_a_process_killed_while_the_prompt_is_in_flight_leaves_it_unknown(t *testing.T) {
	host := fakehost.Start(t)
	input := worktreeInput(t)
	worktreeHost(host, input.Destination)
	paused, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	host.Script("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}, Paused: paused, Release: release})
	state := filepath.Join(t.TempDir(), "operations.sqlite3")
	child := exec.Command(os.Args[0], "-test.run", "^Test_round3_crash_child$", "-test.count=1")
	child.Env = append(os.Environ(), "CRW15_CRASH_SOCKET="+host.SocketPath, "CRW15_CRASH_STATE="+state, "CRW15_CRASH_SOURCE="+input.Source, "CRW15_CRASH_REVISION="+input.Revision, "CRW15_CRASH_DESTINATION="+input.Destination)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	select {
	case <-paused:
	case err := <-exited:
		t.Fatalf("child ended before turn/start reached the host: %v", err)
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("turn/start never reached the host")
	}
	// The frame is on the wire and unanswered: kill the process there, as a crash would.
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-exited
	store, err := ledger.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	retained, err := store.Get(context.Background(), "crash")
	if err != nil {
		t.Fatal(err)
	}
	if retained["phase"] != "dispatching_initial_prompt" || object(retained["initialPrompt"])["state"] != "outcome_unknown" || retained["status"] != "in_progress_or_unknown" || retained["recoveryRequired"] != true || retained["recovery"] != worktreeRecovery {
		t.Fatalf("a crash mid-dispatch left %v", retained)
	}
	// A restarted bridge answers the request id from that row and sends nothing again.
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	input.RequestID, input.Prompt = "crash", "work"
	before := host.Count("turn/start")
	replay, err := New(client, store, executionPolicy()).CreateWorktreeThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || object(replay["initialPrompt"])["state"] != "outcome_unknown" || host.Count("turn/start") != before {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
}
