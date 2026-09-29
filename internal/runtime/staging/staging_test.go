package staging_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func claimOf(kind string) reading.Reading {
	switch kind {
	case "ABSENT":
		return reading.Reading{State: reading.Absent}
	case "UNREADABLE":
		return reading.Reading{State: reading.Unreadable, Detail: "the claim detail"}
	case "ACCESS_ERROR":
		return reading.Reading{State: reading.AccessError, Detail: "the claim detail"}
	}
	return reading.Reading{State: reading.Present, Value: record.Object{{Key: "state", Value: kind}}}
}

// staging.decide's whole table (5 claim readings x 3 livenesses x 3 listings x protected x
// selected) decides the same in Go, reason for reason, except the retired RECORDED branch: a
// populated directory without a claim is FOREIGN whatever the record selects.
func TestDecideIsPythonsTable(t *testing.T) {
	rows := golden.List(golden.Section(t, "stagingDecisions"))
	if len(rows) != 180 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, raw := range rows {
		row := golden.Obj(raw)
		var occupied *bool
		if v, ok := record.Get(row, "occupied").(bool); ok {
			occupied = &v
		}
		decision, reason := staging.Decide(claimOf(record.Text(row, "claim")), record.Text(row, "liveness"), occupied, record.Get(row, "protected") == true, record.Get(row, "selected") == true)
		want, wantReason := record.Text(row, "decision"), record.Text(row, "reason")
		if want == "RECORDED" {
			if decision != staging.Foreign || staging.Removes(decision) {
				t.Errorf("%s: the retired RECORDED case decided %s", golden.Canon(row), decision)
			}
			continue
		}
		if decision != want || reason != wantReason {
			t.Errorf("%s\n go: %s %s", golden.Canon(row), decision, reason)
		}
	}
}

