//go:build linux

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

func Test30ControlSocketRealSurface(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	server, err := ListenControl(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", ControlPath(state))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := hook.RequestGuard(ctx, conn, hook.Object{}, hook.GuardOptions{Root: t.TempDir(), Mode: hook.Observe, Now: "2026-01-01T00:00:00Z"})
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if get(answer, "decision") != "release" || get(answer, "state") != "unmanaged" {
		t.Fatal(answer)
	}
	conn, err = net.Dial("unix", ControlPath(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write([]byte("{\"protocol\":1,\"method\":\"inbox-submit\",\"params\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	var queued map[string]any
	if err = json.NewDecoder(conn).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if queued["reason"] != "inbox_unavailable" || queued["status"] != nil {
		t.Fatal(queued)
	}
	if err = server.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(state, 0770); err != nil {
		t.Fatal(err)
	}
	if server, err = ListenControl(ctx, state); err == nil {
		_ = server.Close()
		t.Fatal("unsafe socket parent accepted")
	}
}
func takeoverCLI(t *testing.T, home string, args ...string) (map[string]any, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	argv := append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket", "takeover"}, args...)
	cmd := exec.CommandContext(ctx, testBinary, argv...)
	cmd.Env = environment(home)
	raw, err := cmd.CombinedOutput()
	t.Logf("CLI %q stdout=%s error=%v", argv, raw, err)
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%v %s", err, raw)
		}
		code = exit.ExitCode()
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	return result, code
}
func seedTakeover(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "t30-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary state survived cleanup: %v", err)
		}
		t.Logf("CLEANUP removed %s", home)
	})
	path := home + "/state/relay.sqlite3"
	if err = testsupport.SeedOwnership(t.Context(), path, home+"/socket", "python"); err != nil {
		t.Fatal(err)
	}
	r, err := ownership.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
	if err = scope.Prepare(); err != nil {
		t.Fatal(err)
	}
	key := scope.Key(home + "/socket")
	r.ScopeKey = &key
	if err = ownership.Publish(path, r, nil); err != nil {
		t.Fatal(err)
	}
	return home
}
func Test30TakeoverBuiltCLI(t *testing.T) {
	home := seedTakeover(t)
	before, err := ownership.Physical(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status", "--json"}, {"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}, {"activate"}} {
		result, code := takeoverCLI(t, home, args...)
		if code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
		if args[0] == "activate" {
			if result["owner"] != "go" || result["phase"] != "active" || result["epoch"] != float64(2) {
				t.Fatal(result)
			}
		}
	}
	record, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	if record.Holder == nil {
		t.Fatal("no ready holder")
	}
	process(t, record.Holder.PID)
	// Exercise the stable RPC through the real activated daemon, not a test stub.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := net.Dial("unix", home+"/state/control.sock")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := hook.RequestGuard(ctx, conn, hook.Object{}, hook.GuardOptions{Root: home + "/markers", Mode: hook.Observe})
	_ = conn.Close()
	if err != nil || get(answer, "decision") != "release" {
		t.Fatal(answer, err)
	}
	result, code := takeoverCLI(t, home, "rollback", "--to", "python")
	if code != 2 || result["reason"] != "store_owned_by_other" {
		t.Fatalf("Python candidate contract must refuse explicitly, got %d %+v", code, result)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "python" || status["phase"] != "starting" || status["epoch"] != float64(3) {
		t.Fatal(code, status)
	}
	after, err := ownership.Physical(home + "/state/relay.sqlite3")
	if err != nil || after != before {
		t.Fatal(after, err)
	}
	t.Log("QA native begin/drain/transfer/activate and control.sock succeeded; reverse CAS retained physical DB, Python activation correctly blocked on todo 36")
}
func Test30StatusSchema(t *testing.T) {
	home := seedTakeover(t)
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 {
		t.Fatal(status)
	}
	raw, err := os.ReadFile(filepath.Join(testRoot, "contract/schema/records.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]struct {
		Keys []string `json:"keys"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for k := range status {
		keys[k] = true
	}
	expected := map[string]bool{}
	for _, k := range schema["takeoverStatus"].Keys {
		expected[k] = true
	}
	if !reflect.DeepEqual(keys, expected) {
		t.Fatal(keys, expected)
	}
}
func Test30ForeignOwnerCLIRefusesWithoutDBChanges(t *testing.T) {
	home := seedTakeover(t)
	path := home + "/state/relay.sqlite3"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "daemon", "--allow-isolated-scope", "--max-ticks", "0")
	cmd.Env = environment(home)
	raw, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatal(err, string(raw))
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("foreign opener changed DB", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("foreign opener created sidecar", suffix, err)
		}
	}
}

// Channel peer closes before active publication: Ready must refuse and close
// admission, not leave a writable unadvertised candidate. net.Pipe gives an
// exact EOF signal without sleeps.
func Test30ActivationChannelClosure(t *testing.T) {
	home := seedTakeover(t)
	path := home + "/state/relay.sqlite3"
	for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
		r, c := takeoverCLI(t, home, args...)
		if c != 0 {
			t.Fatal(r)
		}
	}
	r, err := ownership.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	channel := CandidateChannel{conn: client, Record: r}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, e := bufio.NewReader(server).ReadBytes('\n'); done <- errors.Join(e, server.Close()) }()
	if err = channel.Ready(t.Context(), "test-build"); err == nil {
		t.Fatal("unadvertised candidate continued after EOF")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func Test30ControllerCrashProcess(t *testing.T) {
	home := os.Getenv("CRW30_CONTROLLER_CRASH_HOME")
	if home == "" {
		return
	}
	t.Setenv(ScopeEnv, home+"/scopes")
	c, err := NewTakeover(t.Context(), store.StateSelection{Path: home + "/state"}, home+"/socket", "test")
	if err != nil {
		t.Fatal(err)
	}
	c.Fault = func(step, point string) error {
		if step == "transfer" && point == "db-committed" {
			os.Exit(91)
		}
		return nil
	}
	t.Fatalf("controller did not reach crash point: %v", c.Transfer(t.Context()))
}

func Test30ActivationEOFRequiresMatchingPublishedHolder(t *testing.T) {
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "other-holder", true: "matching-holder"}[matches], func(t *testing.T) {
			home := seedTakeover(t)
			t.Setenv(ScopeEnv, home+"/scopes")
			for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
				if answer, code := takeoverCLI(t, home, args...); code != 0 {
					t.Fatal(answer)
				}
			}
			path := home + "/state/relay.sqlite3"
			r, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			if err = client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			channel := CandidateChannel{conn: client, Record: r}
			done := make(chan error, 1)
			go func() { done <- channel.Ready(t.Context(), "candidate-build") }()
			var ready candidateMessage
			if err = json.NewDecoder(server).Decode(&ready); err != nil {
				t.Fatal(err)
			}
			r.Phase, r.Holder = "active", &ready.Identity
			if !matches {
				r.Holder.PID++
			}
			if err = ownership.Publish(path, r, nil); err != nil {
				t.Fatal(err)
			}
			if err = server.Close(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; (err == nil) != matches {
				t.Fatalf("published holder matches=%t: %v", matches, err)
			}
		})
	}
}

func Test30BuiltCLIRecoversCommittedCrash(t *testing.T) {
	home := seedTakeover(t)
	if result, code := takeoverCLI(t, home, "begin", "--to", "go"); code != 0 {
		t.Fatal(result)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test30ControllerCrashProcess$")
	child.Env = append(os.Environ(), "CRW30_CONTROLLER_CRASH_HOME="+home)
	raw, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 91 {
		t.Fatalf("controller crash: %v %s", err, raw)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "starting" || status["jsonStale"] != true {
		t.Fatal(code, status)
	}
	status, code = takeoverCLI(t, home, "activate")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["jsonStale"] != false {
		t.Fatal(code, status)
	}
	r, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
	if err != nil || r.Holder == nil {
		t.Fatal(r, err)
	}
	process(t, r.Holder.PID)
}

// Drive the actual daemon's inherited channel. EOF is the event, not a sleep;
// a pidfd observes exit and flock acquisition proves every writer lock is gone.
func Test30RealCandidateControllerEOF(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-active", true: "after-active"}[published], func(t *testing.T) {
			home := seedTakeover(t)
			t.Setenv(ScopeEnv, home+"/scopes")
			path := home + "/state/relay.sqlite3"
			for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
				if result, code := takeoverCLI(t, home, args...); code != 0 {
					t.Fatal(result)
				}
			}
			r, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			identity := ProcessIdentity("")
			r.Controller = &identity
			if err = ownership.Publish(path, r, nil); err != nil {
				t.Fatal(err)
			}
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := os.NewFile(uintptr(pair[0]), "controller")
			child := os.NewFile(uintptr(pair[1]), "candidate")
			defer child.Close()
			conn, err := net.FileConn(parent)
			if err = errors.Join(err, parent.Close()); err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "takeover", "candidate")
			cmd.Env = append(environment(home), candidateFDEnv+"=3")
			cmd.ExtraFiles = []*os.File{child}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			if err = child.Close(); err != nil {
				t.Fatal(err)
			}
			h := process(t, cmd.Process.Pid)
			if err = json.NewEncoder(conn).Encode(candidateMessage{Kind: "start", Record: r}); err != nil {
				t.Fatal(err)
			}
			var ready candidateMessage
			if err = json.NewDecoder(conn).Decode(&ready); err != nil {
				t.Fatal(err)
			}
			if ready.Kind != "ready" || ready.Identity.PID != cmd.Process.Pid || ready.StoreID != r.StoreID || ready.Epoch != r.Epoch {
				t.Fatal(ready)
			}
			if published {
				r.Phase, r.Holder = "active", &ready.Identity
				if err = ownership.Publish(path, r, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
			if published {
				client, err := net.DialTimeout("unix", ControlPath(home+"/state"), 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				answer, err := hook.RequestGuard(ctx, client, hook.Object{}, hook.GuardOptions{Root: home + "/markers", Mode: hook.Observe})
				_ = client.Close()
				if err != nil || get(answer, "decision") != "release" || h.Wait(0) {
					t.Fatal(answer, err)
				}
				if !h.Send(unix.SIGINT) {
					t.Fatal(h.Detail)
				}
			}
			if !h.Wait(5 * time.Second) {
				t.Fatal("candidate outlived controller EOF without active publication")
			}
			scope := ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
			for _, lockPath := range []string{home + "/state/daemon.lock", scope.path(home+"/socket", ".lock"), home + "/state/write-gate.lock"} {
				f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
				if err = errors.Join(err, f.Close()); err != nil {
					t.Fatal("candidate retained lock", lockPath, err)
				}
			}
			t.Logf("CLEANUP candidate pid=%d exited; daemon/scope/write-gate locks released", cmd.Process.Pid)
		})
	}
}
