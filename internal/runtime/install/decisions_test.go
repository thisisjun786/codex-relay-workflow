package install_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// An existing directory of the candidate's name is decided on its claim before anything is
// built: installed and selected is reported as already installed, an abandoned staging (a
// STAGING claim whose lock nobody holds, nothing selecting it) is reclaimed and rebuilt, a
// finished runtime nothing selects is kept and refused, and a directory without a claim is
// somebody else's.
func TestAnExistingDirectoryIsDecidedOnItsClaim(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	env := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	again, code := install.Install(context.Background(), h.options(), "update", install.Source{From: first})
	if code != install.OK || at(again, "alreadyInstalled") != true || at(again, "stagingDecision") != staging.Settled {
		t.Fatalf("settled: exit %d\n%s", code, golden.Canon(again))
	}

	second := archive(t, "0.9.1", "")
	abandoned := runtimeDir(h, "0.9.1", second, t)
	write(t, filepath.Join(abandoned, "bin", "half-written"), "x")
	write(t, staging.ClaimPath(abandoned), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	h.mustInstall(t, "update", second)
	if _, err := os.Stat(filepath.Join(abandoned, "bin", "half-written")); !os.IsNotExist(err) {
		t.Fatal("the abandoned staging was not rebuilt")
	}

	// env is now finished and unselected: kept, and never rebuilt over.
	kept, code := install.Install(context.Background(), h.options(), "update", install.Source{From: first})
	if code != install.Refused || at(kept, "stagingDecision") != staging.Keep {
		t.Fatalf("finished and unselected: exit %d\n%s", code, golden.Canon(kept))
	}
	if claimState(t, env) != staging.Complete || h.pointerTarget(t) != abandoned {
		t.Fatal("a refusal changed something")
	}

	third := archive(t, "0.9.2", "")
	foreign := runtimeDir(h, "0.9.2", third, t)
	write(t, filepath.Join(foreign, "somebody"), "x")
	if refused, code := install.Install(context.Background(), h.options(), "update", install.Source{From: third}); code != install.Refused || at(refused, "stagingDecision") != staging.Foreign {
		t.Fatalf("no claim: exit %d\n%s", code, golden.Canon(refused))
	}
	if _, err := os.Stat(filepath.Join(foreign, "somebody")); err != nil {
		t.Fatal("somebody's directory was touched")
	}
}

// A run killed after committing its selection and before moving the pointer leaves a STAGING
// claim over a selection that names its directory. The next run resumes it: it moves the
// pointer and settles the claim, rebuilding nothing. When the claim cannot be settled (another
// writer holds its lock), the result is exit 3: promoted and in service, the record of it
// outstanding, with the recovery named - never exit 1, which would read as nothing happened.
func TestAnInterruptedPromotionIsResumedAndAnUnsettledClaimIsExit3(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, next := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	// Rewind to the interrupted state: the pointer back on the old runtime, the claim STAGING.
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
		t.Fatal(err)
	}
	write(t, staging.ClaimPath(next), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
	saved := record.LockTimeout
	record.LockTimeout = 200 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	write(t, staging.ClaimPath(next)+record.LockSuffix, "4242")
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Incomplete || at(result, "resumed") != true || at(result, "promoted") != true || at(result, "inService") != true || at(result, "claimSettled") != false {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if !strings.Contains(text(at(result, "recoveryRequires")), "wait for the run that holds") || h.pointerTarget(t) != next {
		t.Fatalf("recovery: %s, pointer %s", at(result, "recoveryRequires"), h.pointerTarget(t))
	}
	if err := os.Remove(staging.ClaimPath(next) + record.LockSuffix); err != nil {
		t.Fatal(err)
	}
	if settled, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second}); code != install.OK || at(settled, "resumed") != true || at(settled, "claimSettled") != true {
		t.Fatalf("the rerun finishes the bookkeeping: exit %d\n%s", code, golden.Canon(settled))
	}
	if settled, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second}); code != install.OK || at(settled, "alreadyInstalled") != true {
		t.Fatalf("then it is installed: exit %d\n%s", code, golden.Canon(settled))
	}
}

// Another run holding the promotion lock (flock, as Python's fcntl.flock takes it) makes the
// promotion refuse after the build: the candidate is released and nothing moved.
func TestAHeldPromotionLockRefusesAndReleasesTheCandidate(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	if err := os.MkdirAll(filepath.Dir(h.record), 0o755); err != nil {
		t.Fatal(err)
	}
	_, release := golden.Spawn(t, "", h.record+record.PromotionLockSuffix)
	defer release()
	saved := record.PromotionTimeout
	record.PromotionTimeout = 200 * time.Millisecond
	defer func() { record.PromotionTimeout = saved }()
	result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first})
	if code != install.Refused || at(result, "failedStep") != "take the promotion lock" || at(result, "retriable") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Lstat(runtimeDir(h, "0.9.0", first, t)); !os.IsNotExist(err) {
		t.Fatal("the candidate was kept")
	}
	if _, err := os.Lstat(pointer.Path(h.dest)); !os.IsNotExist(err) {
		t.Fatal("the pointer was placed")
	}
}

