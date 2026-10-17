package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func crw1650Reader(t *testing.T, files map[string]string, deny map[string]bool) func(string) ([]byte, error) {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		local := filepath.Join(root, strings.TrimPrefix(path, "/"))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func(path string) ([]byte, error) {
		if deny[path] {
			return nil, os.ErrPermission
		}
		return os.ReadFile(filepath.Join(root, strings.TrimPrefix(path, "/")))
	}
}

func TestCRW1650EffectiveWorkerCPU(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		deny  map[string]bool
		want  int
	}{
		"root quota":        {files: map[string]string{"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/cpu.max": "200000 100000\n"}, want: 2},
		"root absent":       {files: map[string]string{"/proc/self/cgroup": "0::/\n"}, want: 8},
		"child and root":    {files: map[string]string{"/proc/self/cgroup": "0::/child\n", "/sys/fs/cgroup/child/cpu.max": "400000 100000\n", "/sys/fs/cgroup/cpu.max": "200000 100000\n"}, want: 2},
		"child root absent": {files: map[string]string{"/proc/self/cgroup": "0::/child\n", "/sys/fs/cgroup/child/cpu.max": "400000 100000\n"}, want: 4},
		"unlimited root":    {files: map[string]string{"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/cpu.max": "max 100000\n"}, want: 8},
		"child absent":      {files: map[string]string{"/proc/self/cgroup": "0::/child\n", "/sys/fs/cgroup/cpu.max": "200000 100000\n"}, want: 0},
		"no cgroup file":    {files: map[string]string{}, want: 0},
		"root denied":       {files: map[string]string{"/proc/self/cgroup": "0::/\n"}, deny: map[string]bool{"/sys/fs/cgroup/cpu.max": true}, want: 0},
		"child denied":      {files: map[string]string{"/proc/self/cgroup": "0::/child\n", "/sys/fs/cgroup/cpu.max": "200000 100000\n"}, deny: map[string]bool{"/sys/fs/cgroup/child/cpu.max": true}, want: 0},
		"malformed quota":   {files: map[string]string{"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/cpu.max": "abc 100000\n"}, want: 0},
		"zero period":       {files: map[string]string{"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/cpu.max": "200000 0\n"}, want: 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := effectiveWorkerCPUFrom(8, crw1650Reader(t, c.files, c.deny)); got != c.want {
				t.Errorf("effectiveWorkerCPUFrom(8) = %d, want %d", got, c.want)
			}
		})
	}
}
