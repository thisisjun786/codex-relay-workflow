package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

const stopPayload = `{"session_id": "s-1", "turn_id": "t-1", "transcript_path": "/nonexistent/absent.jsonl", "cwd": "/", "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": "DONE"}`

// nativeStopRecorded runs the Stop command the package declares, `crw hook --plugin-launch`
// through the pointer, and answers whether it journaled the Stop.
func (h *host) nativeStopRecorded(t *testing.T) bool {
	t.Helper()
	return h.stopRecordedBy(t, stopCommandIn(t, wiring("hooks", "stop-recording-completion.json")))
}

func (h *host) stopRecordedBy(t *testing.T, command string) bool {
	t.Helper()
	journal := filepath.Join(h.home, "journal")
	before := len(journalRows(t, journal))
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = h.launcherEnv(preNativePayload(t))
	cmd.Stdin = strings.NewReader(stopPayload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the Stop command %q: %v\n%s", command, err, stderr.String())
	}
	return len(journalRows(t, journal)) == before+1
}

// twoGoRuntimes is a host on its second Go runtime: 0.8.0 installed with the plugin-owned Stop
// settings `crw install hook` writes, then 0.9.0 installed over it. It answers the runtime the
// update replaced, the one the pointer names and the settings' bytes.
func (h *host) twoGoRuntimes(t *testing.T) (string, string, string) {
	t.Helper()
	previous, settings := h.goEraHost(t)
	h.mustInstall(t, "update", archive(t, "0.9.0", ""))
	return previous, h.pointerTarget(t), settings
}

// No window: a rollback never rewrites the Stop settings, so the native Stop command,
// `crw hook --plugin-launch` through the pointer, records a Stop while the selection is committed
// and while the pointer is placed, and once the pointer names the runtime rolled back to.
func TestARollbackNeverRewritesTheSettings(t *testing.T) {
	h := newHost(t)
	previous, current, settings := h.twoGoRuntimes(t)
	path := filepath.Join(h.codex, install.SettingsName)
	if !h.nativeStopRecorded(t) {
		t.Fatalf("the native Stop command was not recorded under the settings\n%s", settings)
	}
	windows := map[string]bool{}
	restoreCommit := install.ReplaceSelectionCommit(func(recordPath string, version int, delta record.Delta) (reading.Reading, error) {
		windows["native commit"] = h.nativeStopRecorded(t)
		if h.pointerTarget(t) != current || readFile(t, path) != settings {
			t.Errorf("at the commit the pointer names %s and the settings read\n%s", h.pointerTarget(t), readFile(t, path))
		}
		return record.Update(recordPath, version, delta)
	})
	restorePlace := install.ReplacePointerPlacement(func(pointerPath, target string) error {
		windows["native placement"] = h.nativeStopRecorded(t)
		return pointer.Place(pointerPath, target)
	})
	result, code := install.Rollback(context.Background(), h.options(), previous)
	restorePlace()
	restoreCommit()
	if !windows["native commit"] || !windows["native placement"] {
		t.Fatalf("a Stop during the rollback reached no adapter: %v", windows)
	}
	if code != install.OK || h.pointerTarget(t) != previous || at(result, "settings", "action") != "none" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if readFile(t, path) != settings {
		t.Fatalf("the rollback rewrote the settings:\n%s", readFile(t, path))
	}
	if !h.nativeStopRecorded(t) {
		t.Fatal("the native Stop command was not recorded on the runtime rolled back to")
	}
}

