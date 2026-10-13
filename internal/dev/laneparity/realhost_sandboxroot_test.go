//go:build dev

package laneparity

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// recordingCodex is a stand-in Codex that appends every call's arguments to a file and answers
// --version; any other call (an exec) fails after being recorded.
func recordingCodex(t *testing.T) (codex, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls")
	codex = filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + record + "'\ncase \"$1\" in --version) echo codex-cli 0.0.0-fake; exit 0;; esac\nexit 2\n"
	if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return codex, record
}

func recordedCalls(t *testing.T, record string) string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// CRW-1265. Every cell of a real-host run starts a sandboxed Codex (read-only) in a work directory
// below the run's scratch directory. CRW-1265 refused a scratch in the account home, or anywhere
// outside the sandbox base, before any Codex exec; since CRW-1308 the cell runs Codex there and the
// rule's verdict is a recorded warning.
func TestRealhostRefusesScratchAtHomeBeforeCodex(t *testing.T) {
	w := sandboxroottest.Fake(t)
	root := repoRoot(t)
	_, crw := fakeHost(t, root, fakeUnsupported, false)
	for name, scratch := range map[string]string{"the account home": w.Home, "outside the sandbox base": w.Mkdir(t, w.Root, "elsewhere")} {
		t.Run(name, func(t *testing.T) {
			w.ClearWarnings(t)
			codex, record := recordingCodex(t)
			_, err := RealHost(RealHostOptions{Root: root, CRW: crw, Plugin: filepath.Join(root, "plugins", "crw"), Codex: codex, Scratch: scratch,
				Timeout: 30 * time.Second, Only: regexp.MustCompile("^turn/crw$")})
			if err != nil && strings.Contains(err.Error(), sandboxroot.Code) {
				t.Fatalf("scratch %s: the rule refused: %v", scratch, err)
			}
			if calls := recordedCalls(t, record); !strings.Contains(calls, "exec") {
				t.Fatalf("scratch %s never reached Codex:\n%s", scratch, calls)
			}
			// The run's own directory (rh-*) below the scratch and the cell's work directory are outside every dedicated root.
			w.Warned(t, "laneparity/realhost", sandboxroot.RuleOutsideDedicatedRoot, "")
			// What the run made inside the scratch is gone with it.
			if leftovers, _ := filepath.Glob(filepath.Join(scratch, "rh-*")); len(leftovers) != 0 {
				t.Fatalf("the run left %v", leftovers)
			}
		})
	}
	// A scratch below the sandbox base is judged fine: the cell goes on to run Codex.
	codex, record := recordingCodex(t)
	_, _ = RealHost(RealHostOptions{Root: root, CRW: crw, Plugin: filepath.Join(root, "plugins", "crw"), Codex: codex, Scratch: w.Mkdir(t, w.Sandbox, "scratch"),
		Timeout: 30 * time.Second, Only: regexp.MustCompile("^turn/crw$")})
	if calls := recordedCalls(t, record); !strings.Contains(calls, "exec") {
		t.Fatalf("an allowed scratch never reached Codex:\n%s", calls)
	}
}
