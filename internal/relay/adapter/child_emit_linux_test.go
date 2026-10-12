package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func init() {
	if os.Getenv("CRW_READONLY_CHILD_BINARY") == "" {
		return
	}
	runtime.LockOSThread()
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_OPENAT, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 32},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.O_WRONLY | unix.O_RDWR | unix.O_CREAT | unix.O_TRUNC, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EROFS)},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		panic(err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program))); errno != 0 {
		panic(errno)
	}
	if err := unix.Exec(os.Getenv("CRW_READONLY_CHILD_BINARY"), append([]string{"crw"}, os.Args[1:]...), os.Environ()); err != nil {
		panic(err)
	}
}

func blockedChildWorld(t *testing.T) (string, *store.Store, *fakehost.Server) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	host := fakehost.Start(t)
	previous := delivery.ObserveTurn
	delivery.ObserveTurn = hostFactory{}.observeTurn
	t.Cleanup(func() { delivery.ObserveTurn = previous })
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-dispatch-1", "status": "completed"}}, "nextCursor": nil}})
	database := seedIntake(t, filepath.Join(state, "relay.sqlite3"), root)
	canonical, err := store.CanonicalSocket(host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Q(context.Background()).ExecContext(context.Background(), "INSERT INTO schema_meta(key,value) VALUES('socket_path',?)", canonical); err != nil {
		t.Fatal(err)
	}
	return state, database, host
}

func readonlyEmit(t *testing.T, state string, host *fakehost.Server) (map[string]any, error) {
	return readonlyCommand(t, state, host, "emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed")
}

func readonlyCommand(t *testing.T, state string, host *fakehost.Server, args ...string) (map[string]any, error) {
	return readonlyIn(t, "", nil, append([]string{"--state", state, "--socket", host.SocketPath}, args...)...)
}

// readonlyIn runs the relay CLI as a readOnly child in its own working directory and with its own environment.
func readonlyIn(t *testing.T, dir string, env []string, args ...string) (map[string]any, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"relay"}, args...)...)
	cmd.Dir, cmd.Env = dir, append(append(os.Environ(), "CRW_READONLY_CHILD_BINARY="+suiteBinary), env...)
	raw, err := cmd.CombinedOutput()
	var result map[string]any
	if decodeErr := json.Unmarshal(raw, &result); decodeErr != nil {
		t.Fatalf("child output %s: %v (%v)", raw, decodeErr, err)
	}
	return result, err
}

func TestReadOnlyChildClaimsAndDeclaresThroughTheDaemon(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	marker, workspace := filepath.Join(filepath.Dir(state), "markers"), filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, marker)
	if _, err := delivery.DeclareIntent(context.Background(), marker, delivery.IntentDeclaration{Workspace: workspace, DispatchRequestID: "dispatch-1", IssueKey: "REL-1", DBPath: database.Path}); err != nil {
		t.Fatal(err)
	}
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	common := []string{"--workspace", workspace, "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task"}
	for _, command := range [][]string{{"intent-claim", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-dispatch-1"}, {"intent-disposition", "--turn", "turn-dispatch-1", "--outcome", "blocked_needs_input"}} {
		result, err := readonlyCommand(t, state, host, append(command, common...)...)
		record, _ := result["storeRecord"].(map[string]any)
		if err != nil || record["recorded"] != true {
			t.Fatalf("daemon did not record %s: %v %v", command[0], result, err)
		}
	}
}