// A rollback killed with SIGKILL at its commit - after the settings check and before the
// selection and the pointer move - leaves a host whose Stops are recorded: the settings were
// never rewritten, the pointer still names the runtime it named, and the ordinary rerun finishes
// the rollback.
func TestARollbackKilledAtItsCommitLeavesStopsRecorded(t *testing.T) {
	if spec := os.Getenv("CRW_TEST_KILLED_ROLLBACK"); spec != "" {
		killedRollback(t, spec)
		return
	}
	h := newHost(t)
	previous, current, settings := h.twoGoRuntimes(t)
	spec, err := json.Marshal(map[string]any{"home": h.home, "dest": h.dest, "codex": h.codex, "state": h.state, "record": h.record,
		"relayState": h.relayState, "socket": h.fake.SocketPath, "env": []string(h.env), "target": previous})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestARollbackKilledAtItsCommitLeavesStopsRecorded$", "-test.count=1")
	child.Env = append(os.Environ(), "CRW_TEST_KILLED_ROLLBACK="+string(spec))
	out, err := child.CombinedOutput()
	if status, ok := child.ProcessState.Sys().(syscall.WaitStatus); err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the rollback was not killed at its commit: %v\n%s", err, out)
	}
	if !h.nativeStopRecorded(t) {
		t.Fatal("after the kill a Stop was not recorded")
	}
	if h.pointerTarget(t) != current || readFile(t, filepath.Join(h.codex, install.SettingsName)) != settings {
		t.Fatalf("after the kill the pointer names %s and the settings changed", h.pointerTarget(t))
	}
	// The killed run's <env>.crw-lock (the O_EXCL protocol of decision 33) is left behind: a
	// rerun is refused naming it until it is stale, and then finishes the rollback.
	saved := record.LockTimeout
	record.LockTimeout = 200 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	if refused, code := install.Rollback(context.Background(), h.options(), previous); code != install.Refused || !strings.Contains(text(at(refused, "refused")), previous+record.LockSuffix) {
		t.Fatalf("the rerun beside the killed run's lock: exit %d\n%s", code, golden.Canon(refused))
	}
	stale := time.Now().Add(-record.StaleLock - time.Minute)
	if err := os.Chtimes(previous+record.LockSuffix, stale, stale); err != nil {
		t.Fatal(err)
	}
	if result, code := install.Rollback(context.Background(), h.options(), previous); code != install.OK || h.pointerTarget(t) != previous {
		t.Fatalf("the rerun: exit %d\n%s", code, golden.Canon(result))
	}
}

// killedRollback is the child of TestARollbackKilledAtItsCommitLeavesStopsRecorded: it rolls the
// host back to the runtime the update replaced and kills itself at the commit.
func killedRollback(t *testing.T, spec string) {
	var given struct {
		Home, Dest, Codex, State, Record, RelayState, Socket, Target string
		Env                                                          []string
	}
	if err := json.Unmarshal([]byte(spec), &given); err != nil {
		t.Fatal(err)
	}
	o := install.Options{Env: scope.Env(given.Env), Dest: given.Dest, CodexHome: given.Codex, RecordPath: given.Record, Socket: given.Socket, State: given.RelayState,
		Issue: "CRW-158", CodexVersion: codexCli, Now: func() time.Time { return time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) }}
	install.ReplaceSelectionCommit(func(string, int, record.Delta) (reading.Reading, error) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	})
	result, code := install.Rollback(context.Background(), o, given.Target)
	t.Fatalf("the rollback returned instead of reaching its commit: exit %d\n%s", code, golden.Canon(result))
}

// A rollback never reverts a host fact: the operator's hold-mode settings, written on the current
// runtime, are what the host has after a rollback and after rolling forward again.
func TestARollbackKeepsTheHostFactsTheSettingsRecord(t *testing.T) {
	h := newHost(t)
	previous, _, _ := h.twoGoRuntimes(t)
	path := filepath.Join(h.codex, install.SettingsName)
	if err := os.Rename(path, filepath.Join(h.home, "set-aside.json")); err != nil {
		t.Fatal(err)
	}
	held := h.hookOptions()
	held.Mode, held.Isolation = "hold", "CRW-999"
	if result, code := install.Hook(context.Background(), h.options(), held); code != install.OK {
		t.Fatalf("hold: exit %d\n%s", code, golden.Canon(result))
	}
	settings := readFile(t, path)
	if back, code := install.Rollback(context.Background(), h.options(), previous); code != install.OK || readFile(t, path) != settings {
		t.Fatalf("rollback: exit %d, settings\n%s\n%s", code, readFile(t, path), golden.Canon(back))
	}
	if again, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || readFile(t, path) != settings {
		t.Fatalf("forward: exit %d, settings\n%s\n%s", code, readFile(t, path), golden.Canon(again))
	}
}

