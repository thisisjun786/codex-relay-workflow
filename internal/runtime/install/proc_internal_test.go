package install

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

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
	Status  string // its status file ("" leaves none, "!" one this user may not read)
}

// ProcStatus is a /proc/<pid>/status whose real, effective, saved and filesystem uids are uids (one
// uid stands for all four) and whose gid and supplementary groups are groups, holding no
// capability.
func ProcStatus(uids []int, groups ...int) string {
	for len(uids) < 4 {
		uids = append(uids, uids[len(uids)-1])
	}
	gid := 1 << 30
	if len(groups) > 0 {
		gid = groups[0]
	}
	list := ""
	for _, g := range groups {
		list += fmt.Sprintf("%d ", g)
	}
	return fmt.Sprintf("Name:\tpython3\nUid:\t%d\t%d\t%d\t%d\nGid:\t%d\t%d\t%d\t%d\nGroups:\t%s\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\n",
		uids[0], uids[1], uids[2], uids[3], gid, gid, gid, gid, list)
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
		if p.Status != "" {
			body, mode := []byte(p.Status), os.FileMode(0o444)
			if p.Status == "!" {
				mode = 0
			}
			if err := os.WriteFile(filepath.Join(dir, "status"), body, mode); err != nil {
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
// fails for another reason, or whose cmdline cannot be read, is not ruled out. A process whose
// working directory cannot be read and whose argv runs something relative to it is not ruled out
// either, whatever its uid, unless it is another user's, not root's, and a directory from / down
// to the runtime is closed to every uid it holds by its mode, owner and group (todo 40 review:
// such a working directory may itself be inside, whatever the relative path names).
func TestLiveProcessesRuleOutOnlyWhatTheyRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so an unreadable one cannot be made")
	}
	// The gate is the directory the runtime lies in: closed (0700) to every other user unless a
	// case opens it. Everything above it is the temporary directory's, and must let anyone search
	// it for the cases that open the gate.
	gate, err := os.MkdirTemp("", "crw-proc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gate, 0o700); os.RemoveAll(gate) })
	aboveOpen := true
	for p := filepath.Dir(gate); ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm()&0o001 == 0 {
			aboveOpen = false
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	info, err := os.Stat(gate)
	if err != nil {
		t.Fatal(err)
	}
	gateGroup := int(info.Sys().(*syscall.Stat_t).Gid)
	directory := filepath.Join(gate, "bin-0.9.0-000000000000")
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
	const other, root = 5000, 9000 // pids from other belong to another user, from root to root
	me, stranger := os.Getuid(), os.Getuid()+1
	restore := ReplaceProcessOwner(func(dir string) (int, error) {
		pid, _ := strconv.Atoi(filepath.Base(dir))
		switch {
		case pid == 101:
			return 0, os.ErrNotExist // gone between the listing and the reading
		case pid >= root:
			return 0, nil
		case pid >= other:
			return stranger, nil
		}
		return me, nil
	})
	defer restore()
	agent := Argv("/bin/sh", "-e", "bin/agent.sh", "--run")
	strangers := ProcStatus([]int{stranger})
	capable := strings.Replace(strangers, "CapPrm:\t0000000000000000", "CapPrm:\t0000000000000004", 1)
	groupless := strings.Replace(strangers, "Groups:\t\n", "", 1)
	for _, c := range []struct {
		label   string
		process FakeProcess
		found   bool
		unruled bool
		gate    os.FileMode // the gate's mode for this case; 0 leaves it 0700
		acl     bool        // an ACL on the gate lets the stranger search it
	}{
		{"a pid whose entries are gone", FakeProcess{Pid: 100}, false, false, 0, false},
		{"a pid whose directory is gone", FakeProcess{Pid: 101, Exe: crw, Cmdline: Argv(crw)}, false, false, 0, false},
		{"this user's process running the runtime", FakeProcess{Pid: 200, Exe: crw, Cmdline: Argv("crw")}, true, false, 0, false},
		{"this user's process running something else", FakeProcess{Pid: 201, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep", "30")}, false, false, 0, false},
		{"this user's process whose exe fails otherwise", FakeProcess{Pid: 300, Exe: "!", Cmdline: Argv("/usr/bin/sleep", "30")}, false, true, 0, false},
		{"this user's process with an unreadable cmdline", FakeProcess{Pid: 301, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep"), Hidden: true}, false, true, 0, false},
		{"this user's capability-holding process (exe denied)", FakeProcess{Pid: 302, Exe: "denied", Cmdline: Argv("/usr/lib/systemd/systemd", "--user")}, false, false, 0, false},
		{"this user's denied process whose cmdline starts the runtime", FakeProcess{Pid: 303, Exe: "denied", Cmdline: Argv(filepath.Join(directory, "bin", "codex-session-relay"))}, true, false, 0, false},
		{"another user's process whose cmdline starts the runtime", FakeProcess{Pid: other, Exe: "denied", Cmdline: Argv(filepath.Join(directory, "bin", "codex-session-relay"), "service", "run")}, true, false, 0, false},
		{"another user's interpreter running a script inside", FakeProcess{Pid: other + 1, Exe: "denied", Cmdline: Argv("/usr/bin/python3", filepath.Join(directory, "bin", "codex-thread-bridge"))}, true, false, 0, false},
		{"another user's process that names nothing inside", FakeProcess{Pid: other + 2, Exe: "denied", Cmdline: Argv("/usr/sbin/sshd", "-D")}, false, false, 0, false},
		{"another user's process whose exe fails otherwise", FakeProcess{Pid: other + 5, Exe: "!", Cmdline: Argv("/usr/sbin/sshd", "-D")}, false, false, 0, false},
		{"another user's kernel thread", FakeProcess{Pid: other + 3, Exe: "denied", Cmdline: []byte{}}, false, false, 0, false},
		{"another user's process under hidepid", FakeProcess{Pid: other + 4, Exe: "denied", Cmdline: Argv("x"), Hidden: true}, false, true, 0, false},
		{"a denied process starting a runtime name bare", FakeProcess{Pid: other + 6, Exe: "denied", Cmdline: Argv("codex-session-relay", "service", "run")}, false, true, 0, false},
		// Relative operands are opened against the process's working directory.
		{"a script named relative to the working directory", FakeProcess{Pid: 400, Exe: "/usr/bin/bash", Cmdline: Argv("/usr/bin/bash", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false, 0, false},
		{"a script after interpreter options", FakeProcess{Pid: 401, Exe: "/usr/bin/bash", Cmdline: Argv("bash", "-e", "-x", "-o", "pipefail", "+u", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false, 0, false},
		{"a script env runs", FakeProcess{Pid: 402, Exe: "/usr/bin/env", Cmdline: Argv("/usr/bin/env", "-i", "LANG=C", "sh", "./"+filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false, 0, false},
		{"a shell script run relative", FakeProcess{Pid: 403, Exe: "/usr/bin/bash", Cmdline: Argv("bash", "-e", "-o", "pipefail", filepath.Join(filepath.Base(directory), "run.sh")), Cwd: filepath.Dir(directory)}, true, false, 0, false},
		{"a relative program whose executable and working directory are denied", FakeProcess{Pid: other + 11, Exe: "denied", Cmdline: Argv(filepath.Join(filepath.Base(directory), "bin", "codex-session-relay"), "service"), Cwd: "denied", Status: strangers}, false, true, 0o755, false},
		{"a process title another user wrote over its argv", FakeProcess{Pid: other + 10, Exe: "denied", Cmdline: Argv("sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups"), Cwd: "denied"}, false, false, 0, false},
		{"an interpreter known by its executable", FakeProcess{Pid: 410, Exe: "/usr/bin/bash", Cmdline: Argv("my-daemon", filepath.Join(filepath.Base(directory), "bin", "codex-thread-bridge")), Cwd: filepath.Dir(directory)}, true, false, 0, false},
		{"a Python program run relative, which a Go runtime holds none of", FakeProcess{Pid: 411, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-u", "bin/WALinuxAgent-2.16.0.2-py3.12.egg", "-run-exthandlers"), Cwd: "denied"}, false, false, 0, false},
		{"an argument that is no operand", FakeProcess{Pid: 409, Exe: "/usr/bin/grep", Cmdline: Argv("grep", "-r", "x", filepath.Base(directory)), Cwd: filepath.Dir(directory)}, false, false, 0, false},
		{"a module run with the working directory inside", FakeProcess{Pid: 405, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-m", "codex_thread_bridge"), Cwd: directory}, true, false, 0, false},
		{"an inline program outside", FakeProcess{Pid: 406, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "-c", "import bin"), Cwd: "/"}, false, false, 0, false},
		{"a relative script outside", FakeProcess{Pid: 407, Exe: "/usr/bin/python3", Cmdline: Argv("python3", "bin/codex-thread-bridge"), Cwd: "/tmp"}, false, false, 0, false},
		{"this user's process whose working directory fails otherwise", FakeProcess{Pid: 408, Exe: "/usr/bin/sleep", Cmdline: Argv("sleep", "30"), Cwd: "!"}, false, true, 0, false},
		{"another user's relative script, its working directory denied and the runtime open to it", FakeProcess{Pid: other + 7, Exe: "denied", Cmdline: Argv("/usr/bin/sh", "bin/codex-thread-bridge"), Cwd: "denied", Status: strangers}, false, true, 0o755, false},
		// A working directory the kernel hides may itself be inside, whatever a relative path
		// names: only a directory on the way in closed to every uid the process holds rules out
		// another user's, never root's, and what cannot be read rules out nothing.
		{"root's agent run relative", FakeProcess{Pid: root, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: ProcStatus([]int{0})}, false, true, 0, false},
		{"another user's agent, the runtime closed to it", FakeProcess{Pid: other + 12, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: strangers}, false, false, 0, false},
		{"another user's agent, the runtime open to it", FakeProcess{Pid: other + 13, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: strangers}, false, true, 0o755, false},
		{"another user's agent, the runtime open to a group it holds", FakeProcess{Pid: other + 14, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: ProcStatus([]int{stranger}, 1<<30, gateGroup)}, false, true, 0o710, false},
		{"another user's agent, the runtime open to a group it does not hold", FakeProcess{Pid: other + 15, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: strangers}, false, false, 0o710, false},
		{"another user's agent whose saved uid is this user's", FakeProcess{Pid: other + 16, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: ProcStatus([]int{stranger, stranger, me, stranger})}, false, true, 0, false},
		{"another user's agent that may become root", FakeProcess{Pid: other + 17, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: ProcStatus([]int{stranger, stranger, 0, stranger})}, false, true, 0, false},
		{"another user's agent holding CAP_DAC_READ_SEARCH", FakeProcess{Pid: other + 19, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: capable}, false, true, 0, false},
		{"another user's agent whose status cannot be read", FakeProcess{Pid: other + 20, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: "!"}, false, true, 0, false},
		{"another user's agent whose status names no groups", FakeProcess{Pid: other + 21, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: groupless}, false, true, 0, false},
		{"another user's agent, the runtime closed by its mode and opened by an ACL", FakeProcess{Pid: other + 22, Exe: "denied", Cmdline: agent, Cwd: "denied", Status: strangers}, false, true, 0, true},
		{"another user's agent, its working directory failing otherwise", FakeProcess{Pid: other + 18, Exe: "denied", Cmdline: agent, Cwd: "!", Status: strangers}, false, true, 0, false},
		{"this user's agent, its working directory denied", FakeProcess{Pid: 304, Exe: "denied", Cmdline: agent, Cwd: "denied"}, false, true, 0, false},
		{"another user's absolute script, its working directory denied", FakeProcess{Pid: other + 8, Exe: "denied", Cmdline: Argv("/usr/bin/python3", "/usr/bin/networkd-dispatcher"), Cwd: "denied"}, false, false, 0, false},
		{"another user's bare program, its working directory denied", FakeProcess{Pid: other + 9, Exe: "denied", Cmdline: Argv("sshd", "-D"), Cwd: "denied"}, false, false, 0, false},
	} {
		if (c.gate != 0 || c.acl) && c.unruled && !aboveOpen {
			t.Logf("%s: skipped, a directory above %s does not let every user search it", c.label, gate)
			continue
		}
		mode := c.gate
		if mode == 0 {
			mode = 0o700
		}
		if err := os.Chmod(gate, mode); err != nil {
			t.Fatal(err)
		}
		if c.acl {
			if err := unix.Lsetxattr(gate, "system.posix_acl_access", searchACL(stranger), 0); err != nil {
				t.Logf("%s: skipped, an ACL cannot be set here: %v", c.label, err)
				continue
			}
		}
		found, unruled, err := liveProcesses(FakeProc(t, c.process), mustIdentify(gate, filepath.Base(directory)))
		if c.acl {
			if err := unix.Lremovexattr(gate, "system.posix_acl_access"); err != nil && !errors.Is(err, unix.ENODATA) {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(gate, 0o700); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		if (len(found) > 0) != c.found || (len(unruled) > 0) != c.unruled {
			t.Errorf("%s: found %s, not ruled out %s", c.label, golden.Canon(found), golden.Canon(unruled))
			continue
		}
		if c.unruled && (record.Get(golden.Obj(unruled[0]), "pid") != int64(c.process.Pid) || record.Get(golden.Obj(unruled[0]), "why") == nil) {
			t.Errorf("%s: %s", c.label, golden.Canon(unruled))
		}
	}
}

// searchACL is a POSIX access ACL (the system.posix_acl_access encoding) giving its owner every
// permission and uid search alone, and nobody else anything.
func searchACL(uid int) []byte {
	const undefined = ^uint32(0)
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{{0x01, 7, undefined}, {0x02, 1, uint32(uid)}, {0x04, 0, undefined}, {0x10, 1, undefined}, {0x20, 0, undefined}}
	out := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range entries {
		out = binary.LittleEndian.AppendUint16(out, e.tag)
		out = binary.LittleEndian.AppendUint16(out, e.perm)
		out = binary.LittleEndian.AppendUint32(out, e.id)
	}
	return out
}
