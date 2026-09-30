package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// A directory named like a tombstone is finished only when it is one this command began: it
// carries a readable claim of crw install's whose staging lock
// nobody holds, or it is empty. Somebody's directory of that name, holding files and no claim, is
// left alone by remove (of the name, of the tombstone, and when clearing the way for a newer
// runtime's removal), by the reclaim of an abandoned staging, and status says it is not ours.
func TestATombstoneNeedsItsClaim(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	h.mustInstall(t, "update", archive(t, "0.9.2", ""))
	grave := filepath.Join(h.dest, ".crw-removing-"+filepath.Base(old))
	foreign := filepath.Join(grave, "somebody's.txt")
	write(t, foreign, "not crw install's\n")
	intact := func(label string) {
		t.Helper()
		if _, err := os.Stat(foreign); err != nil {
			t.Fatalf("%s: the unclaimed directory was touched: %v", label, err)
		}
	}

	status, _ := install.Status(context.Background(), h.options())
	if listed := golden.List(at(status, "interruptedRemovals")); len(listed) != 1 || at(golden.Obj(listed[0]), "ours") != false {
		t.Fatalf("status: %s", golden.Canon(at(status, "interruptedRemovals")))
	}
	if refused, code := install.Remove(context.Background(), h.options(), old); code != install.Refused || !strings.Contains(text(at(refused, "refused")), "carries no claim") {
		t.Fatalf("remove clearing the way for the runtime of that name: exit %d\n%s", code, golden.Canon(refused))
	}
	intact("remove of the runtime")
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("the runtime was removed although its tombstone's name was taken")
	}
	unsettle(t, old)
	if kept, code := install.Install(context.Background(), h.options(), "update", install.Source{From: first}); code != install.Refused || !strings.Contains(text(at(kept, "refused")), "carries no claim") {
		t.Fatalf("the reclaim: exit %d\n%s", code, golden.Canon(kept))
	}
	intact("the reclaim")
	if err := os.RemoveAll(old); err != nil { // the name is gone; only the look-alike is left
		t.Fatal(err)
	}
	for _, named := range []string{old, grave} {
		if refused, code := install.Remove(context.Background(), h.options(), named); code != install.Refused || !strings.Contains(text(at(refused, "refused")), "carries no claim") {
			t.Fatalf("remove %s: exit %d\n%s", named, code, golden.Canon(refused))
		}
		intact("remove " + named)
	}

	// An interrupted deletion leaves the claim (it goes last) or nothing; both are finished.
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if finished, code := install.Remove(context.Background(), h.options(), grave); code != install.OK || at(finished, "finished") != grave {
		t.Fatalf("an empty tombstone: exit %d\n%s", code, golden.Canon(finished))
	}
	write(t, filepath.Join(grave, "bin", "crw"), "x")
	write(t, staging.ClaimPath(grave), string(record.Encode(staging.NewPayload(staging.Complete, "CRW-158", "1"))))
	if finished, code := install.Remove(context.Background(), h.options(), grave); code != install.OK || at(finished, "finished") != grave {
		t.Fatalf("a claimed tombstone: exit %d\n%s", code, golden.Canon(finished))
	}
	if _, err := os.Lstat(grave); !os.IsNotExist(err) {
		t.Fatal("the tombstone is left")
	}
}
