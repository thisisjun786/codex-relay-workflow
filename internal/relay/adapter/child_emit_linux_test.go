package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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
	// A read-only filesystem as the child sees it: a writable open, and every call that makes, removes, renames or
	// links a name, fails with EROFS.
	changes := []uint32{unix.SYS_MKDIRAT, unix.SYS_MKNODAT, unix.SYS_UNLINKAT, unix.SYS_RENAMEAT, unix.SYS_RENAMEAT2, unix.SYS_LINKAT, unix.SYS_SYMLINKAT}
	filters := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	for index, number := range changes {
		filters = append(filters, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: number, Jt: uint8(len(changes) - index + 4)})
	}
	filters = append(filters, []unix.SockFilter{
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_OPENAT, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 32},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.O_WRONLY | unix.O_RDWR | unix.O_CREAT | unix.O_TRUNC, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EROFS)},
	}...)
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
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"relay", "--state", state, "--socket", host.SocketPath}, args...)...)
	cmd.Env = append(os.Environ(), "CRW_READONLY_CHILD_BINARY="+suiteBinary)
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

// CRW-1286: the relay daemon process itself (crw relay daemon, its control.sock over the store it owns) records a readOnly
// child's report, not only a listener a test builds over a store it opened.
func TestTheRelayDaemonProcessRecordsAReadOnlyChildsReport(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(root, "scopes"))
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-dispatch-1", "status": "completed"}}, "nextCursor": nil}})
	// The store is bound to the host socket the daemon serves, so the daemon takes it as it is.
	database := seedIntake(t, filepath.Join(state, "relay.sqlite3"), root, host.SocketPath)
	var output bytes.Buffer
	daemon := exec.Command(suiteBinary, "relay", "--state", state, "--socket", host.SocketPath, "daemon", "--deadline", "60", "--allow-isolated-scope")
	daemon.Stdout, daemon.Stderr = &output, &output
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
	address, release, err := hook.ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if conn, err := net.Dial("unix", address); err == nil {
			_ = conn.Close()
			break
		} else if time.Since(start) > 20*time.Second {
			t.Fatalf("the daemon never served control.sock: %v", err)
		}
	}
	if result, err := readonlyEmit(t, state, host); err != nil || countRows(t, database, "SELECT COUNT(*) FROM events WHERE outcome='blocked_needs_input' AND producer='child'") != 1 {
		_, _ = daemon.Process.Kill(), daemon.Wait()
		t.Fatalf("the daemon process did not record the readOnly child's report: %v %v; daemon: %s", result, err, output.String())
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

var blockedEmit = []string{"emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed"}

// childIn runs the relay CLI as a readOnly child in its own working directory, env added to its environment.
func childIn(t *testing.T, dir string, env []string, args ...string) (map[string]any, error) {
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

// CRW-1286: the relay daemon alone decides where a child report is recorded. The child's state selection, a flag
// absolute or relative to its own directory or its environment, only finds the daemon's socket; the daemon, in
// another directory with its own environment naming another state, records the report in its own store.
func TestChildReportStateLocationIsTheDaemons(t *testing.T) {
	for _, selection := range []struct {
		name      string
		args, env []string
	}{{"absolute-flag", []string{"--state", ""}, nil}, {"relative-flag", []string{"--state", "state"}, nil}, {"child-environment", nil, []string{store.StateEnv + "="}}} {
		t.Run(selection.name, func(t *testing.T) {
			state, database, host := blockedChildWorld(t)
			decoy := t.TempDir()
			t.Setenv(store.StateEnv, decoy)
			control, err := service.ListenControlStore(context.Background(), state, database, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			args, env := slices.Clone(selection.args), slices.Clone(selection.env)
			if len(args) > 0 && args[1] == "" {
				args[1] = state
			}
			if len(env) > 0 {
				env[0] += state
			}
			result, err := childIn(t, filepath.Dir(state), env, append(append(args, "--socket", host.SocketPath), blockedEmit...)...)
			if err != nil || countRows(t, database, "SELECT COUNT(*) FROM events WHERE outcome='blocked_needs_input'") != 1 {
				t.Fatalf("the child's report is not in the daemon's store: %v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(decoy, "relay.sqlite3")); !os.IsNotExist(err) {
				t.Fatalf("a relay store was made where the daemon's environment points: %v", err)
			}
		})
	}
}

// CRW-1286: a request that reaches the daemon naming another state, an initialized store holding the same
// relationship or a path relative to the child, cannot redirect the record: the daemon records it in its own store.
func TestAChildStateSelectionCannotRedirectTheDaemonsRecord(t *testing.T) {
	for _, named := range []string{"other", "state"} {
		t.Run(named, func(t *testing.T) {
			state, database, host := blockedChildWorld(t)
			otherRoot := t.TempDir()
			other := seedIntake(t, filepath.Join(otherRoot, "state", "relay.sqlite3"), otherRoot)
			if named == "other" {
				named = filepath.Dir(other.Path)
			}
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
			argv := [][]byte{} // base64 on the wire, as hook.ForwardChild sends it
			for _, argument := range append([]string{"--state", named, "--socket", host.SocketPath}, blockedEmit...) {
				argv = append(argv, []byte(argument))
			}
			request, _ := json.Marshal(map[string]any{"protocol": 1, "method": "child-command", "program": "crw", "argv": argv})
			var answer map[string]any
			if _, err := conn.Write(append(request, '\n')); err != nil || json.NewDecoder(conn).Decode(&answer) != nil {
				t.Fatalf("no answer: %v %v", answer, err)
			}
			if countRows(t, database, "SELECT COUNT(*) FROM events") != 1 || countRows(t, other, "SELECT COUNT(*) FROM events") != 0 {
				t.Fatalf("the child's --state %s decided where its report is recorded: %v", named, answer)
			}
		})
	}
}

// CRW-1286: the daemon never interprets a path relative to the child. A child naming one gets an explicit error
// naming the option, and nothing is recorded.
func TestTheDaemonRefusesACallerRelativePath(t *testing.T) {
	claim := []string{"intent-claim", "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task", "--dispatch-request-id", "dispatch-1"}
	for _, relative := range []struct {
		option string
		argv   []string
	}{
		{"--socket", append([]string{"--socket", "host.sock"}, blockedEmit...)},
		{"--independent-review", append(slices.Clone(blockedEmit), "--independent-review", "review.json")},
		{"--workspace", append(slices.Clone(claim), "--workspace", ".")},
		{"--marker-root", append(slices.Clone(claim), "--marker-root", "markers", "--workspace", "")},
	} {
		t.Run(relative.option, func(t *testing.T) {
			state, database, _ := blockedChildWorld(t)
			root := filepath.Dir(state)
			t.Setenv(delivery.MarkerEnv, filepath.Join(root, "markers"))
			if _, err := delivery.DeclareIntent(context.Background(), filepath.Join(root, "markers"), delivery.IntentDeclaration{Workspace: root, DispatchRequestID: "dispatch-1", IssueKey: "REL-1", DBPath: database.Path}); err != nil {
				t.Fatal(err)
			}
			argv := slices.Clone(relative.argv)
			if index := slices.Index(argv, ""); index > 0 {
				argv[index] = root
			}
			control, err := service.ListenControlStore(context.Background(), state, database, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			result, err := childIn(t, root, nil, append([]string{"--state", state}, argv...)...)
			if detail := fmt.Sprint(result["detail"]); err == nil || !strings.Contains(detail, relative.option) || !strings.Contains(detail, "caller-relative") {
				t.Fatalf("a caller-relative %s must be refused by name: %v %v", relative.option, result, err)
			}
			if countRows(t, database, "SELECT COUNT(*) FROM events")+countRows(t, database, "SELECT COUNT(*) FROM reporting_sessions") != 0 {
				t.Fatalf("a refused request was recorded: %v", result)
			}
		})
	}
}
