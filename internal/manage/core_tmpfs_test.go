package manage

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// The memory-backed temporary root of the package's tests (CRW-1262, after CRW-1259).
//
// Nearly every test builds a state directory, a store or a repository, and the code under test syncs them: a run makes about 16,000
// fsyncs, and on a loaded host each one waits behind the other writers of the disk (the package took 591 s once, 229 s of it in one
// test). The tests assert no durability a sync gives, so they run in a tmpfs root (/dev/shm) when one is fit, and the production store
// is untouched. A root is fit only if it has room (bytes and inodes), and a script can run from it; otherwise the tests stay on the
// TMPDIR the caller set. Two regressions made CRW-1259 drop this route, and each has a test (core_tmpfs_cases_test.go):
//
//   - a test binary started again by a test (a supervisor child, any helper that goes through TestMain) made a root of its own and
//     left it. The process that makes a root names it in tmpfsRootEnv ("<pid>:<root>"); a process that finds a live owner's root there
//     uses it, makes none and removes none, and the owner removes the root after m.Run, whatever the helpers did. A process that chose no
//     tmpfs says so ("<pid>:-") and its helpers choose none either;
//   - one small exec probe did not reject a small or full tmpfs, and then every fixture write failed. The root must have tmpfsFloorBytes
//     and tmpfsFloorInodes free as well as run a script.
//
// A run that is killed (a go test timeout, SIGKILL) cannot remove its root, and the root is memory: the next run removes the roots whose
// owner process is gone.

const (
	// tmpfsRootEnv names the root of the run: "<pid of the process that made it>:<root>". It is inherited by every process a test starts.
	tmpfsRootEnv = "CRW_MANAGE_TEST_TMPFS_ROOT"
	// tmpfsBaseEnv names the directory the root is made in, for the tests of this file; empty is /dev/shm.
	tmpfsBaseEnv    = "CRW_MANAGE_TEST_TMPFS_BASE"
	tmpfsRootPrefix = "crw-manage-tmp-"
	// tmpfsFloorBytes and tmpfsFloorInodes are what the base must have free: a run of the package keeps well under a quarter of the bytes.
	tmpfsDeclined    = "-" // the root part of a marker whose process chose no tmpfs
	tmpfsFloorBytes  = 2 << 30
	tmpfsFloorInodes = 20000
)

// tmpfsHost is what the choice reads from the machine, so a test can name a small, full or noexec tmpfs.
type tmpfsHost struct {
	Free  func(dir string) (bytes, inodes uint64, err error) // free to an unprivileged user
	Probe func(dir string) error                             // nil if a script written in dir runs
	Alive func(pid int) bool
}

func tmpfsRealHost() tmpfsHost {
	return tmpfsHost{
		Free: func(dir string) (uint64, uint64, error) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(dir, &st); err != nil {
				return 0, 0, err
			}
			return st.Bavail * uint64(st.Bsize), st.Ffree, nil
		},
		Probe: func(dir string) error {
			script := filepath.Join(dir, "probe.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				return err
			}
			return exec.Command(script).Run()
		},
		Alive: func(pid int) bool {
			err := syscall.Kill(pid, 0)
			return err == nil || err == syscall.EPERM
		},
	}
}

// tmpfsRoot is the root this process runs its tests in, or "" for the caller's TMPDIR. owner is true if this call made the root, and
// then the caller removes it; a root that a live owner named in the environment is used as it is and never removed by this process.
func tmpfsRoot(base string, h tmpfsHost, getenv func(string) string) (root string, owner bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	if tmpfsDeclinedByOwner(h, getenv(tmpfsRootEnv)) {
		return "", false
	}
	if named, ok := tmpfsNamedRoot(h, getenv(tmpfsRootEnv)); ok {
		return named, false
	}
	bytes, inodes, err := h.Free(base)
	if err != nil || bytes < tmpfsFloorBytes || inodes < tmpfsFloorInodes {
		return "", false
	}
	tmpfsSweep(base, h)
	// The name is the prefix and the pid and nothing else. The fake hosts of the tests bind their unix sockets below tmploc.SocketRoot,
	// never under TMPDIR (CRW-1568), so this root's length bounds no socket path. A root of that name that is already there is not ours
	// to replace.
	root = filepath.Join(base, tmpfsRootPrefix+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(root, 0o700); err != nil {
		return "", false
	}
	if err := h.Probe(root); err != nil {
		_ = os.RemoveAll(root)
		return "", false
	}
	return root, true
}

// tmpfsDeclinedMarker is the marker of a process that chose no tmpfs root: "<pid>:-". A helper it starts again follows the choice and does
// not look again, as it would make a root of its own and leave it through os.Exit.
func tmpfsDeclinedMarker(pid int) string { return strconv.Itoa(pid) + ":" + tmpfsDeclined }

// tmpfsDeclinedByOwner is true if marker is "<pid>:-" of a live process.
func tmpfsDeclinedByOwner(h tmpfsHost, marker string) bool {
	pidText, rest, found := strings.Cut(marker, ":")
	pid, err := strconv.Atoi(pidText)
	return found && rest == tmpfsDeclined && err == nil && pid > 0 && h.Alive(pid)
}

// tmpfsNamedRoot is the root of a marker "<pid>:<root>" whose owner is alive and whose root is still a directory of ours.
func tmpfsNamedRoot(h tmpfsHost, marker string) (string, bool) {
	pidText, root, found := strings.Cut(marker, ":")
	pid, err := strconv.Atoi(pidText)
	if !found || err != nil || pid <= 0 || !filepath.IsAbs(root) || filepath.Base(root) != tmpfsRootPrefix+pidText || !h.Alive(pid) {
		return "", false
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return "", false
	}
	return root, true
}

// tmpfsSweep removes the roots of base that carry the prefix and the number of a process that is gone (a run that was killed).
func tmpfsSweep(base string, h tmpfsHost) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), tmpfsRootPrefix))
		if !strings.HasPrefix(entry.Name(), tmpfsRootPrefix) || err != nil || pid <= 0 || !entry.IsDir() || h.Alive(pid) {
			continue
		}
		if info, err := entry.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
				_ = os.RemoveAll(filepath.Join(base, entry.Name()))
			}
		}
	}
}
