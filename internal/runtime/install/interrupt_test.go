package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// interruptedWhileHeld runs command with a context that ends at 300 ms while holding is held,
// lets go of holding at 1 s, and answers what command answered: a command that honours its
// context has refused by then, one that does not goes on once the lock frees.
func interruptedWhileHeld(t *testing.T, release func(), command func(context.Context) (record.Object, int)) (record.Object, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	results := make(chan record.Object, 1)
	codes := make(chan int, 1)
	go func() { result, code := command(ctx); results <- result; codes <- code }()
	time.Sleep(time.Second)
	release()
	return <-results, <-codes
}

// A rollback interrupted while it waits for the promotion lock refuses, and when the holder lets
// go nothing has moved: the pointer, the selection and outgoing are as they were.
func TestAnInterruptedRollbackMovesNothing(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	updated := runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	before := readFile(t, h.record)
	_, release := golden.Spawn(t, "", h.record+record.PromotionLockSuffix)
	result, code := interruptedWhileHeld(t, release, func(ctx context.Context) (record.Object, int) {
		return install.Rollback(ctx, h.options(), "")
	})
	if code != install.Refused || !strings.Contains(text(at(result, "refused")), "interrupted") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != updated || readFile(t, h.record) != before {
		t.Fatal("an interrupted rollback moved the pointer or wrote the record")
	}
}

// register-mcp and hook interrupted while another run holds the lock they wait on write nothing:
// the bridge record and the Stop settings are still absent once that run lets go.
func TestInterruptedRegistrationsWriteNothing(t *testing.T) {
	h := newHost(t)
	for label, c := range map[string]struct {
		lock, written string
		command       func(context.Context) (record.Object, int)
	}{
		"register-mcp": {filepath.Join(h.codex, install.OwnershipLockName) + record.LockSuffix, filepath.Join(h.codex, install.BridgeRecordName),
			func(ctx context.Context) (record.Object, int) {
				return install.RegisterMCP(ctx, h.options(), install.RegisterOptions{Owner: install.OwnerPlugin})
			}},
		"hook": {filepath.Join(h.codex, install.SettingsName) + record.LockSuffix, filepath.Join(h.codex, install.SettingsName),
			func(ctx context.Context) (record.Object, int) { return install.Hook(ctx, h.options(), h.hookOptions()) }},
	} {
		write(t, c.lock, "4242")
		result, code := interruptedWhileHeld(t, func() { _ = os.Remove(c.lock) }, c.command)
		if code != install.Refused || !strings.Contains(golden.Canon(result), install.Interrupted) {
			t.Fatalf("%s: exit %d\n%s", label, code, golden.Canon(result))
		}
		if _, err := os.Lstat(c.written); !os.IsNotExist(err) {
			t.Fatalf("%s: an interrupted run wrote %s", label, c.written)
		}
	}
}

// Finishing an interrupted promotion moves the pointer, so it asks what a promotion asks under
// the promotion lock, on the registrations as they stand now: a second owner of the bridge
// registered since the interrupted run is refused, and the pointer stays where it was.
func TestAResumedPromotionAsksForSecondOwners(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, next := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil { // the interrupted move
		t.Fatal(err)
	}
	unsettle(t, next)
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.codex-thread-bridge]\ncommand = \"/opt/elsewhere/bin/codex-thread-bridge\"\n")
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Refused || at(result, "secondOwner") == nil || h.pointerTarget(t) != old {
		t.Fatalf("exit %d, pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(result))
	}
}

// A rollback proves the pointer it placed as a promotion does: when the Go runtime it returns to
// cannot be reached whole through it once placed (here its bin/crw is left unexecutable), the
// pointer, the selection and the settings are put back.
func TestARollbackProvesThePointerItPlaced(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	restore := install.ReplacePointerPlacement(func(path, target string) error {
		if target == old {
			if err := os.Chmod(filepath.Join(target, "bin", "crw"), 0o644); err != nil {
				return err
			}
		}
		return pointer.Place(path, target)
	})
	defer restore()
	result, code := install.Rollback(context.Background(), h.options(), "")
	if code != install.Refused || !strings.Contains(text(at(result, "refused")), "does not reach") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != updated || at(h.hostRecord(t), "selected", "codex-session-relay") != filepath.Join(updated, "bin") {
		t.Fatalf("the pointer or the selection was left on %s", h.pointerTarget(t))
	}
}
