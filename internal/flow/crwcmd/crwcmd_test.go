package crwcmd_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/crwcmd"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// program writes an executable file at dir/name and returns its path.
func program(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name, "crw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWordOrder (CRW-1572): the word an owner line starts with is CRW_BIN as written (trimmed), else the running binary's path as
// one shell word (a Linux " (deleted)" suffix stripped), else the installer's runtime pointer, else the bare crw with a warning.
func TestWordOrder(t *testing.T) {
	dir := t.TempDir()
	// a path with a space and a single quote: the word is one literal shell word
	exe := program(t, dir, "it's a dir")
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	pointer := host.ShellWord(filepath.Join(home, ".local", "share", "crw-runtime", "current", "bin", "crw"))

	t.Run("CRW_BIN as written", func(t *testing.T) {
		defer crwcmd.UseExecutable(exe)()
		t.Setenv(host.BinEnv, "  x y  ")
		if got := crwcmd.Word(); got != "x y" {
			t.Fatalf("Word() = %q, want CRW_BIN %q", got, "x y")
		}
		if w := crwcmd.Warning(); w != "" {
			t.Fatalf("a warning with CRW_BIN set: %q", w)
		}
	})
	t.Run("the running binary", func(t *testing.T) {
		defer crwcmd.UseExecutable(exe)()
		t.Setenv(host.BinEnv, " ")
		got := crwcmd.Word()
		if got != host.ShellWord(exe) || crwcmd.Warning() != "" {
			t.Fatalf("Word() = %q (warning %q), want the executable %q as one shell word", got, crwcmd.Warning(), host.ShellWord(exe))
		}
		// the word runs the binary at that path as one argument, whatever the shell's PATH
		out, err := exec.Command("/bin/sh", "-c", got+" flow next").CombinedOutput()
		if err != nil || string(out) != exe+"\nflow\nnext\n" {
			t.Fatalf("sh -c %q: %v %q", got+" flow next", err, out)
		}
	})
	t.Run("a deleted suffix", func(t *testing.T) {
		defer crwcmd.UseExecutable(exe + " (deleted)")()
		t.Setenv(host.BinEnv, "")
		if got := crwcmd.Word(); got != host.ShellWord(exe) {
			t.Fatalf("Word() = %q, want %q", got, host.ShellWord(exe))
		}
	})
	for name, path := range map[string]string{
		"a missing executable":   filepath.Join(dir, "gone", "crw"),
		"a deleted and gone one": filepath.Join(dir, "gone", "crw") + " (deleted)",
		"a relative path":        "bin/crw",
		"a directory":            dir,
		"no executable":          "",
	} {
		t.Run(name, func(t *testing.T) {
			defer crwcmd.UseExecutable(path)()
			t.Setenv(host.BinEnv, "")
			if got := crwcmd.Word(); got != pointer || crwcmd.Warning() != "" {
				t.Fatalf("Word() = %q (warning %q), want the runtime pointer %q", got, crwcmd.Warning(), pointer)
			}
		})
	}
	t.Run("no home", func(t *testing.T) {
		defer crwcmd.UseExecutable("")()
		defer crwcmd.UseRuntimePointer("", errors.New("no absolute home directory holds the crw runtime pointer: set CRW_BIN"))()
		t.Setenv(host.BinEnv, "")
		if got := crwcmd.Word(); got != "crw" {
			t.Fatalf("Word() = %q, want the bare crw", got)
		}
		if w := crwcmd.Warning(); !strings.Contains(w, "crw") || !strings.Contains(w, "PATH") || !strings.Contains(w, "set CRW_BIN") {
			t.Fatalf("Warning() = %q, want why the bare crw is named", w)
		}
	})
	t.Run("override", func(t *testing.T) {
		defer crwcmd.UseExecutable(exe)()
		t.Setenv(host.BinEnv, "x y")
		restore := crwcmd.Override("'/opt/crw test/bin/crw'")
		if got := crwcmd.Word(); got != "'/opt/crw test/bin/crw'" || crwcmd.Warning() != "" {
			restore()
			t.Fatalf("Word() under Override = %q", got)
		}
		inner := crwcmd.Override("")
		if got := crwcmd.Word(); got != "x y" {
			t.Errorf("Word() under Override(\"\") = %q, want the order's CRW_BIN", got)
		}
		inner()
		if got := crwcmd.Word(); got != "'/opt/crw test/bin/crw'" {
			t.Errorf("the inner restore left %q", got)
		}
		restore()
		if got := crwcmd.Word(); got != "x y" {
			t.Errorf("Word() after restore = %q, want CRW_BIN", got)
		}
	})
	t.Run("the test binary itself", func(t *testing.T) {
		t.Setenv(host.BinEnv, "")
		self, err := os.Executable()
		if err != nil {
			t.Skip(err)
		}
		if got := crwcmd.Word(); got != host.ShellWord(self) {
			t.Fatalf("Word() with no seam = %q, want this process's executable %q", got, host.ShellWord(self))
		}
	})
}
