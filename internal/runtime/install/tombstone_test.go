package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
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

// An interrupted removal leaves one trace, the tombstone, and the two commands that report it
// name one recovery for it: crw install status lists every tombstone, and crw doctor's residue
// lists the ones whose claim is an abandoned staging's (a reclaim killed before its deletion
// finished). The residue used to say the next install would reclaim it, but an install reclaims
// the directory under a runtime's own name, never a tombstone for its own sake: crw install
// remove is what finishes one.
func TestDoctorResidueAndStatusNameOneRecoveryForATombstone(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	grave := filepath.Join(h.dest, ".crw-removing-"+filepath.Base(old))
	if err := os.Rename(old, grave); err != nil {
		t.Fatal(err)
	}
	unsettle(t, grave)

	status, _ := install.Status(context.Background(), h.options())
	interrupted := golden.List(at(status, "interruptedRemovals"))
	if len(interrupted) != 1 || at(golden.Obj(interrupted[0]), "path") != grave || at(golden.Obj(interrupted[0]), "ours") != true {
		t.Fatalf("status: %s", golden.Canon(at(status, "interruptedRemovals")))
	}
	fromStatus := text(at(golden.Obj(interrupted[0]), "recoveryRequires"))

	report := doctor.Diagnose(context.Background(), doctor.Options{Env: h.env, CodexVersion: codexCli, Socket: h.fake.SocketPath, State: h.relayState})
	paths, recoveries := golden.List(at(report, "residue", "residualPaths")), golden.List(at(report, "residue", "recoveryRequires"))
	if len(paths) != 1 || paths[0] != grave || len(recoveries) != 1 {
		t.Fatalf("the survey no longer lists the tombstone as its one residue: %s", golden.Canon(at(report, "residue")))
	}
	fromDoctor := text(recoveries[0])

	for label, got := range map[string]string{"status": fromStatus, "doctor": fromDoctor} {
		if !strings.Contains(got, "crw install remove "+grave) || strings.Contains(got, "next install") {
			t.Errorf("%s does not name crw install remove for %s: %s", label, grave, got)
		}
	}
	if fromDoctor != fromStatus {
		t.Errorf("the recoveries differ\n doctor: %s\n status: %s", fromDoctor, fromStatus)
	}
}
