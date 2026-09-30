package install_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// A rollback records the pointer's placement with --issue as its evidence, and a blank one is a
// placement record.PlacementRecorded rejects, after which every install and rollback would refuse
// the pointer as unowned. So a blank or whitespace issue is refused before any lock is taken or
// anything read or written, as install refuses it.
func TestARollbackNeedsAnIssue(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	updated := h.pointerTarget(t)
	before := readFile(t, h.record)
	for _, issue := range []string{"", "   "} {
		o := h.options()
		o.Issue = issue
		result, code := install.Rollback(context.Background(), o, "")
		if code != install.Refused || !strings.Contains(text(at(result, "refused")), "--issue") {
			t.Fatalf("issue %q: exit %d\n%s", issue, code, golden.Canon(result))
		}
		if h.pointerTarget(t) != updated || readFile(t, h.record) != before {
			t.Fatalf("issue %q: a refused rollback changed the host", issue)
		}
	}
	if result, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK {
		t.Fatalf("with an issue: exit %d\n%s", code, golden.Canon(result))
	}
}

// A runtime whose install finished is already installed only when every component selects it
// and the pointer names it. With a split selection - the relay on this runtime, the bridge on an
// older one, the pointer here - reinstalling it is refused, naming the split, and nothing is
// written; crw install rollback to it, which moves no pointer, selects every component, after
// which it is already installed.
func TestASplitSelectionIsNotReportedInstalled(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if _, err := record.Update(h.record, 1, record.Delta{Select: []contract.Field{{Key: "codex-thread-bridge", Value: filepath.Join(old, "bin")}}}); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, h.record)
	refused, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Refused || at(refused, "alreadyInstalled") == true || !strings.Contains(text(at(refused, "refused")), "codex-thread-bridge selects") {
		t.Fatalf("a split selection: exit %d\n%s", code, golden.Canon(refused))
	}
	if readFile(t, h.record) != before || h.pointerTarget(t) != updated {
		t.Fatal("the refusal changed the host")
	}
	if result, code := install.Rollback(context.Background(), h.options(), updated); code != install.OK || at(result, "moved") != false {
		t.Fatalf("the repair: exit %d\n%s", code, golden.Canon(result))
	}
	if again, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second}); code != install.OK || at(again, "alreadyInstalled") != true {
		t.Fatalf("after the repair: exit %d\n%s", code, golden.Canon(again))
	}
}
