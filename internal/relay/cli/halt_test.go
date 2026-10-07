package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The CLI's half of CRW-848: with the halt marker present a writable command is refused
// store_write_halted at the open, whose detail names the marker file, while read-only commands and
// doctor still answer and doctor reports the marker.

// haltedState is a Go-owned store with the marker beside it, as a detection leaves it.
func haltedState(t *testing.T, name, marker string) string {
	t.Helper()
	home := tempHome(t)
	state := ownerOnlyState(t, home, name)
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	if err := os.WriteFile(filepath.Join(state, "corruption.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestHaltWritableCommandRefusesAndDoctorReports(t *testing.T) {
	state := haltedState(t, "halted", `{"detectedAt":"2026-10-06T13:44:00.000000+00:00","pid":1,"command":"crw relay","code":522,"message":"disk I/O error","site":"observation","sequence":1}`)
	got := goCLI(t, "--state", state, "store-challenge", "--write")
	if got.code != 2 || object(t, got.stdout)["reason"] != "store_write_halted" {
		t.Fatalf("a writable command was not refused store_write_halted: %d %s", got.code, got.stdout)
	}
	detail, _ := object(t, got.stdout)["detail"].(string)
	if !strings.Contains(detail, "corruption.json") || !strings.Contains(detail, "522") {
		t.Fatalf("the refusal does not name the marker and the detection: %q", detail)
	}
	// A read-only command still answers.
	status := goCLI(t, "--state", state, "status")
	if status.code != 0 {
		t.Fatalf("a read-only command refused: %d %s", status.code, status.stdout)
	}
	// And doctor reports the marker.
	answer := goCLI(t, "--state", state, "doctor")
	if answer.code != 0 {
		t.Fatalf("doctor refused: %d %s", answer.code, answer.stdout)
	}
	corruption, _ := object(t, answer.stdout)["corruption"].(map[string]any)
	if corruption == nil || corruption["present"] != true || corruption["code"] != 522.0 || corruption["site"] != "observation" {
		t.Fatalf("doctor did not report the marker: %v", object(t, answer.stdout)["corruption"])
	}
	if path, _ := corruption["path"].(string); !strings.HasSuffix(path, filepath.Join("halted", "corruption.json")) {
		t.Fatalf("doctor named %q", path)
	}
}

// TestHaltUnreadableMarkerStillHalts: a marker nobody can read is still a halt, and doctor says why
// rather than reporting a healthy store.
func TestHaltUnreadableMarkerStillHalts(t *testing.T) {
	state := haltedState(t, "unreadable", "not json")
	got := goCLI(t, "--state", state, "store-challenge", "--write")
	if got.code != 2 || object(t, got.stdout)["reason"] != "store_write_halted" {
		t.Fatalf("an unreadable marker did not refuse: %d %s", got.code, got.stdout)
	}
	answer := goCLI(t, "--state", state, "doctor")
	if answer.code != 0 {
		t.Fatalf("doctor refused: %d %s", answer.code, answer.stdout)
	}
	corruption, _ := object(t, answer.stdout)["corruption"].(map[string]any)
	if corruption == nil || corruption["present"] != true {
		t.Fatalf("doctor did not report the marker: %v", object(t, answer.stdout)["corruption"])
	}
	if detail, _ := corruption["detail"].(string); detail == "" {
		t.Fatalf("doctor reported an unreadable marker as healthy: %v", corruption)
	}
}

// TestHaltLeavesAHealthyStoreAlone: with no marker the writable command still writes and doctor adds
// no corruption key, so nothing else in the CLI surface moved.
func TestHaltLeavesAHealthyStoreAlone(t *testing.T) {
	home := tempHome(t)
	state := ownerOnlyState(t, home, "healthy")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	if got := goCLI(t, "--state", state, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("a healthy store refused a write: %d %s", got.code, got.stdout)
	}
	answer := goCLI(t, "--state", state, "doctor")
	if answer.code != 0 {
		t.Fatalf("doctor refused: %d %s", answer.code, answer.stdout)
	}
	if _, present := object(t, answer.stdout)["corruption"]; present {
		t.Fatalf("doctor reported a marker on a healthy store: %s", answer.stdout)
	}
}
