package install_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// A first install verifies the archive, unpacks the binary with its three links, exercises the
// candidate through its own executables, records the install entries, a point and the pointer's
// placement, moves the pointer and settles the claim last.
func TestInstallRecordsThePointerAndSettlesTheClaimLast(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	env := runtimeDir(h, "0.9.0", first, t)
	result := h.mustInstall(t, "install", first)
	if at(result, "environment") != env || at(result, "inService") != true || at(result, "swapGate", "verdict") != "ALLOWED" {
		t.Fatalf("result: %s", golden.Canon(result))
	}
	if target := h.pointerTarget(t); target != env {
		t.Fatalf("pointer names %s", target)
	}
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
		if link, err := os.Readlink(filepath.Join(env, "bin", name)); err != nil || link != "crw" {
			t.Fatalf("%s -> %q %v", name, link, err)
		}
	}
	if claimState(t, env) != staging.Complete {
		t.Fatalf("claim %v", claimState(t, env))
	}
	rec := h.hostRecord(t)
	bin := filepath.Join(env, "bin")
	for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
		if at(rec, "selected", c) != bin {
			t.Fatalf("selected %s: %v", c, at(rec, "selected", c))
		}
		installs := golden.List(at(rec, "components", c, "installs"))
		points := golden.List(at(rec, "components", c, "measuredPoints"))
		if len(installs) != 1 || len(points) != 1 {
			t.Fatalf("%s: %d installs, %d points", c, len(installs), len(points))
		}
		entry, point := golden.Obj(installs[0]), golden.Obj(points[0])
		if record.Get(entry, "location") != bin || record.Get(entry, "version") != "0.9.0" || record.Get(entry, "interpreterPath") != nil {
			t.Fatalf("install entry: %s", golden.Canon(entry))
		}
		if record.Get(point, "exercised") != true || record.Get(point, "install") != bin || record.Get(point, "appServer") == nil || record.Get(point, "codexCli") != "codex-cli 0.154.0" {
			t.Fatalf("point: %s", golden.Canon(point))
		}
		if at(rec, "outgoing", c, "selected") != nil {
			t.Fatalf("a first install replaced nothing: %s", golden.Canon(at(rec, "outgoing")))
		}
	}
	if !record.PlacementRecorded(record.Get(rec, "pointer")) || at(rec, "recordVersion") != int64(1) {
		t.Fatalf("pointer ownership: %s", golden.Canon(record.Get(rec, "pointer")))
	}
	status, code := install.Status(context.Background(), h.options())
	if code != install.OK || at(status, "selected") != "go-binary" || at(status, "runtime", "agrees") != true {
		t.Fatalf("status: %s", golden.Canon(status))
	}
}

// An update to a second archive swaps the pointer and records the replaced runtime as outgoing;
// the old directory stays, settled. Rollback returns to it (and records the one it left as
// outgoing, so a second rollback returns again). Remove refuses the selected runtime and removes
// an unselected settled one, dropping its install entries.
func TestUpdateRollbackAndRemove(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	result := h.mustInstall(t, "update", second)
	if h.pointerTarget(t) != updated || at(result, "pointer", "previousTarget") != old {
		t.Fatalf("update: pointer %s\n%s", h.pointerTarget(t), golden.Canon(at(result, "pointer")))
	}
	if claimState(t, old) != staging.Complete {
		t.Fatalf("the replaced runtime's claim: %v", claimState(t, old))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatal("the replaced runtime was removed")
	}
	rec := h.hostRecord(t)
	if at(rec, "outgoing", "codex-session-relay", "selected") != filepath.Join(old, "bin") || at(rec, "outgoing", "codex-session-relay", "present") != true {
		t.Fatalf("outgoing: %s", golden.Canon(at(rec, "outgoing")))
	}

	back, code := install.Rollback(context.Background(), h.options(), "")
	if code != install.OK || h.pointerTarget(t) != old || at(back, "environment") != old {
		t.Fatalf("rollback: exit %d pointer %s\n%s", code, h.pointerTarget(t), golden.Canon(back))
	}
	rec = h.hostRecord(t)
	if at(rec, "selected", "codex-thread-bridge") != filepath.Join(old, "bin") || at(rec, "outgoing", "codex-thread-bridge", "selected") != filepath.Join(updated, "bin") {
		t.Fatalf("after rollback: %s %s", golden.Canon(at(rec, "selected")), golden.Canon(at(rec, "outgoing")))
	}
	status, _ := install.Status(context.Background(), h.options())
	if at(status, "runtime", "agrees") != true || at(status, "runtime", "targetResolves") != old {
		t.Fatalf("status after rollback: %s", golden.Canon(at(status, "runtime")))
	}
	if again, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != updated {
		t.Fatalf("a second rollback returns: exit %d\n%s", code, golden.Canon(again))
	}
	if again, code := install.Rollback(context.Background(), h.options(), old); code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("a rollback to a named runtime: exit %d\n%s", code, golden.Canon(again))
	}

	refused, code := install.Remove(context.Background(), h.options(), old)
	if code != install.Refused || at(refused, "applied") != false {
		t.Fatalf("removing the selected runtime: exit %d\n%s", code, golden.Canon(refused))
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a refused remove removed something")
	}
	removed, code := install.Remove(context.Background(), h.options(), updated)
	if code != install.OK || at(removed, "removed") != true {
		t.Fatalf("removing an unselected settled runtime: exit %d\n%s", code, golden.Canon(removed))
	}
	if _, err := os.Lstat(updated); !os.IsNotExist(err) {
		t.Fatal("the removed runtime is still there")
	}
	for _, raw := range golden.List(at(h.hostRecord(t), "components", "codex-session-relay", "installs")) {
		if record.Get(golden.Obj(raw), "environment") == updated {
			t.Fatal("the removed runtime's install entry is still recorded")
		}
	}
	if len(golden.List(at(h.hostRecord(t), "components", "codex-session-relay", "measuredPoints"))) != 2 {
		t.Fatal("measured points are history and stay")
	}
}