// A runtime that was promoted and in service but whose claim never settled (an exit 3, or a run
// killed after its pointer moved) is a runtime a rollback returns to: the record's outgoing
// proves a promotion committed it, and the rollback settles its claim as resuming would. The
// runtime a rollback leaves, in service with its claim still STAGING, is settled as well, so a
// later install of its archive keeps it instead of reclaiming it. A STAGING runtime no selection
// accounts for is still refused, naming what its claim says.
func TestARollbackReturnsToAPromotedRuntimeWhoseClaimNeverSettled(t *testing.T) {
	h := newHost(t)
	first, second, third := archive(t, "0.9.0", ""), archive(t, "0.9.1", ""), archive(t, "0.9.2", "")
	a, b := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	write(t, staging.ClaimPath(a), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	h.mustInstall(t, "update", second)
	// The update settles the claim of the in-service runtime it replaces; the state a rollback
	// meets is one it did not settle (a promotion made before it did, or one whose write failed).
	if claimState(t, a) != staging.Complete {
		t.Fatalf("the update left the replaced runtime's claim %v", claimState(t, a))
	}
	write(t, staging.ClaimPath(a), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))

	result, code := install.Rollback(context.Background(), h.options(), "")
	if code != install.OK || h.pointerTarget(t) != a || at(result, "claim", "settled") != true || claimState(t, a) != staging.Complete {
		t.Fatalf("rollback to the unsettled runtime: exit %d\n%s", code, golden.Canon(result))
	}

	write(t, staging.ClaimPath(a), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	left, code := install.Rollback(context.Background(), h.options(), b)
	if code != install.OK || h.pointerTarget(t) != b || at(left, "leftClaim", "settled") != true || claimState(t, a) != staging.Complete {
		t.Fatalf("the runtime a rollback leaves: exit %d\n%s", code, golden.Canon(left))
	}

	h.mustInstall(t, "update", third)
	write(t, staging.ClaimPath(a), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	refused, code := install.Rollback(context.Background(), h.options(), a)
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), `"STAGING"`) {
		t.Fatalf("a STAGING runtime nothing selects: exit %d\n%s", code, golden.Canon(refused))
	}
}

// A rollback to the runtime the pointer already names - the state a run killed between its
// commit and its pointer move leaves - replaces no runtime, so it is not held by the swap gate
// while a relay daemon of that runtime runs: it moves the selection back to what the host
// already reaches.
func TestARollbackThatMovesNoRuntimeAsksNoGate(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, next := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
		t.Fatal(err)
	}
	write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	h.runDaemon(t, filepath.Join(old, "bin", "codex-session-relay"))
	result, code := install.Rollback(context.Background(), h.options(), old)
	if code != install.OK || at(result, "moved") != false || at(result, "swapGate") != nil || h.pointerTarget(t) != old {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if at(h.hostRecord(t), "selected", "codex-session-relay") != filepath.Join(old, "bin") {
		t.Fatalf("selected: %s", golden.Canon(at(h.hostRecord(t), "selected")))
	}
	status, _ := install.Status(context.Background(), h.options())
	if at(status, "runtime", "agrees") != true {
		t.Fatalf("status: %s", golden.Canon(at(status, "runtime")))
	}
	// A rollback that does move the pointer is still held by the same daemon. (The runtime the
	// interrupted promotion committed stays the outgoing selection, so it may be returned to.)
	if refused, code := install.Rollback(context.Background(), h.options(), next); code != install.Refused || at(refused, "swapGate", "verdict") != "BLOCKED" {
		t.Fatalf("a move while the daemon runs: exit %d\n%s", code, golden.Canon(refused))
	}
}

