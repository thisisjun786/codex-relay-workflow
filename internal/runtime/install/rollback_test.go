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

func needsPython(t *testing.T) {
	t.Helper()
	if hostPython() == "" {
		t.Skip("python3 runs the packaged Stop launcher and the venv's Python Stop adapter")
	}
}

// stopRecorded runs the plugin's declared Stop command as a cached turn runs it (the packaged
// launcher, with only HOME, CODEX_HOME, the plugin root and PATH) and answers whether the
// adapter the settings name through the pointer journaled the Stop.
func (h *host) stopRecorded(t *testing.T) bool {
	t.Helper()
	journal := filepath.Join(h.home, "journal")
	before := journalRows(t, journal)
	cmd := exec.Command("sh", "-c", declaredStopCommand(t))
	cmd.Env = h.launcherEnv(filepath.Join(golden.Root(), "plugins", "crw"))
	cmd.Stdin = strings.NewReader(stopPayload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the declared Stop command: %v\n%s", err, stderr.String())
	}
	return journalRows(t, journal) == before+1
}

// goEraAfterPythonEra is the relay host after its first Go install: the Python venv installed
// and the outgoing selection, the Go runtime selected and named by the pointer, the Python-era
// Stop settings archived and their Go variant in place. It answers the venv, the Go runtime and
// the settings' bytes.
func (h *host) goEraAfterPythonEra(t *testing.T) (string, string, string) {
	t.Helper()
	venv, _ := h.pythonEraHost(t)
	result := h.mustInstall(t, "update", archive(t, "0.9.0", ""))
	if at(result, "settings", "action") != "replaced" {
		t.Fatalf("settings: %s", golden.Canon(at(result, "settings")))
	}
	return venv, h.pointerTarget(t), readFile(t, filepath.Join(h.codex, install.SettingsName))
}

// No window: a rollback to the Python runtime never rewrites the Stop settings, so a Stop is
// recorded at every step of it - while the selection is committed and while the pointer is
// placed (the pointer still on the Go runtime, whose hook the document reaches) and after the
// pointer names the venv (whose crw-completion-hook is the Python Stop adapter, reading the same
// document). The archived Python-era document is never put back.
func TestARollbackToAVenvNeverRewritesTheSettings(t *testing.T) {
	needsPython(t)
	h := newHost(t)
	venv, goRuntime, settings := h.goEraAfterPythonEra(t)
	path := filepath.Join(h.codex, install.SettingsName)
	archives := must(filepath.Glob(path + ".superseded-*"))
	if !h.stopRecorded(t) {
		t.Fatal("a Stop on the Go runtime was not recorded")
	}
	windows := map[string]bool{}
	restoreCommit := install.ReplaceSelectionCommit(func(recordPath string, version int, delta record.Delta) (reading.Reading, error) {
		windows["commit"] = h.stopRecorded(t)
		if h.pointerTarget(t) != goRuntime || readFile(t, path) != settings {
			t.Errorf("at the commit the pointer names %s and the settings read\n%s", h.pointerTarget(t), readFile(t, path))
		}
		return record.Update(recordPath, version, delta)
	})
	restorePlace := install.ReplacePointerPlacement(func(pointerPath, target string) error {
		windows["placement"] = h.stopRecorded(t)
		return pointer.Place(pointerPath, target)
	})
	result, code := install.Rollback(context.Background(), h.options(), venv)
	restorePlace()
	restoreCommit()
	if !windows["commit"] || !windows["placement"] {
		t.Fatalf("a Stop during the rollback reached no adapter: %v", windows)
	}
	if code != install.OK || h.pointerTarget(t) != venv || at(result, "settings", "action") != "none" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if readFile(t, path) != settings || len(must(filepath.Glob(path+".superseded-*"))) != len(archives) {
		t.Fatalf("the rollback rewrote the settings:\n%s", readFile(t, path))
	}
	if !h.stopRecorded(t) {
		t.Fatal("a Stop through the venv's Python adapter was not recorded")
	}
}

// A rollback killed with SIGKILL at its commit - after every settings step a rollback takes and
// before the selection and the pointer move - leaves a host whose Stops are recorded: the
// settings were never rewritten, the pointer still names the Go runtime, and the ordinary
// rerun finishes the rollback.
func TestARollbackKilledAtItsCommitLeavesStopsRecorded(t *testing.T) {
	if spec := os.Getenv("CRW_TEST_KILLED_ROLLBACK"); spec != "" {
		killedRollback(t, spec)
		return
	}
	needsPython(t)
	h := newHost(t)
	venv, goRuntime, settings := h.goEraAfterPythonEra(t)
	spec, err := json.Marshal(map[string]any{"home": h.home, "dest": h.dest, "codex": h.codex, "state": h.state, "record": h.record,
		"relayState": h.relayState, "socket": h.fake.SocketPath, "env": []string(h.env), "venv": venv})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestARollbackKilledAtItsCommitLeavesStopsRecorded$", "-test.count=1")
	child.Env = append(os.Environ(), "CRW_TEST_KILLED_ROLLBACK="+string(spec))
	out, err := child.CombinedOutput()
	if status, ok := child.ProcessState.Sys().(syscall.WaitStatus); err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the rollback was not killed at its commit: %v\n%s", err, out)
	}
	if !h.stopRecorded(t) {
		t.Fatal("after the kill a Stop reached no adapter")
	}
	if h.pointerTarget(t) != goRuntime || readFile(t, filepath.Join(h.codex, install.SettingsName)) != settings {
		t.Fatalf("after the kill the pointer names %s and the settings changed", h.pointerTarget(t))
	}
	if result, code := install.Rollback(context.Background(), h.options(), venv); code != install.OK || h.pointerTarget(t) != venv {
		t.Fatalf("the rerun: exit %d\n%s", code, golden.Canon(result))
	}
	if !h.stopRecorded(t) {
		t.Fatal("after the rerun a Stop through the venv reached no adapter")
	}
}