// Remove refuses a directory a live process runs out of, one with no claim of this command's,
// one whose staging another run still holds, and anything that is not a runtime directory
// directly under the destination; it accepts a Python env-* claim.
func TestRemoveRefusesWhatMayStillBeInUse(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)

	// The record's selection protects a runtime even while the pointer names another one.
	if err := pointer.Place(pointer.Path(h.dest), old); err != nil {
		t.Fatal(err)
	}
	if refused, code := install.Remove(context.Background(), h.options(), updated); code != install.Refused || !strings.Contains(text(at(refused, "refused")), "selects") {
		t.Fatalf("a selected runtime the pointer does not name: exit %d\n%s", code, golden.Canon(refused))
	}
	if err := pointer.Place(pointer.Path(h.dest), updated); err != nil {
		t.Fatal(err)
	}

	sleeper := filepath.Join(old, "bin", "sleep")
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sleeper, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(sleeper, "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	refused, code := install.Remove(context.Background(), h.options(), old)
	if code != install.Refused || len(golden.List(at(refused, "processes"))) != 1 {
		t.Fatalf("a live process: exit %d\n%s", code, golden.Canon(refused))
	}
	_ = process.Process.Kill()
	_ = process.Wait()

	held, err := staging.Take(old)
	if err != nil {
		t.Fatal(err)
	}
	if refused, code := install.Remove(context.Background(), h.options(), old); code != install.Refused || !strings.Contains(text(at(refused, "refused")), "still holds") {
		t.Fatalf("a held staging lock: exit %d\n%s", code, golden.Canon(refused))
	}
	held.Release()

	noClaim := filepath.Join(h.dest, "bin-0.0.1-000000000000")
	write(t, filepath.Join(noClaim, "bin", "crw"), "x")
	if _, code := install.Remove(context.Background(), h.options(), noClaim); code != install.Refused {
		t.Fatalf("no claim: exit %d", code)
	}
	elsewhere := filepath.Join(h.home, "bin-0.0.2-000000000000")
	write(t, staging.ClaimPath(elsewhere), string(record.Encode(staging.NewPayload(staging.Complete, nil, nil))))
	if _, code := install.Remove(context.Background(), h.options(), elsewhere); code != install.Refused {
		t.Fatalf("outside the destination: exit %d", code)
	}
	for _, dir := range []string{noClaim, elsewhere, old} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("a refusal removed %s", dir)
		}
	}

	venv := h.pythonVenv(t)
	if removed, code := install.Remove(context.Background(), h.options(), venv); code != install.OK || len(golden.List(at(removed, "droppedInstallEntries"))) != 1 {
		t.Fatalf("a settled Python venv: exit %d\n%s", code, golden.Canon(removed))
	}
	if removed, code := install.Remove(context.Background(), h.options(), old); code != install.OK {
		t.Fatalf("the settled old runtime once nothing runs from it: exit %d\n%s", code, golden.Canon(removed))
	}
}

// The command line: `crw install` dispatches its seven commands, rejects an unknown one and a
// missing directory as usage errors (2), and prints one JSON document.
func TestTheCommandLine(t *testing.T) {
	h := newHost(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, install.Usage, "required: command"},
		{[]string{"unpack"}, install.Usage, "invalid choice: 'unpack'"},
		{[]string{"remove"}, install.Usage, "required: directory"},
		{[]string{"status", "extra"}, install.Usage, "unrecognized arguments: extra"},
		{[]string{"install", "--from", filepath.Join(h.home, "crw.tar.gz")}, install.Refused, `"refused"`},
		{[]string{"status"}, install.OK, `"command": "status"`},
	} {
		var stdout, stderr strings.Builder
		args := append(append([]string{}, tc.args...), "--codex-home", h.codex, "--record", h.record)
		if len(tc.args) == 0 {
			args = nil
		}
		code := install.Main(context.Background(), args, h.env, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stdout.String()+stderr.String(), tc.want) {
			t.Errorf("%v: exit %d\nstdout %s\nstderr %s", tc.args, code, stdout.String(), stderr.String())
		}
	}
	var stdout strings.Builder
	if code := install.Main(context.Background(), []string{"status", "--record", h.record, "--codex-home", h.codex}, h.env, &stdout, &strings.Builder{}); code != install.OK {
		t.Fatal(code)
	}
	var buf strings.Builder
	_ = contract.Emit(&buf, record.Object{})
	if !strings.HasPrefix(stdout.String(), "{\n") {
		t.Fatalf("not one JSON document: %s", stdout.String())
	}
}

// A link at the pointer path that this host record never recorded placing is somebody else's:
// renaming over it would succeed whoever made it, so the promotion refuses and leaves it.
func TestAnUnrecordedPointerIsNotReplaced(t *testing.T) {
	h := newHost(t)
	elsewhere := filepath.Join(h.home, "somebody-else")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, pointer.Path(h.dest)); err != nil {
		t.Fatal(err)
	}
	first := archive(t, "0.9.0", "")
	result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first})
	if code != install.Refused || at(result, "failedStep") != "establish the pointer is this command's" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if h.pointerTarget(t) != elsewhere {
		t.Fatal("somebody's link was replaced")
	}
}

// OPS-4.4 is asked on the reading the promotion promotes on: a store holding a schema object the
// candidate does not declare (the candidate NARROWS it) blocks the swap, the candidate is
// released, and the runtime a host reaches is the one it reached before. A running daemon is
// TestARunningDaemonOfTheSelectedRuntimeBlocksTheSwap.
func TestAStoreSchemaTheCandidateNarrowsBlocksTheSwap(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	db, err := sql.Open("sqlite", filepath.Join(h.relayState, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE only_in_this_store (x)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
	if code != install.Refused || at(result, "failedStep") != "read whether it is safe to swap" || at(result, "swapGate", "verdict") != "BLOCKED" || at(result, "swapGate", "cells", "storeSchema", "answer") != "NARROWS" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if h.pointerTarget(t) != old || at(result, "retriable") != true {
		t.Fatalf("pointer %s", h.pointerTarget(t))
	}
}
