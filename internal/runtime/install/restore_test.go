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

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// goEraHost is a relay host on a Go runtime: 0.8.0 installed, selected and reached through the
// pointer this record placed, and the plugin-owned Stop settings `crw install hook` wrote. It
// answers the runtime and the settings' bytes.
func (h *host) goEraHost(t *testing.T) (string, string) {
	t.Helper()
	first := archive(t, "0.8.0", "")
	h.mustInstall(t, "install", first)
	return runtimeDir(h, "0.8.0", first, t), h.goEraSettings(t)
}

// hostState is everything a failed promotion must leave as it found it: where the pointer
// points, the record's selection, pointer ownership and outgoing (absent is not null) and the
// Stop settings' bytes.
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
	out.WriteString("settings:\n" + readFile(t, filepath.Join(h.codex, install.SettingsName)))
	return out.String()
}

var errNoSpace = &os.PathError{Op: "write", Path: "injected", Err: syscall.ENOSPC}

// A settings write that fails - a full disk, an I/O error, a quota - is a refusal, never an answer
// that the settings were created, and leaves nothing at the path; a write that lands and does not
// read back as written is a refusal too, naming config_applied_unverified.
func TestAFailedSettingsWriteIsARefusal(t *testing.T) {
	path := func(h *host) string { return filepath.Join(h.codex, install.SettingsName) }
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	restore := install.ReplaceSettingsWriter(func(string, []byte) error { return errNoSpace })
	created, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	restore()
	if _, err := os.Lstat(path(h)); code != install.Refused || at(created, "settings", "outcome") != install.ConfigNotWritten || !os.IsNotExist(err) {
		t.Fatalf("a write that fails: exit %d\n%s", code, golden.Canon(created))
	}
	restore = install.ReplaceSettingsWriter(func(target string, text []byte) error {
		if target == path(h) {
			text = bytes.Replace(text, []byte(`"observe"`), []byte(`"hold"`), 1)
		}
		return record.AtomicWrite(target, text)
	})
	landed, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	restore()
	if code != install.Refused || at(landed, "settings", "outcome") != install.ConfigAppliedUnverified || !strings.Contains(readFile(t, path(h)), `"hold"`) {
		t.Fatalf("a write that does not read back: exit %d\n%s", code, golden.Canon(landed))
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A promotion that fails after it checked the Stop settings - the selection cannot be
// committed, the pointer cannot be placed, or it is placed and does not read back as naming the
// candidate - puts back everything it changed: the pointer target, the selection, the pointer's
// ownership entry and outgoing all equal what the run found, and the settings, which no promotion
// rewrites, are the very file it found. Checked for an update and for a rollback to the runtime
// the update replaced.
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
			previous, _ := h.goEraHost(t)
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
			if h.pointerTarget(t) != previous || at(result, "settings", "action") != "none" {
				t.Fatalf("pointer %s, settings %s", h.pointerTarget(t), golden.Canon(at(result, "settings")))
			}
			if _, err := os.Lstat(runtimeDir(h, "0.9.0", candidate, t)); !os.IsNotExist(err) {
				t.Fatal("the candidate was kept")
			}
		})
		t.Run("rollback/"+tc.name, func(t *testing.T) {
			h := newHost(t)
			previous, _ := h.goEraHost(t)
			h.mustInstall(t, "update", archive(t, "0.9.0", ""))
			before := h.hostState(t)
			restore := tc.inject(t)
			result, code := install.Rollback(context.Background(), h.options(), previous)
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
