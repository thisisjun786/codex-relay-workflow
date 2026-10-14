package manage

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1262: the memory-backed temporary root of the package's tests (core_tmpfs_test.go). The two regressions that made CRW-1259 drop
// it are pinned here: a re-executed helper made its own root and left it, and one small exec probe did not reject a small or full tmpfs.

const tmpfsChildEnv = "CRW_MANAGE_TEST_TMPFS_CHILD"
const tmpfsResultEnv = "CRW_MANAGE_TEST_TMPFS_RESULT"

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
	if filepath.Dir(root) != base || !strings.HasPrefix(filepath.Base(root), tmpfsRootPrefix) {
		t.Errorf("root %q is not a %s* directory of %s", root, tmpfsRootPrefix, base)
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
// there or one that is not a root of ours is no marker.
func TestTmpfsRootHelperReusesTheParentsRoot(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, tmpfsRootPrefix+"4242-abc")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
	marker := func(v string) func(string) string {
		return func(k string) string {
			if k == tmpfsRootEnv {
				return v
			}
			return ""
		}
	}
	root, owner := tmpfsRoot(base, host, marker(fmt.Sprintf("4242:%s", parent)))
	if root != parent || owner {
		t.Fatalf("helper root %q owner %v, want %q not owned", root, owner, parent)
	}
	if left := tmpfsEntries(t, base); len(left) != 1 {
		t.Errorf("the helper made its own root: %v", left)
	}
	for name, value := range map[string]string{
		"a dead parent":       fmt.Sprintf("4243:%s", parent),
		"a missing root":      fmt.Sprintf("4242:%s", filepath.Join(base, tmpfsRootPrefix+"4242-gone")),
		"a foreign directory": fmt.Sprintf("4242:%s", t.TempDir()),
		"no process number":   parent,
		"a relative root":     "4242:" + tmpfsRootPrefix + "4242-abc",
	} {
		t.Run(name, func(t *testing.T) {
			own, owner := tmpfsRoot(base, host, marker(value))
			if own == parent || own == "" || !owner {
				t.Fatalf("marker %q gave root %q owner %v, want a root of its own", value, own, owner)
			}
		})
	}
}

// A run killed by a timeout or a signal cannot remove its root, and the root is memory; the next run removes the roots of processes that
// are gone, and only those.
func TestTmpfsRootSweepsTheRootsOfDeadProcessesOnly(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{tmpfsRootPrefix + "4243-dead", tmpfsRootPrefix + "4242-alive", tmpfsRootPrefix + "notapid-x", "crw-manage-123456", "other"} {
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
	for _, name := range []string{tmpfsRootPrefix + "4242-alive", tmpfsRootPrefix + "notapid-x", "crw-manage-123456", "other", filepath.Base(root)} {
		if !got[name] {
			t.Errorf("%s was removed", name)
		}
	}
	if got[tmpfsRootPrefix+"4243-dead"] {
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
