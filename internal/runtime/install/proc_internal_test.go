package install

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// FakeProcess is one /proc/<pid> entry: Exe is a link target ("" leaves no exe, "denied" one the
// kernel refuses for want of ptrace access, and "!" one unreadable for another reason), Cmdline
// its bytes (nil leaves none), and a Hidden cmdline one this user may not read, as hidepid
// leaves another user's.
type FakeProcess struct {
	Pid     int
	Exe     string
	Cmdline []byte
	Hidden  bool
	Cwd     string // the working directory link: "" is "/", "denied" and "!" as for Exe
}

// FakeProc writes a process table whose processes are pids, and reads their exe as the kernel
// does until the test ends; the caller's owner function says whose each is.
func FakeProc(t *testing.T, processes ...FakeProcess) string {
	t.Helper()
	savedExe, savedCwd := readExe, readCwd
	kernel := func(path string) (string, error) {
		if raw, err := os.ReadFile(path); err == nil && string(raw) == "denied" {
			return "", &os.PathError{Op: "readlink", Path: path, Err: syscall.EACCES}
		}
		return os.Readlink(path)
	}
	readExe, readCwd = kernel, kernel
	t.Cleanup(func() { readExe, readCwd = savedExe, savedCwd })
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range processes {
		dir := filepath.Join(root, strconv.Itoa(p.Pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		switch p.Exe {
		case "":
		case "!", "denied":
			body := []byte(p.Exe)
			if p.Exe == "!" {
				body = nil
			}
			if err := os.WriteFile(filepath.Join(dir, "exe"), body, 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.Symlink(p.Exe, filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
		cwd := filepath.Join(dir, "cwd")
		switch p.Cwd {
		case "":
			if err := os.Symlink("/", cwd); err != nil {
				t.Fatal(err)
			}
		case "!", "denied":
			body := []byte(p.Cwd)
			if p.Cwd == "!" {
				body = nil
			}
			if err := os.WriteFile(cwd, body, 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.Symlink(p.Cwd, cwd); err != nil {
				t.Fatal(err)
			}
		}
		if p.Cmdline != nil {
			mode := os.FileMode(0o444)
			if p.Hidden {
				mode = 0
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), p.Cmdline, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// Argv is a cmdline: each word NUL-terminated.
func Argv(words ...string) []byte {
	var out []byte
	for _, w := range words {
		out = append(append(out, w...), 0)
	}
	return out
}

// A process table is read so that nothing unread passes for absent. A pid that is gone is
// skipped. A process whose exe the kernel refuses (another user's; this user's own holding
// capabilities, as systemd --user does) is judged by its cmdline - the interpreter and the
// script it starts, spelled and resolved - and is not ruled out when that is hidden too or when
// it starts one of this runtime's executables by a bare name. A process of this user whose exe
// fails for another reason, or whose cmdline cannot be read, is not ruled out.
func TestLiveProcessesRuleOutOnlyWhatTheyRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so an unreadable one cannot be made")
	}
	directory := filepath.Join(t.TempDir(), "bin-0.9.0-000000000000")
	if err := os.MkdirAll(filepath.Join(directory, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	crw := filepath.Join(directory, "bin", "crw")
	if err := os.WriteFile(crw, []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("crw", filepath.Join(directory, "bin", "codex-session-relay")); err != nil {
		t.Fatal(err)
	}
	const other = 5000 // pids from here belong to another user
	restore := ReplaceProcessOwner(func(dir string) (int, error) {
		pid, _ := strconv.Atoi(filepath.Base(dir))
		switch {
		case pid == 101:
			return 0, os.ErrNotExist // gone between the listing and the reading
		case pid >= other:
			return os.Getuid() + 1, nil
		}
		return os.Getuid(), nil
	})
	defer restore()
	for _, c := range []struct {
		label   string
		process FakeProcess
		found   bool
		unruled bool
	}{
		{"a pid whose entries are gone", FakeProcess{Pid: 100}, false, false},
		{"a pid whose directory is gone", FakeProcess{Pid: 101, Exe: crw, Cmdline: Argv(crw)}, false, false},
		{"this user's process running the runtime", FakeProcess{Pid: 200, Exe: crw, Cmdline: Argv("crw")}, true, false},
		{"this user's process running something else", FakeProcess{Pid: 201, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep", "30")}, false, false},
		{"this user's process whose exe fails otherwise", FakeProcess{Pid: 300, Exe: "!", Cmdline: Argv("/usr/bin/sleep", "30")}, false, true},
		{"this user's process with an unreadable cmdline", FakeProcess{Pid: 301, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep"), Hidden: true}, false, true},
		{"this user's capability-holding process (exe denied)", FakeProcess{Pid: 302, Exe: "denied", Cmdline: Argv("/usr/lib/systemd/systemd", "--user")}, false, false},
		{"this user's denied process whose cmdline starts the runtime", FakeProcess{Pid: 303, Exe: "denied", Cmdline: Argv(filepath.Join(directory, "bin", "codex-session-relay"))}, true, false},
		{"another user's process whose cmdline starts the runtime", FakeProcess{Pid: other, Exe: "denied", Cmdline: Argv(filepath.Join(directory, "bin", "codex-session-relay"), "service", "run")}, true, false},
		{"another user's interpreter running a script inside", FakeProcess{Pid: other + 1, Exe: "denied", Cmdline: Argv("/usr/bin/python3", filepath.Join(directory, "bin", "codex-thread-bridge"))}, true, false},
		{"another user's process that names nothing inside", FakeProcess{Pid: other + 2, Exe: "denied", Cmdline: Argv("/usr/sbin/sshd", "-D")}, false, false},
		{"another user's process whose exe fails otherwise", FakeProcess{Pid: other + 5, Exe: "!", Cmdline: Argv("/usr/sbin/sshd", "-D")}, false, false},
		{"another user's kernel thread", FakeProcess{Pid: other + 3, Exe: "denied", Cmdline: []byte{}}, false, false},
		{"another user's process under hidepid", FakeProcess{Pid: other + 4, Exe: "denied", Cmdline: Argv("x"), Hidden: true}, false, true},
		{"a denied process starting a runtime name bare", FakeProcess{Pid: other + 6, Exe: "denied", Cmdline: Argv("codex-session-relay", "service", "run")}, false, true},
		// Relative operands are opened against the process's working directory.
		{"a script named relative to the working directory", FakeProcess{Pid: 400, Exe: "/usr/bin/python3", Cmdline: Argv("/usr/bin/python3", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false},
		{"a script after interpreter options", FakeProcess{Pid: 401, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-u", "-Wignore::DeprecationWarning", "-X", "utf8", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false},
		{"a script env runs", FakeProcess{Pid: 402, Exe: "/usr/bin/env", Cmdline: Argv("/usr/bin/env", "-i", "LANG=C", "python3", "./"+filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false},
		{"a shell script run relative", FakeProcess{Pid: 403, Exe: "/usr/bin/bash", Cmdline: Argv("bash", "-e", "-o", "pipefail", filepath.Join(filepath.Base(directory), "run.sh")), Cwd: filepath.Dir(directory)}, true, false},
		{"a relative program whose executable and working directory are denied", FakeProcess{Pid: other + 11, Exe: "denied", Cmdline: Argv(filepath.Join(filepath.Base(directory), "bin", "codex-session-relay"), "service"), Cwd: "denied"}, false, true},
		{"a process title another user wrote over its argv", FakeProcess{Pid: other + 10, Exe: "denied", Cmdline: Argv("sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups"), Cwd: "denied"}, false, false},
		{"an interpreter known by its executable", FakeProcess{Pid: 410, Exe: "/usr/bin/python3", Cmdline: Argv("my-daemon", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false},
		{"an argument that is no operand", FakeProcess{Pid: 409, Exe: "/usr/bin/grep", Cmdline: Argv("grep", "-r", "x", filepath.Base(directory)), Cwd: filepath.Dir(directory)}, false, false},
		{"a module run with the working directory inside", FakeProcess{Pid: 405, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-m", "codex_thread_bridge"), Cwd: directory}, true, false},
		{"an inline program outside", FakeProcess{Pid: 406, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-c", "import bin"), Cwd: "/"}, false, false},
		{"a relative script outside", FakeProcess{Pid: 407, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "bin/codex-thread-bridge"), Cwd: "/tmp"}, false, false},
		{"this user's process whose working directory fails otherwise", FakeProcess{Pid: 408, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep", "30"), Cwd: "!"}, false, true},
		{"another user's relative script, its working directory denied", FakeProcess{Pid: other + 7, Exe: "denied", Cmdline: Argv("/usr/bin/python3", "bin/codex-thread-bridge"), Cwd: "denied"}, false, true},
		{"another user's absolute script, its working directory denied", FakeProcess{Pid: other + 8, Exe: "denied", Cmdline: Argv("/usr/bin/python3", "/usr/bin/networkd-dispatcher"), Cwd: "denied"}, false, false},
		{"another user's bare program, its working directory denied", FakeProcess{Pid: other + 9, Exe: "denied", Cmdline: Argv("sshd", "-D"), Cwd: "denied"}, false, false},
	} {
		found, unruled, err := liveProcesses(FakeProc(t, c.process), mustIdentify(filepath.Dir(directory), filepath.Base(directory)))
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		if (len(found) > 0) != c.found || (len(unruled) > 0) != c.unruled {
			t.Fatalf("%s: found %s, not ruled out %s", c.label, golden.Canon(found), golden.Canon(unruled))
		}
		if c.unruled && record.Get(golden.Obj(unruled[0]), "pid") != int64(c.process.Pid) {
			t.Fatalf("%s: %s", c.label, golden.Canon(unruled))
		}
	}
}