// A failure after the unpack and before the promotion - here the candidate cannot reach the
// App Server, so it is never exercised - leaves the previous selection and pointer exactly as
// they were, releases only this run's directory, and status reads the host as it was.
func TestAFailedUpdateLeavesThePreviousRuntime(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, failed := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	before, _ := os.ReadFile(h.record)
	o := h.options()
	o.Socket = filepath.Join(t.TempDir(), "nobody-listens.sock")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "failedStep") != "exercise the candidate" || at(result, "retriable") != true || at(result, "removedCandidate") != failed {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != old {
		t.Fatalf("the pointer moved to %s", h.pointerTarget(t))
	}
	if _, err := os.Lstat(failed); !os.IsNotExist(err) {
		t.Fatal("the failed candidate's directory was left behind")
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil || claimState(t, old) != staging.Complete {
		t.Fatal("the previous runtime was touched")
	}
	rec := h.hostRecord(t)
	if at(rec, "selected", "codex-session-relay") != filepath.Join(old, "bin") {
		t.Fatalf("selection: %s", golden.Canon(at(rec, "selected")))
	}
	for _, raw := range golden.List(at(rec, "components", "codex-session-relay", "installs")) {
		if record.Get(golden.Obj(raw), "environment") == failed {
			t.Fatal("the failed candidate's install entry was kept")
		}
	}
	if string(before) == "" {
		t.Fatal("no record before")
	}
	status, _ := install.Status(context.Background(), h.options())
	if at(status, "runtime", "targetResolves") != old || at(status, "runtime", "agrees") != true || len(golden.List(at(status, "runtimes"))) != 1 {
		t.Fatalf("status: %s", golden.Canon(status))
	}
}

// An archive whose bytes do not hash to what SHA256SUMS lists is refused before anything is
// unpacked: nothing under the destination and no host record is created.
func TestADigestMismatchIsRefusedBeforeUnpacking(t *testing.T) {
	h := newHost(t)
	path := archive(t, "0.9.0", "")
	write(t, filepath.Join(filepath.Dir(path), install.SumsName), "0000000000000000000000000000000000000000000000000000000000000000  "+filepath.Base(path)+"\n")
	result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: path})
	if code != install.Refused || at(result, "applied") != false {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Lstat(h.dest); !os.IsNotExist(err) {
		t.Fatal("the destination was created for an archive that failed verification")
	}
	if _, err := os.Lstat(h.record); !os.IsNotExist(err) {
		t.Fatal("the host record was written for an archive that failed verification")
	}
	for name, sums := range map[string]string{
		"unlisted": "", "listed twice": archiveDigest(t, path) + "  " + filepath.Base(path) + "\n" + archiveDigest(t, path) + "  " + filepath.Base(path) + "\n",
	} {
		write(t, filepath.Join(filepath.Dir(path), install.SumsName), sums)
		if _, code := install.Install(context.Background(), h.options(), "install", install.Source{From: path}); code != install.Refused {
			t.Fatalf("%s: exit %d", name, code)
		}
	}
	if _, err := os.Lstat(h.dest); !os.IsNotExist(err) {
		t.Fatal("the destination was created")
	}
}