// killedRollback is the child of TestARollbackKilledAtItsCommitLeavesStopsRecorded: it rolls the
// host back to the venv and kills itself at the commit.
func killedRollback(t *testing.T, spec string) {
	var given struct {
		Home, Dest, Codex, State, Record, RelayState, Socket, Venv string
		Env                                                        []string
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
	result, code := install.Rollback(context.Background(), o, given.Venv)
	t.Fatalf("the rollback returned instead of reaching its commit: exit %d\n%s", code, golden.Canon(result))
}

// The one document needs no archive to roll back with: on a host whose settings `crw install
// hook` wrote (no Python-era document was ever archived) a rollback to the venv is allowed and
// the venv's Python adapter records the Stop. A venv that could not serve the document - no
// crw-completion-hook console script - is refused with the repair named, and nothing moves.
func TestARollbackToAVenvNeedsNoArchive(t *testing.T) {
	needsPython(t)
	h := newHost(t)
	venv := h.pythonVenv(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	goRuntime := h.pointerTarget(t)
	if _, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK {
		t.Fatal("hook settings")
	}
	path := filepath.Join(h.codex, install.SettingsName)
	settings := readFile(t, path)

	result, code := install.Rollback(context.Background(), h.options(), venv)
	if code != install.OK || h.pointerTarget(t) != venv || readFile(t, path) != settings || len(must(filepath.Glob(path+".superseded-*"))) != 0 {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if !h.stopRecorded(t) {
		t.Fatal("a Stop through the venv's Python adapter was not recorded")
	}
	if forward, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != goRuntime {
		t.Fatalf("forward: exit %d\n%s", code, golden.Canon(forward))
	}

	hookScript := filepath.Join(venv, "bin", "crw-completion-hook")
	if err := os.Remove(hookScript); err != nil {
		t.Fatal(err)
	}
	refused, code := install.Rollback(context.Background(), h.options(), venv)
	if code != install.Refused || h.pointerTarget(t) != goRuntime || readFile(t, path) != settings || !strings.Contains(text(at(refused, "refused")), hookScript) {
		t.Fatalf("a venv without the Python adapter's console script: exit %d\n%s", code, golden.Canon(refused))
	}
}

// A rollback never reverts a host fact: the operator's hold-mode settings, written on the Go
// host after the Python-era observe document was archived, are what the host has after a
// rollback to the venv and after rolling forward again.
func TestARollbackKeepsTheHostFactsTheSettingsRecord(t *testing.T) {
	h := newHost(t)
	venv, _, _ := h.goEraAfterPythonEra(t)
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
	if back, code := install.Rollback(context.Background(), h.options(), venv); code != install.OK || readFile(t, path) != settings {
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
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "'STAGING'") {
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
	// A rollback that does move the pointer is still held by the same daemon.
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

// A rollback points the pointer only at a runtime that can be launched as it stands: a venv
// whose interpreter link names nothing (an OS Python upgrade), whose Stop adapter script is not
// the Python Stop adapter, or whose relay package is from before the fence; a Go runtime whose
// compatibility link is gone. Each is refused with what is wrong, and nothing moves.
func TestARollbackRefusesARuntimeThatCannotBeLaunched(t *testing.T) {
	for name, tc := range map[string]struct {
		venv  bool
		spoil func(t *testing.T, target string) string
	}{
		"a venv whose python names nothing": {venv: true, spoil: func(t *testing.T, venv string) string {
			python := filepath.Join(venv, "bin", "python3")
			if err := os.Remove(python); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(venv, "gone", "python3.12"), python); err != nil {
				t.Fatal(err)
			}
			return python
		}},
		"a venv whose hook script is not the Stop adapter": {venv: true, spoil: func(t *testing.T, venv string) string {
			hookScript := filepath.Join(venv, "bin", "crw-completion-hook")
			executable(t, hookScript, "#!/bin/sh\nexit 0\n")
			return hookScript
		}},
		"a venv whose relay predates the fence": {venv: true, spoil: func(t *testing.T, venv string) string {
			fence := filepath.Join(sitePackages(venv), "codex_session_relay", "ownership.py")
			if err := os.Remove(fence); err != nil {
				t.Fatal(err)
			}
			return "before the fence"
		}},
		"a Go runtime without its bridge link": {spoil: func(t *testing.T, runtime string) string {
			link := filepath.Join(runtime, "bin", "codex-thread-bridge")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
			var target string
			if tc.venv {
				target = h.pythonVenv(t)
				h.mustInstall(t, "install", first)
			} else {
				target = runtimeDir(h, "0.9.0", first, t)
				h.mustInstall(t, "install", first)
				h.mustInstall(t, "update", second)
			}
			named := tc.spoil(t, target)
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
