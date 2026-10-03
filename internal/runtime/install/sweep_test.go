package install_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// Where there is no process table (darwin), an abandoned staging cannot be ruled out of use, so
// the reinstall keeps it - and says what the operator does about a staging that was never
// promoted, not remove's recovery for a runtime a daemon was started from.
func TestReclaimWithoutAProcessTableSaysHowToRecover(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	h.mustInstall(t, "update", archive(t, "0.9.2", ""))
	unsettle(t, old)
	o := h.options()
	o.Proc = t.TempDir() // a process table that is not procfs, as on darwin
	result, code := install.Install(context.Background(), o, "update", install.Source{From: first})
	recovery := text(at(result, "recoveryRequires"))
	if code != install.Refused || at(result, "stagingDecision") != staging.Reclaim || !strings.Contains(text(at(result, "refused")), "no process table") ||
		!strings.Contains(recovery, "never promoted") || !strings.Contains(recovery, "rerun the install") || strings.Contains(recovery, "service stop") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("the staging was removed")
	}
}

// Remove reads the relay's daemon records as the retention scan does (a pid counts while its
// start time and boot id match this process table): one that cannot be read leaves a daemon
// unknown, so the directory is kept. The records are those of the registry the relay resolves in
// the command's environment: with CODEX_SESSION_RELAY_SCOPE_DIR set, that one and no other, so a
// malformed claim in the production registry refuses a remove only where no override is set
// (todo 40 review: IS-8's remove in an isolated home read this machine's live registry, and a
// malformed claim there refused it). Every answer that rests on the process table says what it
// cannot see: another PID namespace, another host; and a remove names the relay records it read.
func TestRemoveReadsTheRelayDaemonRecords(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	daemon := filepath.Join(h.relayState, "daemon.json")
	write(t, daemon, "{not json")
	refused, code := install.Remove(context.Background(), h.options(), old)
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "relay daemon record could not be read") {
		t.Fatalf("an unreadable daemon.json: exit %d\n%s", code, golden.Canon(refused))
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	o := h.options()
	malformed := filepath.Join(o.ScopeRegistry, "0000000000000000.json")
	write(t, malformed, "{not json")
	o.Env = o.Env.Without("CODEX_SESSION_RELAY_SCOPE_DIR")
	refused, code = install.Remove(context.Background(), o, old)
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "relay daemon record could not be read") || !strings.Contains(strings.Join(strList(at(refused, "unreadable")), "\n"), malformed) {
		t.Fatalf("a malformed production claim and no override: exit %d, want a refusal naming it\n%s", code, golden.Canon(refused))
	}
	isolated := filepath.Join(h.home, "isolated-scopes")
	o.Env = o.Env.With("CODEX_SESSION_RELAY_SCOPE_DIR", isolated)
	removed, code := install.Remove(context.Background(), o, old)
	registries, states := strList(at(removed, "relayRecords", "scopeRegistries")), strList(at(removed, "relayRecords", "stateDirectories"))
	if code != install.OK || !strings.Contains(text(at(removed, "processTable")), "another PID namespace") || strings.Join(registries, "|") != isolated ||
		!slices.Contains(states, h.relayState) || !slices.Contains(states, filepath.Join(h.state, "codex-session-relay")) {
		t.Fatalf("the same host under an override: exit %d, want the removal to read %s alone and name it\n%s", code, isolated, golden.Canon(removed))
	}
}

// Every lock wait ends when the command's context does, and nothing destructive follows: a
// remove interrupted while another run holds the promotion lock refuses, and when that run lets
// go the directory and its entries are still there; so does a reinstall interrupted while its
// reclaim waits.
func TestAnInterruptedWaitRemovesNothing(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	h.mustInstall(t, "update", archive(t, "0.9.2", ""))
	for label, run := range map[string]func(context.Context) (record.Object, int){
		"remove": func(ctx context.Context) (record.Object, int) { return install.Remove(ctx, h.options(), old) },
		"reclaim": func(ctx context.Context) (record.Object, int) {
			unsettle(t, old)
			return install.Install(ctx, h.options(), "update", install.Source{From: first})
		},
	} {
		_, release := golden.Spawn(t, "", h.record+record.PromotionLockSuffix)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		done := make(chan record.Object, 1)
		codes := make(chan int, 1)
		start := time.Now()
		go func() { result, code := run(ctx); done <- result; codes <- code }()
		time.Sleep(time.Second)
		release()
		result, code := <-done, <-codes
		cancel()
		if code != install.Refused || !strings.Contains(text(at(result, "refused")), "interrupted") {
			t.Fatalf("%s: exit %d after %v\n%s", label, code, time.Since(start), golden.Canon(result))
		}
		if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil || !listed(h.installsOf(t), old) {
			t.Fatalf("%s: an interrupted run removed the directory or its entries", label)
		}
	}
}

// Every path takes a runtime directory's .crw-lock before the promotion lock, as take() and
// runtime_install.py's resume do: a remove waiting for the directory's lock holds nothing else,
// so it never stalls a promotion elsewhere on the host.
func TestRemoveTakesTheDirectoryLockBeforeThePromotionLock(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	held, err := record.Lock(context.Background(), old, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { _, code := install.Remove(context.Background(), h.options(), old); done <- code }()
	time.Sleep(500 * time.Millisecond)
	state, detail := record.Probe(h.record + record.PromotionLockSuffix)
	held.Release()
	if state == record.Held {
		t.Fatalf("remove holds the promotion lock while it waits for the directory's: %s", detail)
	}
	if code := <-done; code != install.OK {
		t.Fatalf("remove once the directory's lock was free: exit %d", code)
	}
}

// A kill after remove set the directory aside as its tombstone, and before it dropped the
// install entries, leaves the entries naming a directory that is gone and the tombstone. status
// names it, and remove - of the name or of the tombstone - finishes it: the entries and the
// outgoing selection naming it are dropped and the tombstone deleted.
func TestAnInterruptedRemovalIsFinished(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	grave := filepath.Join(h.dest, ".crw-removing-"+filepath.Base(old))
	if err := os.Rename(old, grave); err != nil {
		t.Fatal(err)
	}
	status, _ := install.Status(context.Background(), h.options())
	if interrupted := golden.List(at(status, "interruptedRemovals")); len(interrupted) != 1 || at(golden.Obj(interrupted[0]), "runtime") != old {
		t.Fatalf("status: %s", golden.Canon(at(status, "interruptedRemovals")))
	}
	finished, code := install.Remove(context.Background(), h.options(), old)
	if code != install.OK || at(finished, "finished") != grave || !listed(golden.List(at(finished, "droppedInstallEntries")), old) || at(finished, "clearedOutgoing") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(finished))
	}
	if _, err := os.Lstat(grave); !os.IsNotExist(err) || listed(h.installsOf(t), old) {
		t.Fatal("the tombstone or the entries are left")
	}
}