func TestReadOnlyChildCanEmitBlockedNeedsInput(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := readonlyEmit(t, state, host)
	if err != nil {
		_ = control.Close()
		t.Fatalf("readOnly child could not report through daemon: %v %v", err, result)
	}
	var count int
	if err := database.Q(context.Background()).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events WHERE outcome='blocked_needs_input' AND producer='child'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("child report not persisted: %d %v", count, err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	result, err = readonlyEmit(t, state, host)
	if err == nil || !strings.Contains(fmt.Sprint(result["detail"]), "daemon") || result["reason"] == "store_owned_by_other" {
		t.Fatalf("absent daemon must be explicit, without local store fallback: %v %v", err, result)
	}
}

func TestVerifierReadOnlyChildCannotOverwriteAnUnrelatedManifest(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	artifact := filepath.Join(filepath.Dir(state), "artifact.txt")
	if err := os.WriteFile(artifact, []byte("permitted artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := t.TempDir()
	manifest := filepath.Join(unrelated, "MANIFEST.json")
	before := "unrelated manifest: must not change"
	if err := os.WriteFile(manifest, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	result, commandErr := readonlyCommand(t, state, host, "emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", artifact, "--manifest-ref", unrelated)
	after, err := os.ReadFile(manifest)
	if err != nil || string(after) != before {
		t.Fatalf("writable-open-denied child overwrote unrelated manifest through daemon: %s %v; response=%v, commandErr=%v", after, err, result, commandErr)
	}
	if commandErr == nil || result["reason"] != "manifest_forbidden" {
		t.Fatalf("external freeze destination must be explicitly refused: %v %v", result, commandErr)
	}
	var count int
	if err := database.Q(context.Background()).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused freeze wrote relay events: count=%d, err=%v", count, err)
	}
}

func TestChildCommandRequiresTheDaemonsOwnedStore(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	marker, workspace := filepath.Join(filepath.Dir(state), "markers"), filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, marker)
	if _, err := delivery.DeclareIntent(context.Background(), marker, delivery.IntentDeclaration{Workspace: workspace, DispatchRequestID: "dispatch-1", IssueKey: "REL-1"}); err != nil {
		t.Fatal(err)
	}
	control, err := service.ListenControl(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if result, err := readonlyCommand(t, state, host, "intent-claim", "--workspace", workspace, "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task", "--dispatch-request-id", "dispatch-1"); err == nil {
		t.Fatalf("a guard-only listener accepted a child write: %v", result)
	}
	var count int
	if err := database.Q(context.Background()).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("listener without the daemon's store wrote an event: %d %v", count, err)
	}
}

func TestReadOnlyChildBlockedReachesTheParent(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if result, err := readonlyEmit(t, state, host); err != nil {
		t.Fatalf("blocked report: %v %v", err, result)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		data := []any{}
		if params["archived"] == false {
			data = append(data, map[string]any{"id": "01parent-task"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "parent-report-turn"}}})
	settings, err := json.Marshal(authorized())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Q(context.Background()).ExecContext(context.Background(), "INSERT INTO authorized_settings VALUES('01parent-task',?,'synthetic','stamp')", string(settings)); err != nil {
		t.Fatal(err)
	}
	adapter, err := Open(host.SocketPath, state, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	clock := &delivery.FakeClock{T: 1700000000}
	scheduler := &delivery.Scheduler{Delivery: delivery.NewService(database, clock), MaxSendsTick: 1}
	if err := scheduler.Deliver(context.Background(), adapter, clock.Now(), &delivery.TickCounts{}); err != nil {
		t.Fatal(err)
	}
	var recipient, status string
	if err := database.Q(context.Background()).QueryRowContext(context.Background(), "SELECT recipient_thread_id,state FROM deliveries").Scan(&recipient, &status); err != nil || recipient != "01parent-task" || status != "dispatched" || host.Count("turn/start") != 1 {
		t.Fatalf("parent did not receive blocked report: %s %s %v, calls %v", recipient, status, err, host.Requests())
	}
}

// CRW-1286: what the child selected relative to its own working directory or environment is resolved where the child
// runs, so the daemon, in another directory with another environment, records the report for the caller.
func TestReadOnlyChildSelectionsResolveInTheChildsContext(t *testing.T) {
	for _, mode := range []string{"relative-state", "child-environment-state", "relative-workspace"} {
		t.Run(mode, func(t *testing.T) {
			state, database, host := blockedChildWorld(t)
			root := filepath.Dir(state)
			t.Setenv(store.StateEnv, "")
			t.Setenv(delivery.MarkerEnv, filepath.Join(root, "markers"))
			control, err := service.ListenControlStore(context.Background(), state, database, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			args, env := []string{"--socket", host.SocketPath}, []string{}
			switch mode {
			case "relative-state":
				args = append(args, "--state", "state")
			case "child-environment-state":
				env = append(env, store.StateEnv+"="+state)
			default:
				if _, err := delivery.DeclareIntent(context.Background(), filepath.Join(root, "markers"), delivery.IntentDeclaration{Workspace: root, DispatchRequestID: "dispatch-1", IssueKey: "REL-1", DBPath: database.Path}); err != nil {
					t.Fatal(err)
				}
				result, err := readonlyIn(t, root, nil, append(args, "--state", state, "intent-claim", "--workspace", ".", "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-dispatch-1")...)
				if record, _ := result["storeRecord"].(map[string]any); err != nil || record["recorded"] != true {
					t.Fatalf("a claim for the child's own workspace was not recorded: %v %v", result, err)
				}
				return
			}
			result, err := readonlyIn(t, root, env, append(args, "emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed")...)
			var count int
			if queryErr := database.Q(context.Background()).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events WHERE outcome='blocked_needs_input'").Scan(&count); err != nil || queryErr != nil || count != 1 {
				t.Fatalf("the child's own state selection did not reach the daemon's store: %v %v, events=%d %v", result, err, count, queryErr)
			}
		})
	}
}

// CRW-1286: the daemon never resolves a caller-relative value in its own directory: a request that still carries one is
// refused, and nothing is published.
func TestTheDaemonRefusesAnUnresolvedCallerRelativeValue(t *testing.T) {
	state, database, _ := blockedChildWorld(t)
	root := filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, filepath.Join(root, "markers"))
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	address, release, err := hook.ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	conn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	argv := []string{"--state", state, "intent-claim", "--marker-root", filepath.Join(root, "markers"), "--workspace", ".", "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task", "--dispatch-request-id", "dispatch-1"}
	request, _ := json.Marshal(map[string]any{"protocol": 1, "method": "child-command", "program": "crw", "argv": argv})
	if _, err := conn.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(answer, `"error"`) || !strings.Contains(answer, "--workspace") {
		t.Fatalf("a caller-relative --workspace must be refused by name: %q %v", answer, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "markers")); err == nil && len(entries) > 0 {
		t.Fatalf("a refused request published markers: %v", entries)
	}
}
