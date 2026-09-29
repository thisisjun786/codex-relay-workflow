package install_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// pythonEraHost is a relay host before the cutover: the Python venv installed, selected and
// reached through a pointer this record placed, and the plugin-owned Stop settings naming the
// Python adapter through that pointer. It answers the venv and the settings' bytes.
func (h *host) pythonEraHost(t *testing.T) (string, string) {
	t.Helper()
	venv := h.pythonVenv(t)
	var selection []contract.Field
	for _, c := range []struct{ name, module string }{{"codex-session-relay", "codex_session_relay"}, {"codex-thread-bridge", "codex_thread_bridge"}} {
		selection = append(selection, contract.Field{Key: c.name, Value: filepath.Join(sitePackages(venv), c.module)})
	}
	placed := record.Object{{Key: "path", Value: pointer.Path(h.dest)}, {Key: "recordedAt", Value: "2026-09-25T00:40:21Z"}, {Key: "recordedBy", Value: "CRW-116"}}
	if _, err := record.Update(h.record, 1, record.Delta{Select: selection, Pointer: placed}); err != nil {
		t.Fatal(err)
	}
	if err := pointer.Place(pointer.Path(h.dest), venv); err != nil {
		t.Fatal(err)
	}
	return venv, h.pythonEraSettings(t)
}

// hostState is everything a failed promotion must leave as it found it: where the pointer
// points, the record's selection, pointer ownership and outgoing (absent is not null), the Stop
// settings' bytes and every archive beside them, by name and bytes.
func (h *host) hostState(t *testing.T) string {
	t.Helper()
	rec := h.hostRecord(t)
	var out strings.Builder
	out.WriteString("pointer -> " + h.pointerTarget(t) + "\n")
	for _, key := range []string{"selected", "pointer", "outgoing"} {
		value, present := record.Lookup(rec, key)
		if !present {
			out.WriteString(key + ": absent\n")
			continue
		}
		out.WriteString(key + ": " + golden.Canon(value) + "\n")
	}
	settings := filepath.Join(h.codex, install.SettingsName)
	out.WriteString("settings:\n" + readFile(t, settings))
	archives, err := filepath.Glob(settings + ".superseded-*")
	if err != nil {
		t.Fatal(err)
	}
	for _, archived := range archives {
		out.WriteString("archive " + filepath.Base(archived) + ":\n" + readFile(t, archived))
	}
	return out.String()
}

var errNoSpace = &os.PathError{Op: "write", Path: "injected", Err: syscall.ENOSPC}