// The claim's bytes are json.dumps(claim_payload(...), indent=2, sort_keys=True) plus a newline,
// with the same key set.
func TestClaimBytesArePythons(t *testing.T) {
	want := golden.Obj(golden.Section(t, "claimPayload"))
	payload := staging.Payload(staging.Staging, staging.WrittenByPython, "CRW-157", "42", 4242, "host", "2026-09-29T00:00:00Z")
	if got := string(record.Encode(payload)); got != record.Get(want, "bytes") {
		t.Fatalf("\n go: %q\n py: %q", got, record.Get(want, "bytes"))
	}
	var keys []string
	for _, f := range payload {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	if got, wanted := golden.Canon(keys), golden.Canon(record.Get(want, "keys")); got != wanted {
		t.Fatalf("key sets differ: %s %s", got, wanted)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Only a claim this command wrote is a claim: anything else at that path is somebody's file.
func TestReadClaimKeepsFourAnswersAndOwnership(t *testing.T) {
	dir := t.TempDir()
	valid := func(writer string) string {
		return string(record.Encode(staging.Payload(staging.Staging, writer, nil, nil, 1, "h", "t")))
	}
	for _, c := range []struct {
		name, text, state string
	}{
		{"python", valid(staging.WrittenByPython), reading.Present},
		{"go", valid(staging.WrittenByGo), reading.Present},
		{"broken", "{ not json", reading.Unreadable},
		{"halfway", `{"claimVersion": 1, "state": "HALFWAY", "writtenBy": "runtime_install.py"}`, reading.Unreadable},
		{"foreign", `{"claimVersion": 1, "state": "STAGING", "writtenBy": "someone"}`, reading.Unreadable},
		{"version", `{"claimVersion": 2, "state": "STAGING", "writtenBy": "runtime_install.py"}`, reading.Unreadable},
		{"bool-version", `{"claimVersion": true, "state": "STAGING", "writtenBy": "runtime_install.py"}`, reading.Unreadable},
		{"list", `[]`, reading.Unreadable},
	} {
		env := filepath.Join(dir, c.name)
		write(t, staging.ClaimPath(env), c.text)
		if got := staging.ReadClaim(env); got.State != c.state {
			t.Errorf("%s: %s %s", c.name, got.State, got.Detail)
		}
	}
	if got := staging.ReadClaim(filepath.Join(dir, "none")); got.State != reading.Absent || got.Value != nil {
		t.Errorf("an absent claim: %+v", got)
	}
	if err := os.MkdirAll(staging.ClaimPath(filepath.Join(dir, "dir")), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := staging.ReadClaim(filepath.Join(dir, "dir")); got.State != reading.Unreadable {
		t.Errorf("a directory at the claim path: %s", got.State)
	}
}

// Liveness is the lock and never a pid: DEAD only when the lock is free (or never taken),
// LIVE while any process holds it.
func TestLivenessIsTheAdvisoryLock(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env")
	if err := os.MkdirAll(env, 0o755); err != nil {
		t.Fatal(err)
	}
	if state, _ := staging.OwnerLiveness(env); state != staging.Dead {
		t.Fatalf("never locked: %s", state)
	}
	held, err := staging.Take(env)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := staging.OwnerLiveness(env); state != staging.Live {
		t.Fatalf("held here: %s", state)
	}
	if _, err := staging.Take(env); err == nil {
		t.Fatal("a held staging lock was taken twice")
	}
	held.Release()
	if state, _ := staging.OwnerLiveness(env); state != staging.Dead {
		t.Fatalf("released: %s", state)
	}
	_, release := golden.Spawn(t, "", staging.LockPath(env))
	if state, _ := staging.OwnerLiveness(env); state != staging.Live {
		t.Fatalf("held by another process: %s", state)
	}
	release()
	if state, _ := staging.OwnerLiveness(env); state != staging.Dead {
		t.Fatalf("its holder ended: %s", state)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(staging.LockPath(env), 0); err != nil {
			t.Fatal(err)
		}
		if state, _ := staging.OwnerLiveness(env); state != staging.Unknown {
			t.Fatalf("an unopenable lock: %s", state)
		}
	}
}

// StagingClaimTests: an abandoned staging is reclaimed, a live one is refused, an owner nobody
// could establish keeps the directory, a finished and selected one is settled.
func TestAbandonedStagingIsReclaimedAndLiveStagingIsNot(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	if err := staging.WriteClaim(env, staging.NewPayload(staging.Staging, nil, nil)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(env, "half-built"), "x")
	liveness, _ := staging.OwnerLiveness(env)
	occupied, _ := staging.DirectoryOccupied(env)
	if decision, why := staging.Decide(staging.ReadClaim(env), liveness, occupied, false, false); decision != staging.Reclaim || !staging.Removes(decision) {
		t.Fatalf("abandoned: %s %s", decision, why)
	}
	held, err := staging.Take(env)
	if err != nil {
		t.Fatal(err)
	}
	liveness, _ = staging.OwnerLiveness(env)
	held.Release()
	if decision, _ := staging.Decide(staging.ReadClaim(env), liveness, occupied, false, false); decision != staging.Occupied || staging.Removes(decision) {
		t.Fatalf("live: %s", decision)
	}
	if decision, _ := staging.Decide(staging.ReadClaim(env), staging.Unknown, occupied, false, false); decision != staging.Keep {
		t.Fatalf("unknown owner: %s", decision)
	}
	if decision, _ := staging.Decide(staging.ReadClaim(env), staging.Dead, occupied, true, true); staging.Removes(decision) {
		t.Fatalf("in use: %s", decision)
	}
	if err := staging.WriteClaim(env, staging.NewPayload(staging.Complete, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if decision, _ := staging.Decide(staging.ReadClaim(env), staging.Dead, occupied, true, true); decision != staging.Settled {
		t.Fatalf("finished and selected: %s", decision)
	}
	if !staging.IsSettled(staging.ReadClaim(env)) {
		t.Fatal("a COMPLETE claim is settled")
	}
}

// A Go staging is its final directory, made with an exclusive mkdir and claimed STAGING while
// its lock is held; a second Create of the same name is refused as not this run's.
func TestCreateClaimsAnExclusiveDirectory(t *testing.T) {
	final := filepath.Join(t.TempDir(), "dest", "bin-0.3.0-aaaaaaaaaaaa")
	held, err := staging.Create(final, "CRW-158", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	claim := staging.ReadClaim(final)
	if claim.State != reading.Present || record.Get(claim.Value.(record.Object), "state") != staging.Staging || record.Get(claim.Value.(record.Object), "writtenBy") != staging.WrittenByGo {
		t.Fatalf("the staging claim: %+v", claim)
	}
	if state, _ := staging.OwnerLiveness(final); state != staging.Live {
		t.Fatalf("a staging in progress: %s", state)
	}
	if _, err := staging.Create(final, nil, nil); !errors.Is(err, staging.ErrNotOwned) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("a second Create took an existing directory: %v", err)
	}
	held.Release()
	if state, _ := staging.OwnerLiveness(final); state != staging.Dead {
		t.Fatalf("a released staging: %s", state)
	}
	occupied, _ := staging.DirectoryOccupied(final)
	if decision, _ := staging.Decide(staging.ReadClaim(final), staging.Dead, occupied, false, false); decision != staging.Reclaim {
		t.Fatalf("an abandoned Go staging: %s", decision)
	}
}

func TestClearOwnRemovesOnlyItsOwnFiles(t *testing.T) {
	env := filepath.Join(t.TempDir(), "env")
	write(t, staging.ClaimPath(env), "{}")
	write(t, staging.LockPath(env), "")
	write(t, filepath.Join(env, "keep"), "x")
	if removed := staging.ClearOwn(env); len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(env, "keep")); err != nil {
		t.Fatal("somebody's file was removed")
	}
	occupied, detail := staging.DirectoryOccupied(env)
	if occupied == nil || !*occupied || detail != "it holds 1 entries besides this command's own" {
		t.Fatalf("occupied: %v %s", occupied, detail)
	}
	if occupied, _ := staging.DirectoryOccupied(filepath.Join(env, "missing")); occupied != nil {
		t.Fatal("an unlistable directory is not an empty one")
	}
}
