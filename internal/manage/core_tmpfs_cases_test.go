package manage

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1262: the memory-backed temporary root of the package's tests (core_tmpfs_test.go). The two regressions that made CRW-1259 drop
// it are pinned here: a re-executed helper made its own root and left it, and one small exec probe did not reject a small or full tmpfs.

const tmpfsChildEnv = "CRW_MANAGE_TEST_TMPFS_CHILD"
const tmpfsResultEnv = "CRW_MANAGE_TEST_TMPFS_RESULT"

// tmpfsLateBaseEnv names a directory the "parent" role of TestTmpfsRootChild makes just before it starts the helper: the base was not
// there (or not fit) when the parent chose, and is fit when the helper starts.
const tmpfsLateBaseEnv = "CRW_MANAGE_TEST_TMPFS_LATE_BASE"

// tmpfsFakeHost is a host with the free space and the exec answer a case names; processes 4242 and only 4242 are alive.
func tmpfsFakeHost(bytes, inodes uint64, probe error) tmpfsHost {
	return tmpfsHost{
		Free:  func(string) (uint64, uint64, error) { return bytes, inodes, nil },
		Probe: func(string) error { return probe },
		Alive: func(pid int) bool { return pid == 4242 },
	}
}

func tmpfsNoEnv(string) string { return "" }

func tmpfsEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestTmpfsRootIsChosenAtTheFloorAndIsTheCallersToRemove(t *testing.T) {
	base := t.TempDir()
	root, owner := tmpfsRoot(base, tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil), tmpfsNoEnv)
	if root == "" || !owner {
		t.Fatalf("root %q owner %v, want a root this call owns", root, owner)
	}
	if want := filepath.Join(base, tmpfsRootPrefix+strconv.Itoa(os.Getpid())); root != want {
		t.Errorf("root %q, want %q", root, want)
	}
	// the fake hosts bind unix sockets under TMPDIR (107 bytes at most, TMPDIR plus 67 for the longest): /dev/shm and this name leave 9 bytes
	if n := len("/dev/shm/") + len(tmpfsRootPrefix) + len(strconv.Itoa(1<<22)); n > 31 {
		t.Errorf("a root under /dev/shm is %d bytes, over the 31 the socket paths of the tests allow", n)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Errorf("root %q: %v", root, err)
	}
}

// A tmpfs the tests cannot use is not chosen, and nothing is left behind in it: a small one (the tests need room for their states and
// repositories), a full one (no bytes or no inodes), and one a script cannot run from.
func TestTmpfsRootRejectsASmallOrFullTmpfs(t *testing.T) {
	cases := map[string]tmpfsHost{
		"one byte under the floor":  tmpfsFakeHost(tmpfsFloorBytes-1, tmpfsFloorInodes, nil),
		"a small tmpfs of 64 MiB":   tmpfsFakeHost(64<<20, 1<<20, nil),
		"a full tmpfs, no bytes":    tmpfsFakeHost(0, 1<<20, nil),
		"a full tmpfs, no inodes":   tmpfsFakeHost(64<<30, 0, nil),
		"one inode under the floor": tmpfsFakeHost(64<<30, tmpfsFloorInodes-1, nil),
		"a mount that forbids exec": tmpfsFakeHost(64<<30, 1<<20, errors.New("fork/exec: permission denied")),
	}
	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			if root, owner := tmpfsRoot(base, host, tmpfsNoEnv); root != "" || owner {
				t.Fatalf("a root %q was chosen (owner %v)", root, owner)
			}
			if left := tmpfsEntries(t, base); len(left) != 0 {
				t.Errorf("the rejected tmpfs was left %v", left)
			}
		})
	}
	t.Run("a root of the same name is there", func(t *testing.T) {
		base := t.TempDir()
		stale := filepath.Join(base, tmpfsRootPrefix+strconv.Itoa(os.Getpid()))
		if err := os.Mkdir(stale, 0o700); err != nil {
			t.Fatal(err)
		}
		host := tmpfsFakeHost(64<<30, 1<<20, nil)
		host.Alive = func(pid int) bool { return pid == os.Getpid() } // the sweep leaves a root of a live pid, this one's
		if root, owner := tmpfsRoot(base, host, tmpfsNoEnv); root != "" || owner {
			t.Fatalf("a root %q was chosen over %s", root, stale)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Errorf("the root that was there was removed: %v", err)
		}
	})
	t.Run("a base that is not there", func(t *testing.T) {
		host := tmpfsRealHost()
		if _, _, err := host.Free(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("free space of a missing directory read")
		}
		if root, _ := tmpfsRoot(filepath.Join(t.TempDir(), "missing"), host, tmpfsNoEnv); root != "" {
			t.Errorf("a root %q was chosen under a base that is not there", root)
		}
	})
}

