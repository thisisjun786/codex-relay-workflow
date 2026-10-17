package manage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

func TestTmpfsRootKeepsControlSocketPathsShort(t *testing.T) {
	root := filepath.Join(tmploctest.MemoryBase(""), tmpfsRootName(1<<22, "ffffffffffffffff"))
	if size := len(root) + 67; size > 107 {
		t.Fatalf("control socket path needs %d bytes, over the Linux limit of 107", size)
	}
}

func TestTmpfsRootNamesPIDNamespace(t *testing.T) {
	base := t.TempDir()
	root, owner := tmpfsRoot(base, tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil), tmpfsNoEnv)
	if root == "" || !owner {
		t.Fatalf("root %q owner %v, want a root this call owns", root, owner)
	}
	if want := fmt.Sprintf("crw-m-%d-effffffc", os.Getpid()); filepath.Base(root) != want {
		t.Errorf("root name %q, want %q", filepath.Base(root), want)
	}
}

func TestTmpfsSweepRespectsPIDNamespaces(t *testing.T) {
	base := t.TempDir()
	roots := map[string]bool{
		"crw-m-4243-effffffc":       false,
		"crw-m-4242-effffffc":       true,
		"crw-m-4243-f0000487":       true,
		"crw-m-5252-f0000487":       true,
		tmpfsRootPrefix + "4244":    false,
		tmpfsRootPrefix + "4242":    true,
		"crw-m-bad-effffffc":        true,
		"crw-m-4243-":               true,
		"crw-m-4243-effffffc-extra": true,
		"crw-m-0-effffffc":          true,
		"crw-m--1-effffffc":         true,
	}
	for name := range roots {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("keep "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	linkTarget := t.TempDir()
	link := filepath.Join(base, "crw-m-6363-effffffc")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	calls := map[int]int{}
	host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
	host.Alive = func(pid int) bool {
		calls[pid]++
		return pid == 4242
	}
	tmpfsSweep(base, host)
	for name, keep := range roots {
		if !keep {
			if _, err := os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
				t.Errorf("%s was kept", name)
			}
			continue
		}
		body, err := os.ReadFile(filepath.Join(base, name, "keep"))
		if err != nil || string(body) != "keep "+name {
			t.Errorf("%s was removed or written: %q %v", name, body, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink root was removed: %v", err)
	}
	if _, err := os.Stat(linkTarget); err != nil {
		t.Errorf("the symlink's target was removed: %v", err)
	}
	for pid, want := range map[int]int{4243: 1, 4244: 1, 4242: 2, 5252: 0, 6363: 0} {
		if calls[pid] != want {
			t.Errorf("Alive(%d) was called %d times, want %d", pid, calls[pid], want)
		}
	}
}

func TestTmpfsRootDeclinesWithoutPIDNamespace(t *testing.T) {
	base := t.TempDir()
	host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
	host.Namespace = ""
	if root, owner := tmpfsRoot(base, host, tmpfsNoEnv); root != "" || owner {
		t.Fatalf("root %q owner %v, want none when the pid namespace cannot be named", root, owner)
	}
	if left := tmpfsEntries(t, base); len(left) != 0 {
		t.Errorf("a nameless root was left %v", left)
	}
}

func TestTmpfsNamedRootRespectsPIDNamespaces(t *testing.T) {
	for name, tc := range map[string]struct {
		dir    string
		accept bool
		probes int
	}{
		"a legacy root of the live owner":  {tmpfsRootPrefix + "4242", true, 1},
		"our own qualified root":           {"crw-m-4242-effffffc", true, 1},
		"another namespace's root":         {"crw-m-4242-f0000487", false, 0},
		"a root of another process number": {"crw-m-4243-effffffc", false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, tc.dir)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "keep"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			probes := 0
			host := tmpfsFakeHost(tmpfsFloorBytes, tmpfsFloorInodes, nil)
			host.Alive = func(pid int) bool {
				probes++
				return pid == 4242
			}
			got, ok := tmpfsNamedRoot(host, "4242:"+root)
			if tc.accept {
				if !ok || got != root {
					t.Fatalf("named root %q %v, want %q", got, ok, root)
				}
			} else if ok || got != "" {
				t.Fatalf("named root %q %v, want none", got, ok)
			}
			if probes != tc.probes {
				t.Errorf("Alive was probed %d times, want %d", probes, tc.probes)
			}
		})
	}
}

func TestTmpfsRealHostNamesPIDNamespace(t *testing.T) {
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("/proc/self/ns/pid is not a Stat_t")
	}
	host := tmpfsRealHost()
	if host.Namespace == "" {
		t.Fatal("the real host names no pid namespace")
	}
	if want := strconv.FormatUint(st.Ino, 16); host.Namespace != want {
		t.Errorf("the real host's namespace is %q, want the inode of /proc/self/ns/pid %q", host.Namespace, want)
	}
}
