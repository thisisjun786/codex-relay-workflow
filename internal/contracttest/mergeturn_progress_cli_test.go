package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-408 criteria c1 to c4 through the built crw: the two new commands' registration, arguments,
// answers, exit codes and refusals. The answers carry elapsed seconds that depend on the real
// clock, so this asserts parsed fields rather than golden bytes; the turn's clock is moved
// by rewriting its recorded times in a temporary store.

type relayRun struct {
	t      *testing.T
	binary string
	home   string
	state  string
}

func (r *relayRun) run(args ...string) (map[string]any, int) {
	r.t.Helper()
	argv := append([]string{"relay", "--state", r.state}, args...)
	command := exec.Command(r.binary, argv...)
	command.Dir = r.home
	command.Env = append(os.Environ(), "HOME="+r.home, "XDG_STATE_HOME="+r.home+"/xs", "XDG_DATA_HOME="+r.home+"/xd",
		"XDG_CONFIG_HOME="+r.home+"/xc", "CODEX_HOME="+r.home+"/ch", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY=")
	var stdout bytes.Buffer
	command.Stdout = &stdout
	exit := 0
	if err := command.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatal(err)
		}
		exit = exitErr.ExitCode()
	}
	var answer map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &answer)
	return answer, exit
}

func (r *relayRun) ok(args ...string) map[string]any {
	r.t.Helper()
	answer, exit := r.run(args...)
	if exit != 0 {
		r.t.Fatalf("%v exited %d: %v", args, exit, answer)
	}
	return answer
}

func (r *relayRun) refused(reason string, args ...string) map[string]any {
	r.t.Helper()
	answer, exit := r.run(args...)
	if exit != 2 || answer["reason"] != reason {
		r.t.Fatalf("%v: want a refusal %s with exit 2, got exit %d: %v", args, reason, exit, answer)
	}
	return answer
}

func object(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

func TestMergeTurnProgressPassAndReturn_the_built_crw_end_to_end(t *testing.T) {
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	r := &relayRun{t: t, binary: binary, home: home, state: filepath.Join(home, "state")}
	r.ok("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha", "--host", "host-a")
	r.ok("linkage-bind", "--role", "parent", "--scope", "PRJ-B", "--task", "task-beta", "--host", "host-b")
	held := r.ok("merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev", "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a", "--head", "head-a", "--ready")
	alphaTurn := held["turnId"].(string)
	waiting := r.ok("merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev", "--project", "PRJ-B", "--task", "task-beta", "--host", "host-b", "--head", "head-b", "--ready")
	betaTurn := waiting["turnId"].(string)

	// c1: the holder records progress; the target shows it.
	progress := object(t, r.ok("merge-turn-progress", "--turn", alphaTurn, "--actor", "task-alpha", "--step", "base_refresh", "--evidence", "merged dev into the candidate")["progress"])
	if progress["step"] != "base_refresh" || progress["sequence"] != float64(1) {
		t.Fatal(progress)
	}
	if _, exit := r.run("merge-turn-progress", "--turn", alphaTurn, "--actor", "task-alpha", "--step", "lunch"); exit == 0 {
		t.Fatal("a step outside the closed list is refused")
	}
	r.refused("merge_turn_not_held", "merge-turn-progress", "--turn", alphaTurn, "--actor", "task-beta", "--step", "ci_started")
	shown := r.ok("merge-turn-show", "--repository", "owner/repo", "--base-ref", "dev")
	if object(t, shown["lastProgress"])["step"] != "base_refresh" || shown["stalled"] != false || shown["holdingLimitSeconds"] != float64(1200) {
		t.Fatal(shown)
	}

	// c2: a turn inside its limit is not passed; the same turn, silent past it, is.
	r.refused("merge_turn_not_held", "merge-turn-pass", "--turn", alphaTurn, "--actor", "task-beta", "--evidence", "too early")
	past := registry.ISO(time.Now().Add(-2 * time.Hour))
	s, err := store.Open(context.Background(), filepath.Join(r.state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(context.Background(), "UPDATE merge_turns SET held_at = ? WHERE turn_id = ?", past, alphaTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(context.Background(), "UPDATE merge_turn_ledger SET recorded_at = ? WHERE turn_id = ?", past, alphaTurn); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	shown = r.ok("merge-turn-show", "--repository", "owner/repo", "--base-ref", "dev")
	if shown["stalled"] != true || object(t, shown["blocked"])["cause"] != "holder_stalled" {
		t.Fatal(shown)
	}
	r.refused("scope_role_mismatch", "merge-turn-pass", "--turn", alphaTurn, "--actor", "task-nobody", "--evidence", "not a waiter")
	passed := r.ok("merge-turn-pass", "--turn", alphaTurn, "--actor", "task-beta", "--evidence", "task-alpha has been silent for two hours")
	if object(t, passed["released"])["state"] != "passed" || object(t, passed["promoted"])["turnId"] != betaTurn || object(t, passed["promoted"])["state"] != "holding" {
		t.Fatal(passed)
	}
	if object(t, passed["passed"])["passedBy"] != "task-beta" || object(t, passed["passed"])["limitSeconds"] != float64(1200) {
		t.Fatal(passed["passed"])
	}

	// c3: the original holder is refused, with a reason that says what happened.
	for _, args := range [][]string{
		{"merge-turn-land", "--turn", alphaTurn, "--actor", "task-alpha", "--landed-sha", "merge-1", "--evidence", "merged"},
		{"merge-turn-release", "--turn", alphaTurn, "--actor", "task-alpha", "--disposition", "returned", "--reason", "done"},
		{"merge-turn-progress", "--turn", alphaTurn, "--actor", "task-alpha", "--step", "ci_result"},
	} {
		answer := r.refused("merge_turn_not_held", args...)
		if detail, _ := answer["detail"].(string); !strings.Contains(detail, "passed") || !strings.Contains(detail, "task-beta") {
			t.Fatalf("%s: %v", args[0], answer)
		}
	}

	// c4: a return request on the new holder's turn is recorded; this turn names no assignment,
	// so the notice is reported unaddressed, not lost.
	requested := r.ok("merge-turn-request-return", "--turn", betaTurn, "--actor", "task-alpha", "--evidence", "I need the lane back")
	if object(t, requested["returnNotice"])["state"] != "unaddressed" {
		t.Fatal(requested["returnNotice"])
	}
}