// The real readings: the free space of a directory and a probe that runs a script there.
func TestTmpfsRealHostReadsSpaceAndRunsAScript(t *testing.T) {
	host := tmpfsRealHost()
	dir := t.TempDir()
	bytes, inodes, err := host.Free(dir)
	if err != nil || bytes == 0 || inodes == 0 {
		t.Fatalf("free space of %s: %d bytes %d inodes %v", dir, bytes, inodes, err)
	}
	if err := host.Probe(dir); err != nil {
		t.Errorf("a script cannot run from %s: %v", dir, err)
	}
	if err := host.Probe(filepath.Join(dir, "missing")); err == nil {
		t.Error("the probe passed in a directory that is not there")
	}
	if !host.Alive(os.Getpid()) || host.Alive(1<<30) {
		t.Error("Alive does not tell this process from a process number nobody has")
	}
}

// A re-executed helper takes the root its parent named and makes none; a marker that names a dead parent, a directory that is not
// there or one that is not a root of that parent is no marker.
func TestTmpfsRootHelperReusesTheParentsRoot(t *testing.T) {
	host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
	marker := func(v string) func(string) string {
		return func(k string) string {
			if k == tmpfsRootEnv {
				return v
			}
			return ""
		}
	}
	made := func(t *testing.T, name string) (base, root string) {
		t.Helper()
		base = t.TempDir()
		root = filepath.Join(base, name)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		return base, root
	}
	t.Run("a live parent", func(t *testing.T) {
		base, parent := made(t, tmpfsRootPrefix+"4242")
		root, owner := tmpfsRoot(base, host, marker("4242:"+parent))
		if root != parent || owner {
			t.Fatalf("helper root %q owner %v, want %q not owned", root, owner, parent)
		}
		if left := tmpfsEntries(t, base); len(left) != 1 {
			t.Errorf("the helper made its own root: %v", left)
		}
	})
	for name, value := range map[string]func(base, parent string) string{
		"a dead parent": func(base, _ string) string {
			dead := filepath.Join(base, tmpfsRootPrefix+"4243")
			_ = os.Mkdir(dead, 0o700)
			return "4243:" + dead
		},
		"a root that is another pid's": func(base, _ string) string {
			other := filepath.Join(base, tmpfsRootPrefix+"4243")
			_ = os.Mkdir(other, 0o700)
			return "4242:" + other
		},
		"a missing root":      func(base, _ string) string { return "4242:" + filepath.Join(base, tmpfsRootPrefix+"4242-gone") },
		"a foreign directory": func(_, _ string) string { return "4242:" + t.TempDir() },
		"no process number":   func(_, parent string) string { return parent },
		"a relative root":     func(_, _ string) string { return "4242:" + tmpfsRootPrefix + "4242" },
	} {
		t.Run(name, func(t *testing.T) {
			base, parent := made(t, tmpfsRootPrefix+"4242")
			own, owner := tmpfsRoot(base, host, marker(value(base, parent)))
			if own == parent || own == "" || !owner {
				t.Fatalf("marker %q gave root %q owner %v, want a root of its own", value(base, parent), own, owner)
			}
		})
	}
}

// A run killed by a timeout or a signal cannot remove its root, and the root is memory; the next run removes the roots of processes that
// are gone, and only those.
func TestTmpfsRootSweepsTheRootsOfDeadProcessesOnly(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{tmpfsRootPrefix + "4243", tmpfsRootPrefix + "4242", tmpfsRootPrefix + "notapid", tmpfsRootPrefix + "4244-x", "crw-manage-123456", "other"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root, owner := tmpfsRoot(base, tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil), tmpfsNoEnv)
	if root == "" || !owner {
		t.Fatalf("no root: %q %v", root, owner)
	}
	got := map[string]bool{}
	for _, name := range tmpfsEntries(t, base) {
		got[name] = true
	}
	for _, name := range []string{tmpfsRootPrefix + "4242", tmpfsRootPrefix + "notapid", tmpfsRootPrefix + "4244-x", "crw-manage-123456", "other", filepath.Base(root)} {
		if !got[name] {
			t.Errorf("%s was removed", name)
		}
	}
	if got[tmpfsRootPrefix+"4243"] {
		t.Error("the root of a dead process was kept")
	}
}

