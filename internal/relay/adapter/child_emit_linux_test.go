package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
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
	// links a name, fails with EROFS. That is Codex's read-only sandbox with its network enabled, where the child's
	// connect to control.sock is allowed (codex-cli 0.154.0, CRW-1286). CRW_READONLY_CHILD_NO_NETWORK adds the refusal
	// Codex gives a sandbox without network access, read-only or workspace-write: every connect fails with EPERM.
	if os.Getenv("CRW_READONLY_CHILD_NO_NETWORK") != "" {
		installSeccomp([]unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CONNECT, Jf: 1},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		})
	}
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
	installSeccomp(filters)
	if err := unix.Exec(os.Getenv("CRW_READONLY_CHILD_BINARY"), append([]string{"crw"}, os.Args[1:]...), os.Environ()); err != nil {
		panic(err)
	}
}

func installSeccomp(filters []unix.SockFilter) {
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		panic(err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program))); errno != 0 {
		panic(errno)
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

// CRW-1286: in a sandbox without network access (Codex's read-only and workspace-write ones) the child cannot connect
// to the daemon's socket. Each child command then ends with the explicit exit 3, naming the sandbox as the reason and
// the sandboxes that can report, records nothing and writes nothing locally, while the daemon is listening.
func TestAChildWithoutNetworkAccessIsToldItsSandboxCannotReport(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	marker, workspace := filepath.Join(filepath.Dir(state), "markers"), filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, marker)
	t.Setenv("CRW_READONLY_CHILD_NO_NETWORK", "1")
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	common := []string{"--workspace", workspace, "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task"}
	for _, command := range [][]string{
		{"emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed"},
		append([]string{"intent-claim", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-dispatch-1"}, common...),
		append([]string{"intent-disposition", "--turn", "turn-dispatch-1", "--outcome", "blocked_needs_input"}, common...),
	} {
		result, err := readonlyCommand(t, state, host, command...)
		detail := fmt.Sprint(result["detail"])
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(detail, "child report was not submitted") || !strings.Contains(detail, "operation not permitted") ||
			!strings.Contains(detail, "sandbox") || !strings.Contains(detail, "danger-full-access") || !strings.Contains(detail, "network access") {
			t.Fatalf("%s: expected the explicit sandbox refusal (exit 3), got %v %v", command[0], err, result)
		}
	}
	if rows := countRows(t, database, "SELECT COUNT(*) FROM events") + countRows(t, database, "SELECT COUNT(*) FROM reporting_sessions"); rows != 0 {
		t.Fatalf("a child that could not reach the daemon recorded %d rows", rows)
	}
	if entries, err := os.ReadDir(marker); err == nil && len(entries) != 0 {
		t.Fatalf("a child that could not reach the daemon published markers: %v", entries)
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

// CRW-1289: a child report that takes longer than the control socket's 5 s request line, here a host read of 7 s, is
// served to its end on both sides of the hop and recorded, not answered as an unknown outcome.
func TestASlowChildReportIsServedToItsEnd(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	host.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
		time.Sleep(7 * time.Second)
		return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-dispatch-1", "status": "completed"}}, "nextCursor": nil}}
	})
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if result, err := readonlyEmit(t, state, host); err != nil || countRows(t, database, "SELECT COUNT(*) FROM events WHERE outcome='blocked_needs_input'") != 1 {
		t.Fatalf("a slow child report was cut short by the hop: %v %v", result, err)
	}
}

// CRW-1289: the daemon's handler takes a child command's arguments as the child gave them, one that is not UTF-8
// included, and the child writes the command's stdout and stderr bytes and exits with its code: what the command
// answers when it runs on the daemon's store in the daemon itself.
func TestTheHopCarriesAChildCommandByteForByte(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	workspace := filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, filepath.Join(workspace, "markers"))
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	owned := dispatch.OwnedChildState(store.WithBorrowedStore(context.Background(), database), state)
	for _, call := range []struct {
		argv     []string
		answered string
	}{
		{[]string{"--json", "intent-claim", "--workspace", workspace, "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "v", "--dispatch-request-id", "x\xffy"}, `'\\udcff' in position 1: surrogates not allowed`},
		{append(slices.Clone(blockedEmit), "--relationship", "r\xfe\xff"), "position 1-2: surrogates not allowed"},
		{append(slices.Clone(blockedEmit), "--turn-id", "turn-\xff"), `turn \"turn-\\xff\" does not exist`},
	} {
		argv := append([]string{"--socket", host.SocketPath}, call.argv...)
		var out, errOut, ownOut, ownErr bytes.Buffer
		code, err := hook.ForwardChild(context.Background(), state, "crw", argv, &out, &errOut)
		own := dispatch.ExecuteOwnedChild(owned, "crw", argv, &ownOut, &ownErr)
		if err != nil || code != own || !bytes.Equal(out.Bytes(), ownOut.Bytes()) || !bytes.Equal(errOut.Bytes(), ownErr.Bytes()) || !strings.Contains(out.String(), call.answered) {
			t.Fatalf("%q through the hop: %d %q %q %v; on the daemon's store: %d %q %q", argv, code, out.String(), errOut.String(), err, own, ownOut.String(), ownErr.String())
		}
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

// CRW-1299: the relay daemon alone decides where a child report is recorded. The child's state selection, a flag
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

// CRW-1299: a request that reaches the daemon naming another state, an initialized store holding the same
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

// CRW-1299: the daemon never interprets a path relative to the child. A child naming one gets an explicit error
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

// CRW-1286: a --kind-module that installs something (codex_session_relay.projects) would install it in the relay daemon
// for every later command, so a child report naming one is refused and records nothing; one that installs nothing is
// served.
func TestTheDaemonRefusesAChildKindModuleThatInstalls(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	result, err := readonlyCommand(t, state, host, append([]string{"--kind-module", "codex_session_relay.projects"}, blockedEmit...)...)
	var exit *exec.ExitError
	if detail := fmt.Sprint(result["detail"]); !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(detail, "codex_session_relay.projects") || countRows(t, database, "SELECT COUNT(*) FROM events") != 0 {
		t.Fatalf("a child's installing --kind-module must be refused and record nothing: %v %v", result, err)
	}
	if result, err := readonlyCommand(t, state, host, append([]string{"--kind-module", "json"}, blockedEmit...)...); err != nil || countRows(t, database, "SELECT COUNT(*) FROM events") != 1 {
		t.Fatalf("a child's --kind-module that installs nothing is served: %v %v", result, err)
	}
}

// CRW-1286: an intent naming the daemon's store by another spelling (a symbolic link to its state directory) names the
// daemon's own store, and the daemon records the child's claim there.
func TestAChildIntentNamingTheDaemonsStoreByAnotherSpellingIsRecorded(t *testing.T) {
	state, database, host := blockedChildWorld(t)
	marker, workspace := filepath.Join(filepath.Dir(state), "markers"), filepath.Dir(state)
	t.Setenv(delivery.MarkerEnv, marker)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.DeclareIntent(context.Background(), marker, delivery.IntentDeclaration{Workspace: workspace, DispatchRequestID: "dispatch-1", IssueKey: "REL-1", DBPath: filepath.Join(alias, "relay.sqlite3")}); err != nil {
		t.Fatal(err)
	}
	control, err := service.ListenControlStore(context.Background(), state, database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	result, err := readonlyCommand(t, state, host, "intent-claim", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-dispatch-1", "--workspace", workspace, "--assignment", delivery.AssignmentID("dispatch-1"), "--session", "01child-task")
	if record, _ := result["storeRecord"].(map[string]any); err != nil || record["recorded"] != true || countRows(t, database, "SELECT COUNT(*) FROM reporting_sessions") != 1 {
		t.Fatalf("the daemon's store named through a symbolic link was not recorded in: %v %v", result, err)
	}
}
