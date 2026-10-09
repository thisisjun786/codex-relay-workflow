//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A switch that is a dangling link or a FIFO is there and cannot be read: the GitHub post guard
// runs (and denies), the warning is left, and the hook does not wait on the file.
func TestHookSwitchDamagedEntryRunsTheGuard(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, switchFile string)
	}{
		{"dangling-link", func(t *testing.T, f string) {
			if err := os.Symlink(f+".gone", f); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, f string) {
			if err := syscall.Mkfifo(f, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, "codex")
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", codexHome)
			t.Setenv("PLUGIN_ROOT", "")
			if err := os.MkdirAll(filepath.Join(codexHome, "crw"), 0o700); err != nil {
				t.Fatal(err)
			}
			c.setup(t, filepath.Join(codexHome, "crw", "switch.json"))
			in := filepath.Join(home, "input.json")
			if err := os.WriteFile(in, []byte(`{"hook_event_name":"PreToolUse","tool_name":"exec_command","tool_input":{"cmd":"gh pr create --body \"inline text\""}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(in)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			stdin := os.Stdin
			os.Stdin = f
			t.Cleanup(func() { os.Stdin = stdin })
			var out, errOut strings.Builder
			got := make(chan int, 1)
			go func() {
				got <- run(context.Background(), "crw", []string{"hook", "pre-tool-use", "--leg", "pre-tool-use-guarding-github-post"}, &out, &errOut)
			}()
			select {
			case code := <-got:
				if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
					t.Fatalf("%d %q %q", code, out.String(), errOut.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the hook waits on the switch file")
			}
			if _, err := os.Stat(filepath.Join(codexHome, "crw", "hook-observations", "switch-warning.json")); err != nil {
				t.Fatalf("no warning: %v", err)
			}
		})
	}
}