// TestTmpfsRootChild is this test binary started again by TestTmpfsRootLeavesNothingAfterHelpersReexec: as the "parent" it goes through
// TestMain (which makes the root), starts itself once more as the "helper", and records the TMPDIR each saw; the helper leaves through
// os.Exit, as the supervisor and bridge helpers do, so nothing of its own is ever removed by it.
func TestTmpfsRootChild(t *testing.T) {
	role := os.Getenv(tmpfsChildEnv)
	if role == "" {
		return // a plain run of the package: only TestTmpfsRootLeavesNothingAfterHelpersReexec starts it as a child
	}
	record := func(line string) {
		f, err := os.OpenFile(os.Getenv(tmpfsResultEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, werr := fmt.Fprintln(f, line)
		if cerr := f.Close(); werr != nil || cerr != nil {
			t.Fatal(werr, cerr)
		}
	}
	record(role + " " + os.Getenv("TMPDIR"))
	if role == "helper" {
		os.Exit(0)
	}
	if late := os.Getenv(tmpfsLateBaseEnv); late != "" {
		if err := os.Mkdir(late, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTmpfsRootChild$")
	cmd.Env = append(os.Environ(), tmpfsChildEnv+"=helper")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the helper: %v\n%s", err, out)
	}
}

// CRW-1262: a run of the package puts its temporary root in one memory-backed directory; a helper it starts again uses that root and
// makes none, and when the run ends nothing of it is left, the helper's isolation tree included.
func TestTmpfsRootLeavesNothingAfterHelpersReexec(t *testing.T) {
	if os.Getenv(tmpfsChildEnv) != "" {
		return // the child runs only TestTmpfsRootChild
	}
	base, disk := t.TempDir(), t.TempDir()
	result := filepath.Join(t.TempDir(), "result")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTmpfsRootChild$")
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, tmpfsRootEnv+"=") && !strings.HasPrefix(kv, tmpfsBaseEnv+"=") && !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, tmpfsChildEnv+"=parent", tmpfsResultEnv+"="+result, tmpfsBaseEnv+"="+base, "TMPDIR="+disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child: %v\n%s", err, out)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		role, tmp, _ := strings.Cut(line, " ")
		seen[role] = tmp
	}
	if seen["parent"] == "" || filepath.Dir(seen["parent"]) != base || !strings.HasPrefix(filepath.Base(seen["parent"]), tmpfsRootPrefix) {
		t.Fatalf("the run's TMPDIR %q is not a root made under %s", seen["parent"], base)
	}
	if seen["helper"] != seen["parent"] {
		t.Errorf("the helper's TMPDIR is %q, want the parent's %q", seen["helper"], seen["parent"])
	}
	for _, dir := range []string{base, disk} {
		if left := tmpfsEntries(t, dir); len(left) != 0 {
			t.Errorf("%s was left %v", dir, left)
		}
	}
}

// A parent that chose no tmpfs (a same-name root, too little room) says so in the marker, and a helper it starts again follows it: it
// does not look again, so it cannot make a root of its own that it leaves through os.Exit (the d1 gap of the CRW-1262 evaluation).
func TestTmpfsRootDeclinedParentLeavesNothingAfterHelpersReexec(t *testing.T) {
	if os.Getenv(tmpfsChildEnv) != "" {
		return // the child runs only TestTmpfsRootChild
	}
	parentBase, disk := t.TempDir(), t.TempDir()
	base := filepath.Join(parentBase, "late") // not there when the parent chooses, fit when the helper starts
	result := filepath.Join(t.TempDir(), "result")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTmpfsRootChild$")
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, tmpfsRootEnv+"=") && !strings.HasPrefix(kv, tmpfsBaseEnv+"=") && !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, tmpfsChildEnv+"=parent", tmpfsResultEnv+"="+result, tmpfsBaseEnv+"="+base, tmpfsLateBaseEnv+"="+base, "TMPDIR="+disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child: %v\n%s", err, out)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		role, tmp, _ := strings.Cut(line, " ")
		seen[role] = tmp
	}
	if seen["parent"] != disk {
		t.Fatalf("the parent's TMPDIR is %q, want the caller's %q (no fit base)", seen["parent"], disk)
	}
	if seen["helper"] != disk {
		t.Errorf("the helper's TMPDIR is %q, want the caller's %q: it chose a root the parent had declined", seen["helper"], disk)
	}
	if left := tmpfsEntries(t, base); len(left) != 0 {
		t.Errorf("%s was left %v", base, left)
	}
}

// The marker of a parent that chose no tmpfs is "<pid>:-": a live pid makes a helper choose none, a dead one is no marker.
func TestTmpfsRootFollowsAnAliveParentsDeclining(t *testing.T) {
	host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
	marker := func(v string) func(string) string {
		return func(k string) string {
			if k == tmpfsRootEnv {
				return v
			}
			return ""
		}
	}
	base := t.TempDir()
	if root, owner := tmpfsRoot(base, host, marker("4242:-")); root != "" || owner {
		t.Fatalf("root %q owner %v, want none after a live parent declined", root, owner)
	}
	if left := tmpfsEntries(t, base); len(left) != 0 {
		t.Errorf("the helper made %v", left)
	}
	if root, owner := tmpfsRoot(base, host, marker("4243:-")); root == "" || !owner {
		t.Errorf("a dead parent's declining held: root %q owner %v", root, owner)
	}
	if got := tmpfsDeclinedMarker(4242); got != "4242:-" {
		t.Errorf("declined marker %q", got)
	}
}
