package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// otherUsers makes every fake pid from 5000 up another user's process, and from 9000 up root's.
func otherUsers(t *testing.T) {
	t.Helper()
	restore := install.ReplaceProcessOwner(func(dir string) (int, error) {
		switch pid, _ := strconv.Atoi(filepath.Base(dir)); {
		case pid >= 9000:
			return 0, nil
		case pid >= 5000:
			return os.Getuid() + 1, nil
		}
		return os.Getuid(), nil
	})
	t.Cleanup(restore)
}

// A process whose command line is hidden (a procfs mounted hidepid) and whose executable is
// another user's cannot be ruled out, so remove refuses, naming its pid, its uid and why, with
// the recovery by hand, and so does the reclaim of an abandoned staging, which shares the rule;
// so does root's process running a script relative to a working directory the kernel hides. A
// table that holds only a pid that is gone, another user's process that names nothing inside,
// and another user's running a relative script from a hidden working directory while the
// runtime lies in a home closed to it does not stop remove.
func TestRemoveAndReclaimRefuseAProcessTheyCannotRuleOut(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so an unreadable one cannot be made")
	}
	otherUsers(t)
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	o := h.options()
	o.Proc = install.FakeProc(t, install.FakeProcess{Pid: 5001, Exe: "denied", Cmdline: install.Argv("x"), Hidden: true})

	refused, code := install.Remove(context.Background(), o, old)
	unruled := golden.List(at(refused, "unreadableProcesses"))
	if code != install.Refused || len(unruled) != 1 || at(golden.Obj(unruled[0]), "pid") != int64(5001) || at(golden.Obj(unruled[0]), "uid") != int64(os.Getuid()+1) ||
		!strings.Contains(text(at(golden.Obj(unruled[0]), "why")), "command line could not be read") ||
		!strings.Contains(text(at(refused, "recoveryRequires")), "remove it by hand once none of the processes named in unreadableProcesses") || !strings.Contains(text(at(refused, "recoveryRequires")), "delete "+old+", then run crw install status") {
		t.Fatalf("remove: exit %d\n%s", code, golden.Canon(refused))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("remove refused and removed")
	}

	unsettle(t, old)
	h.mustInstall(t, "update", archive(t, "0.9.2", "")) // outgoing moves on, so only the process table protects it
	unsettle(t, old)
	kept, code := install.Install(context.Background(), o, "update", install.Source{From: first})
	if code != install.Refused || at(kept, "stagingDecision") != "RECLAIM" || len(golden.List(at(kept, "unreadableProcesses"))) != 1 || !strings.Contains(text(at(kept, "recoveryRequires")), "delete "+old+" by hand and rerun the install") {
		t.Fatalf("reclaim: exit %d\n%s", code, golden.Canon(kept))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("the reclaim refused and removed")
	}
	// Root's process whose working directory is hidden, running a relative script: that working
	// directory may be inside the staging, and no directory is closed to root.
	o.Proc = install.FakeProc(t, install.FakeProcess{Pid: 9003, Exe: "denied", Cmdline: install.Argv("python3", "-u", "bin/WALinuxAgent-2.16.0.2-py3.12.egg", "-run-exthandlers"), Cwd: "denied", Status: install.ProcStatus([]int{0})})
	kept, code = install.Install(context.Background(), o, "update", install.Source{From: first})
	if unruled := golden.List(at(kept, "unreadableProcesses")); code != install.Refused || at(kept, "stagingDecision") != "RECLAIM" || len(unruled) != 1 || at(golden.Obj(unruled[0]), "pid") != int64(9003) || at(golden.Obj(unruled[0]), "uid") != int64(0) {
		t.Fatalf("reclaim, root's hidden working directory: exit %d\n%s", code, golden.Canon(kept))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("the reclaim refused and removed")
	}

	// The home is closed to other users, as a user's home is.
	if err := os.Chmod(h.home, 0o700); err != nil {
		t.Fatal(err)
	}
	o.Proc = install.FakeProc(t,
		install.FakeProcess{Pid: 4000},
		install.FakeProcess{Pid: 5002, Exe: "denied", Cmdline: install.Argv("/usr/sbin/sshd", "-D")},
		install.FakeProcess{Pid: 5004, Exe: "denied", Cmdline: install.Argv("python3", "-u", "bin/crw"), Cwd: "denied", Status: install.ProcStatus([]int{os.Getuid() + 1})})
	if removed, code := install.Remove(context.Background(), o, old); code != install.OK || at(removed, "removed") != true {
		t.Fatalf("a table with nothing unruled: exit %d\n%s", code, golden.Canon(removed))
	}
}

// Removing the runtime the record's outgoing names clears outgoing in the write that drops its
// install entries, so a bare rollback then says there is nothing to return to, rather than
// being sent to a directory that is gone and whose entries are not in the record.
func TestRemovingTheOutgoingRuntimeClearsOutgoing(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	removed, code := install.Remove(context.Background(), h.options(), old)
	if code != install.OK || at(removed, "clearedOutgoing") != true {
		t.Fatalf("remove: exit %d\n%s", code, golden.Canon(removed))
	}
	if _, present := record.Lookup(h.hostRecord(t), "outgoing"); present {
		t.Fatalf("outgoing: %s", golden.Canon(at(h.hostRecord(t), "outgoing")))
	}
	back, code := install.Rollback(context.Background(), h.options(), "")
	if code != install.Refused || !strings.Contains(text(at(back, "refused")), "carries no outgoing selection") {
		t.Fatalf("a bare rollback after removing the outgoing runtime: exit %d\n%s", code, golden.Canon(back))
	}
}