// runDaemon starts the relay daemon from relay against the host's state and waits until its
// service reads itself running; it is stopped when the test ends.
func (h *host) runDaemon(t *testing.T, relay string) {
	t.Helper()
	address := []string{"--socket", h.fake.SocketPath, "--state", h.relayState}
	daemon := exec.Command(relay, append(address, "daemon", "--allow-isolated-scope", "--deadline", strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10))...)
	daemon.Env = h.env
	var daemonOutput bytes.Buffer
	daemon.Stdout, daemon.Stderr = &daemonOutput, &daemonOutput
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = daemon.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = daemon.Process.Kill()
			<-exited
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; {
		status := exec.Command(relay, append(address, "service", "status")...)
		status.Env = h.env
		out, _ := status.Output()
		if strings.Contains(string(out), `"running": true`) {
			return
		}
		select {
		case <-exited:
			t.Fatalf("the daemon exited before it read itself running:\n%s", daemonOutput.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never read itself running: %s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A rollback points the pointer only at a Go runtime that can be launched as it stands: one
// whose compatibility link is gone, and a directory the record lists that holds no bin/crw (a
// Python venv an older installer left, say), are each refused with what is wrong, and nothing
// moves.
func TestARollbackRefusesARuntimeThatCannotBeLaunched(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, h *host, runtime string) (string, string){
		"a Go runtime without its bridge link": func(t *testing.T, h *host, runtime string) (string, string) {
			link := filepath.Join(runtime, "bin", "codex-thread-bridge")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			return runtime, link
		},
		"a recorded directory without bin/crw": func(t *testing.T, h *host, _ string) (string, string) {
			other := filepath.Join(h.dest, "env-1-0be23c258476")
			if err := os.MkdirAll(filepath.Join(other, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, staging.ClaimPath(other), string(record.Encode(staging.NewPayload(staging.Complete, "CRW-116", "1"))))
			var delta record.Delta
			for _, component := range []string{"codex-session-relay", "codex-thread-bridge"} {
				delta.Installs = append(delta.Installs, record.Named{Component: component, Entry: record.GoInstall(other, component, strings.Repeat("0", 64), "by hand", false)})
			}
			if _, err := record.Update(h.record, 1, delta); err != nil {
				t.Fatal(err)
			}
			return other, "is not a Go runtime"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
			h.mustInstall(t, "install", first)
			h.mustInstall(t, "update", second)
			target, named := spoil(t, h, runtimeDir(h, "0.9.0", first, t))
			before := h.pointerTarget(t)
			selected := golden.Canon(at(h.hostRecord(t), "selected"))
			result, code := install.Rollback(context.Background(), h.options(), target)
			if code != install.Refused || !strings.Contains(text(at(result, "refused")), named) {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if h.pointerTarget(t) != before || golden.Canon(at(h.hostRecord(t), "selected")) != selected {
				t.Fatal("a refused rollback moved something")
			}
		})
	}
}

// A named rollback names a runtime directory the record lists, exactly: the destination, its
// parent or / contain runtimes but are none, and are refused rather than read as whichever
// runtime has the newest install entries (which would silently undo the rollback before).
func TestANamedRollbackNamesARuntimeDirectory(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if result, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("rollback: exit %d\n%s", code, golden.Canon(result))
	}
	for _, named := range []string{h.dest, filepath.Dir(h.dest), "/", filepath.Join(old, "bin")} {
		if result, code := install.Rollback(context.Background(), h.options(), named); code != install.Refused || h.pointerTarget(t) != old {
			t.Fatalf("%s: exit %d, pointer %s\n%s", named, code, h.pointerTarget(t), golden.Canon(result))
		}
	}
}
