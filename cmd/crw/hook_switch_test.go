package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// switchOn puts the switch file that turns the ported hooks on in a test's CODEX_HOME, as
// crw install switch writes it (CRW-201).
func switchOn(t *testing.T, codexHome string) {
	t.Helper()
	writeSwitch(t, codexHome, `{"active":"crw","changedAt":"2026-10-10T00:00:00.000Z","by":"test"}`)
}

func writeSwitch(t *testing.T, codexHome, body string) {
	t.Helper()
	dir := filepath.Join(codexHome, "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "switch.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The ported legs (K1 and the GitHub post guard) read <CODEX_HOME>/crw/switch.json before anything
// else: no file and a cxc switch leave them silent, with exit 0 and nothing written; a crw switch
// runs them; a file that cannot be read runs them too, so a protective guard never goes quiet,
// and leaves a warning among the hook observations. The relay's own Stop (--plugin-launch) is not
// behind the switch.
func TestHookSwitchGatesThePortedLegs(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, codexHome string)
		on    bool
		warn  bool
	}{
		{"missing", func(*testing.T, string) {}, false, false},
		{"cxc", func(t *testing.T, h string) {
			writeSwitch(t, h, `{"active":"cxc","changedAt":"2026-10-10T00:00:00.000Z","by":"test"}`)
		}, false, false},
		{"crw", switchOn, true, false},
		{"unreadable", func(t *testing.T, h string) { writeSwitch(t, h, `{"active":`) }, true, true},
		{"unknown-state", func(t *testing.T, h string) { writeSwitch(t, h, `{"active":"both"}`) }, true, true},
		{"directory", func(t *testing.T, h string) {
			if err := os.MkdirAll(filepath.Join(h, "crw", "switch.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, "codex")
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", codexHome)
			t.Setenv("PLUGIN_ROOT", "")
			c.setup(t, codexHome)
			before := treeOf(t, codexHome)
			over := filepath.Join(home, "oversized.json")
			if err := os.WriteFile(over, []byte(strings.Repeat("x", 4*1024*1024+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			stdin := os.Stdin
			t.Cleanup(func() { os.Stdin = stdin })
			for _, leg := range []struct {
				args []string
				want string
			}{
				{[]string{"hook", "stop", "--leg", "stop-checking-pabcd-continuation"}, `"decision":"block"`},
				{[]string{"hook", "pre-tool-use", "--leg=pre-tool-use-guarding-github-post"}, `"permissionDecision":"deny"`},
			} {
				f, err := os.Open(over)
				if err != nil {
					t.Fatal(err)
				}
				os.Stdin = f
				var out, errOut strings.Builder
				code := run(context.Background(), "crw", leg.args, &out, &errOut)
				f.Close()
				if code != 0 || errOut.Len() != 0 {
					t.Fatalf("%v: %d %q %q", leg.args, code, out.String(), errOut.String())
				}
				if c.on != strings.Contains(out.String(), leg.want) || !c.on && out.Len() != 0 {
					t.Fatalf("%v with switch %s: output %q, want on=%v", leg.args, c.name, out.String(), c.on)
				}
			}
			warning := filepath.Join(codexHome, "crw", "hook-observations", "switch-warning.json")
			b, err := os.ReadFile(warning)
			if c.warn {
				var w map[string]any
				if err != nil || json.Unmarshal(b, &w) != nil || w["treatedAs"] != "crw" || w["leg"] != "pre-tool-use-guarding-github-post" || w["problem"] == "" {
					t.Fatalf("warning %q %v", b, err)
				}
				return
			}
			if after := treeOf(t, codexHome); after != before {
				t.Fatalf("switch %s wrote under CODEX_HOME:\n%s\nwas:\n%s", c.name, after, before)
			}
		})
	}
}

// Off means not read: the input of a silenced leg stays unread, and the relay's Stop is not gated.
func TestHookSwitchOffLeavesTheInputUnread(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	in, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer hold.Close()
	stdin := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = stdin })
	// The pipe is never written or closed: a leg that read it would wait for ever.
	var out, errOut strings.Builder
	got := make(chan int, 1)
	go func() {
		got <- run(context.Background(), "crw", []string{"hook", "session-start", "--leg", "session-start-bootstrapping-pabcd-state"}, &out, &errOut)
	}()
	select {
	case code := <-got:
		if code != 0 || out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("%d %q %q", code, out.String(), errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a leg behind an off switch waits for its input")
	}
}

// The link-time state stands in for the file: a test build of the replay links crw in, and a build
// that links nothing reads the file.
func TestHookSwitchLinkTimeState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	old := hookSwitchTestState
	t.Cleanup(func() { hookSwitchTestState = old })
	for state, want := range map[string]bool{"": false, "crw": true, "cxc": false} {
		hookSwitchTestState = state
		if got := hookSwitchOn("stop-checking-pabcd-continuation", os.LookupEnv); got != want {
			t.Errorf("linked %q: on=%v, want %v", state, got, want)
		}
	}
}

func treeOf(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
