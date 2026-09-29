package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
)

// A promotion proves the pointer it placed as a host reaches it: when <pointer>/bin/crw is not a
// regular executable file once the pointer is placed (here the placement leaves the candidate's
// binary unexecutable), the pointer is put back, the selection restored and the run refused -
// never a promotion reported for a pointer no host can use.
func TestAPromotionProvesThePointerItPlaced(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	restore := install.ReplacePointerPlacement(func(path, target string) error {
		if err := os.Chmod(filepath.Join(target, "bin", "crw"), 0o644); err != nil {
			return err
		}
		return pointer.Place(path, target)
	})
	defer restore()
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Refused || at(result, "failedStep") != "replace the owned pointer" || at(result, "applied") != false {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != old || at(h.hostRecord(t), "selected", "codex-session-relay") != filepath.Join(old, "bin") {
		t.Fatalf("the pointer or the selection was left on the candidate: %s", h.pointerTarget(t))
	}
}

// A destination spelled through the owned pointer puts the candidate inside the runtime the
// pointer names, and the pointer placed at it would loop. It is refused before anything is
// created.
func TestADestinationInsideTheRuntimeIsRefused(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	o := h.options()
	o.Dest = pointer.Path(h.dest)
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || !strings.Contains(text(at(result, "refused")), "through the owned pointer") || at(result, "steps") != nil {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if entries, _ := filepath.Glob(filepath.Join(old, "bin-*")); len(entries) != 0 || h.pointerTarget(t) != old {
		t.Fatalf("something was created inside the runtime: %v", entries)
	}
}
