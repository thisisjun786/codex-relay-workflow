package crwconfig

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hostPath matches what ties product text to one machine: this host's data and scratch roots, a
// per-task directory of a lane, a device name and a mount of a named disk. A host places its
// roots through the configuration file's paths section; product code, skills and packet
// templates name a root (scratch_root, temp_root) and never a path.
var hostPath = regexp.MustCompile(`/home/jun\b|(^|[^.\w])/(state/crw|scratch)(/|\b)|/var/tmp/crw|/var/tmp/cxc-v0\.2\.40/|/dev/(nvme|sd[a-z]|vd[a-z]|xvd[a-z]|disk[0-9])|\bnvme[0-9]+n[0-9]+\b|/mnt/(disk|nvme|ssd|hdd|data)[0-9]*\b|\blina-os\b`)

// repositoryRoot walks up from the test's directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// CRW-958: no host path or device name sits in product code, in a skill or in a packet template.
// Tests, test data and the documentation of what happened on a host are outside this scan: a test
// builds its own temporary tree, and a record may say where it ran.
func TestNoHostPathOrDeviceNameInProduct(t *testing.T) {
	root := repositoryRoot(t)
	var hits []string
	scanned := 0
	for _, top := range []string{"cmd", "internal", "plugins", "packages", "contract", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case "testdata", "node_modules", ".git":
					return fs.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if strings.HasSuffix(name, "_test.go") || !entry.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.IndexByte(string(data), 0) >= 0 {
				return nil // binary
			}
			scanned++
			for n, line := range strings.Split(string(data), "\n") {
				if hostPath.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					if rel == filepath.Join("internal", "crwconfig", "paths.go") && strings.TrimSpace(line) == `const DefaultSandboxRoot = "/scratch/crw"` {
						// CRW-1265, decision C: sandbox_root's default is the one host path product code names.
						continue
					}
					hits = append(hits, rel+":"+strconv.Itoa(n+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("the scan read %d files: it is not looking at the product tree", scanned)
	}
	for _, hit := range hits {
		t.Errorf("host path or device name in product text: %s", hit)
	}
}

// The pattern itself: it names this host's places and a disk, and leaves the XDG defaults and the
// word "scratch" alone.
func TestHostPathPattern(t *testing.T) {
	for _, line := range []string{"/home/jun/code", "/state/crw/mgmt", "/scratch/crw/fuzz", "/var/tmp/crw-12", "/var/tmp/cxc-v0.2.40/plugins", "/dev/nvme0n1p2", "/dev/sda1", "nvme1n1", "/mnt/disk2", "lina-os"} {
		if !hostPath.MatchString(line) {
			t.Errorf("%q is not matched", line)
		}
	}
	for _, line := range []string{"~/.local/state/crw", "scratch_root", "/tmp/crw", "$XDG_STATE_HOME/crw", "/mnt/user-data/outputs", "cxc-v0.2.40/plugins/codexclaw", "/var/tmp/cxc-v0.2.40-rec"} {
		if hostPath.MatchString(line) {
			t.Errorf("%q is matched", line)
		}
	}
}