// A settings write that fails after the Python-era document was set aside - a full disk, an I/O
// error, a quota - leaves that document in place: the install refuses at the settings step,
// the pointer and the selection are untouched, and the settings path holds the Python-era bytes
// with no archive left behind. The archive is a second name for the document rather than a
// move, so at the moment the Go document is written the path still holds the Python-era one.
// A write that lands and does not read back as written means the path now holds bytes this run
// did not write - another writer's save - and those are never removed or overwritten: they stay
// at the path, the Python-era document stays at its archive name, and the answer names both.
// `crw install hook`, the other writer of that replacement, refuses on the same failures
// instead of answering success.
func TestAFailedSettingsWriteKeepsThePythonEraSettings(t *testing.T) {
	settingsPath := func(h *host) string { return filepath.Join(h.codex, install.SettingsName) }
	for name, tc := range map[string]struct {
		fault func(h *host, t *testing.T, original string) func(string, []byte) error
		// landed is what the path holds afterwards when the write landed as another's bytes.
		landed bool
	}{
		"the write fails": {fault: func(h *host, t *testing.T, original string) func(string, []byte) error {
			return func(path string, text []byte) error {
				if path != settingsPath(h) {
					return record.AtomicWrite(path, text)
				}
				if raw, err := os.ReadFile(path); err != nil || string(raw) != original {
					t.Errorf("when the Go settings were written the path held %q (%v), not the Python-era document", raw, err)
				}
				return errNoSpace
			}
		}},
		"the write does not read back": {landed: true, fault: func(h *host, t *testing.T, original string) func(string, []byte) error {
			return func(path string, text []byte) error {
				if path == settingsPath(h) {
					text = bytes.Replace(text, []byte(`"observe"`), []byte(`"hold"`), 1)
				}
				return record.AtomicWrite(path, text)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			venv, original := h.pythonEraHost(t)
			before := h.hostState(t)
			restore := install.ReplaceSettingsWriter(tc.fault(h, t, original))
			result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: archive(t, "0.9.0", "")})
			restore()
			if code != install.Refused || at(result, "failedStep") != "carry the Stop settings to this runtime" {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if h.pointerTarget(t) != venv {
				t.Fatal("the pointer moved")
			}
			archives := must(filepath.Glob(settingsPath(h) + ".superseded-*"))
			if tc.landed {
				// The bytes that landed are not this run's, so they are left, and the document
				// the run found is where the answer says it is.
				if got := readFile(t, settingsPath(h)); !strings.Contains(got, `"hold"`) || len(archives) != 1 || readFile(t, archives[0]) != original ||
					at(result, "settings", "write", "outcome") != install.ConfigAppliedUnverified || at(result, "settings", "write", "retired") != archives[0] {
					t.Fatalf("settings %q archives %v\n%s", got, archives, golden.Canon(at(result, "settings")))
				}
			} else if after := h.hostState(t); after != before {
				t.Fatalf("a failed settings write changed the host:\n%s\nwas\n%s", after, before)
			}

			// The same failure through `crw install hook`, once a Go runtime is what the pointer
			// names: refused, and the Python-era document not lost.
			for _, archived := range archives {
				if err := os.Remove(archived); err != nil {
					t.Fatal(err)
				}
			}
			write(t, settingsPath(h), original)
			h.mustInstall(t, "update", archive(t, "0.9.1", ""))
			write(t, settingsPath(h), original)
			for _, archived := range must(filepath.Glob(settingsPath(h) + ".superseded-*")) {
				if err := os.Remove(archived); err != nil {
					t.Fatal(err)
				}
			}
			restore = install.ReplaceSettingsWriter(tc.fault(h, t, original))
			refused, code := install.Hook(context.Background(), h.options(), h.hookOptions())
			restore()
			archives = must(filepath.Glob(settingsPath(h) + ".superseded-*"))
			switch {
			case tc.landed:
				if code != install.Refused || at(refused, "settings", "outcome") != install.ConfigAppliedUnverified || len(archives) != 1 || readFile(t, archives[0]) != original || !strings.Contains(readFile(t, settingsPath(h)), `"hold"`) {
					t.Fatalf("hook: exit %d\n%s", code, golden.Canon(refused))
				}
			case code != install.Refused || at(refused, "settings", "outcome") != install.ConfigNotWritten:
				t.Fatalf("hook: exit %d\n%s", code, golden.Canon(refused))
			case readFile(t, settingsPath(h)) != original || len(archives) != 0:
				t.Fatal("hook: the Python-era settings were not kept as they were")
			}
			if !tc.landed {
				// And a first write that fails is a refusal too, never an answer that they were created.
				if err := os.Remove(settingsPath(h)); err != nil {
					t.Fatal(err)
				}
				restore = install.ReplaceSettingsWriter(func(string, []byte) error { return errNoSpace })
				created, code := install.Hook(context.Background(), h.options(), h.hookOptions())
				restore()
				if _, err := os.Lstat(settingsPath(h)); code != install.Refused || at(created, "settings", "outcome") != install.ConfigNotWritten || !os.IsNotExist(err) {
					t.Fatalf("hook on no settings: exit %d\n%s", code, golden.Canon(created))
				}
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A promotion that fails after it carried the Stop settings to the new runtime - the selection
// cannot be committed, the pointer cannot be placed, or it is placed and does not read back as
// naming the candidate - puts back everything it changed: the pointer target, the selection,
// the pointer's ownership entry, outgoing and the settings bytes all equal what the run found,
// the settings path is the very file it found (the archive is exchanged back, not copied), and
// no archive is left beside the settings. Checked for an update away from a Python-era host and
// for a rollback from a Go runtime back to that Python runtime, which changes no settings.
func TestAFailedPromotionPutsBackEverythingItChanged(t *testing.T) {
	commitFails := func(t *testing.T) func() {
		return install.ReplaceSelectionCommit(func(string, int, record.Delta) (reading.Reading, error) {
			return reading.Reading{}, errNoSpace
		})
	}
	placementFails := func(t *testing.T) func() {
		return install.ReplacePointerPlacement(func(string, string) error { return errNoSpace })
	}
	placementLandsElsewhere := func(t *testing.T) func() {
		elsewhere := t.TempDir()
		return install.ReplacePointerPlacement(func(path, _ string) error { return pointer.Place(path, elsewhere) })
	}
	for _, tc := range []struct {
		name   string
		inject func(t *testing.T) func()
		step   string
	}{
		{"the commit fails", commitFails, "commit the selection"},
		{"the placement fails", placementFails, "replace the owned pointer"},
		{"the placement does not read back", placementLandsElsewhere, "replace the owned pointer"},
	} {
		t.Run("update/"+tc.name, func(t *testing.T) {
			h := newHost(t)
			venv, _ := h.pythonEraHost(t)
			candidate := archive(t, "0.9.0", "")
			before := h.hostState(t)
			found := must(os.Stat(filepath.Join(h.codex, install.SettingsName)))
			restore := tc.inject(t)
			result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: candidate})
			restore()
			if code != install.Refused || at(result, "failedStep") != tc.step {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if after := h.hostState(t); after != before {
				t.Fatalf("the failed promotion left the host changed:\n%s\nwas\n%s\n%s", after, before, golden.Canon(result))
			}
			if now := must(os.Stat(filepath.Join(h.codex, install.SettingsName))); !os.SameFile(found, now) {
				t.Fatal("the settings path holds a copy of the document it held, not the document itself")
			}
			if h.pointerTarget(t) != venv || at(result, "settings", "undone", "undone") != true {
				t.Fatalf("pointer %s, settings %s", h.pointerTarget(t), golden.Canon(at(result, "settings")))
			}
			if _, err := os.Lstat(runtimeDir(h, "0.9.0", candidate, t)); !os.IsNotExist(err) {
				t.Fatal("the candidate was kept")
			}
		})
		t.Run("rollback/"+tc.name, func(t *testing.T) {
			h := newHost(t)
			venv, _ := h.pythonEraHost(t)
			h.mustInstall(t, "update", archive(t, "0.9.0", ""))
			before := h.hostState(t)
			restore := tc.inject(t)
			result, code := install.Rollback(context.Background(), h.options(), venv)
			restore()
			if code != install.Refused || at(result, "applied") != false || at(result, "settings", "action") != "none" {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if after := h.hostState(t); after != before {
				t.Fatalf("the failed rollback left the host changed:\n%s\nwas\n%s\n%s", after, before, golden.Canon(result))
			}
		})
	}
}

// OPS-4.4 is asked of the selected runtime's own relay: while a relay daemon started from the
// selected runtime holds the store, its service reads itself running, the swap is blocked, the
// candidate is released and the pointer stays where it was.
func TestARunningDaemonOfTheSelectedRuntimeBlocksTheSwap(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	relay := filepath.Join(old, "bin", "codex-session-relay")
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
			break
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
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Refused || at(result, "failedStep") != "read whether it is safe to swap" || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "cells", "daemon", "answer") != "RUNNING" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != old || at(result, "retriable") != true {
		t.Fatalf("pointer %s", h.pointerTarget(t))
	}
	if _, err := os.Lstat(runtimeDir(h, "0.9.1", second, t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the candidate was kept")
	}
}
