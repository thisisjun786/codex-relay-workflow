package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func Test28_EmitDeliverClaimAckRoundTrip(t *testing.T) {
	root := t.TempDir()
	repo, _ := filepath.Abs("../../..")
	binary, alias := suiteBinary, suiteAlias
	seed := copyDeliverySeed(t, root)
	goState, pyState := filepath.Join(root, "go"), filepath.Join(root, "python")
	for _, pair := range [][2]string{{"go.sqlite3", goState}, {"python.sqlite3", pyState}} {
		if err := os.Mkdir(pair[1], 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, pair[0]), filepath.Join(pair[1], "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(context.Background(), filepath.Join(pair[1], "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		testsupport.FencePythonFixture(t, s.DB, s.Path, "", pair[1] == pyState)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	host := fakehost.Start(t)
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		data := []any{}
		if p["archived"] != true && p["sourceKinds"] == nil {
			data = append(data, map[string]any{"id": "01parent-task"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}})
	host.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		data := []any{}
		if p["threadId"] == "01child-task" {
			data = append(data, map[string]any{"id": "turn-dispatch-1", "status": "completed", "startedAt": 1699999900})
		} else {
			data = append(data, map[string]any{"id": "ack-turn", "status": "inProgress", "startedAt": 1700000001})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	parent := resume()
	parent["cwd"] = "/parent"
	parent["runtimeWorkspaceRoots"] = []any{"/parent"}
	parent["thread"].(map[string]any)["environments"] = []any{map[string]any{"environmentId": "local", "cwd": "/parent", "runtimeWorkspaceRoots": []any{"/parent"}}}
	host.Respond("thread/resume", fakehost.Reply{Result: parent})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "delivery-turn"}}})
	// Both stores share the same seeded identities and data. Remove the prepared
	// event so the first command proves admission, not just re-observation.
	for _, state := range []string{goState, pyState} {
		s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Querier(context.Background()).ExecContext(context.Background(), "DELETE FROM deliveries; DELETE FROM events; DELETE FROM journal WHERE kind IN ('event_accepted','delivery_enqueued')"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	oldClock, oldHost, oldObserve := delivery.CommandClock, delivery.HostCommand, delivery.ObserveTurn
	delivery.CommandClock = delivery.NewFakeClock()
	delivery.HostCommand = hostCommand
	delivery.ObserveTurn = observeTurn
	t.Cleanup(func() {
		delivery.CommandClock = oldClock
		delivery.HostCommand = oldHost
		delivery.ObserveTurn = oldObserve
	})
	invocations := 0
	run := func(args ...string) map[string]any {
		t.Helper()
		var got, stderr bytes.Buffer
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(root, "go-ledger"))
		argv := append([]string{"--state", goState, "--socket", host.SocketPath}, args...)
		program := alias
		if invocations%2 == 0 {
			program = binary
			argv = append([]string{"relay"}, argv...)
		}
		invocations++
		goCmd := exec.Command(program, argv...)
		goCmd.Stdout = &got
		goCmd.Stderr = &stderr
		code := 0
		if err := goCmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(map[string]any{"argv": append([]string{"--state", pyState, "--socket", host.SocketPath}, args...)})
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/roundtrip_capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(raw)
		cmd.Env = append(os.Environ(), "CODEX_SESSION_RELAY_STATE="+filepath.Join(root, "python-ledger"))
		var want, pyErr bytes.Buffer
		cmd.Stdout = &want
		cmd.Stderr = &pyErr
		err := cmd.Run()
		pyCode := 0
		if exit, ok := err.(*exec.ExitError); ok {
			pyCode = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != pyCode || !bytes.Equal(got.Bytes(), want.Bytes()) || !bytes.Equal(stderr.Bytes(), pyErr.Bytes()) {
			t.Fatalf("%v Go(%d) %s %s Python(%d) %s %s", args, code, &got, &stderr, pyCode, &want, &pyErr)
		}
		if code != 0 {
			t.Fatalf("unexpected round trip exit %d: %v: %s", code, args, &got)
		}
		var answer map[string]any
		if err := json.Unmarshal(got.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	emit := []string{"emit", "--relationship", "rel-1", "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", "01child-task", "--turn-id", "turn-dispatch-1", "--turn-status", "completed", "--artifact", filepath.Join(seed.Work, "out.txt")}
	// The seed relationship has Python's derived id, not the symbolic rel-1.
	s, err := store.Open(context.Background(), filepath.Join(goState, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	var rid string
	if err := s.Querier(context.Background()).QueryRowContext(context.Background(), "SELECT relationship_id FROM relationships").Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	emit[2] = rid
	first := run(emit...)
	if first["stage"] != "final" {
		t.Fatalf("emit did not finalize: %v", first)
	}
	duplicate := run(emit...)
	if duplicate["duplicate"] != true {
		t.Fatalf("duplicate not recognized: %v", duplicate)
	}
	// The revision identity outranks a changed rerun counter and predecessor claim:
	// Python replays the retained receipt rather than manufacturing a refusal.
	reemitted := run(append(append([]string{}, emit...), "--attempt", "2", "--supersedes-revision", first["receipt"].(map[string]any)["revisionHash"].(string))...)
	if reemitted["duplicate"] != true {
		t.Fatalf("same revision not retained: %v", reemitted)
	}
	dispatch := run("deliver", "--event", seed.Event)
	if dispatch["attempt"].(map[string]any)["deliveryState"] != "dispatched" {
		t.Fatalf("not dispatched: %v", dispatch)
	}
	run("claim", "--event", seed.Event, "--turn", "ack-turn")
	ack := run("ack", "--event", seed.Event, "--ack-turn", "ack-turn", "--ack-proof", delivery.AckProof(seed.Event, "ack-turn"))
	if ack["_verified"] != "verified" {
		t.Fatalf("ack unverified: %v", ack)
	}
	left, err := store.Open(context.Background(), filepath.Join(goState, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := store.Open(context.Background(), filepath.Join(pyState, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	actual, _ := json.Marshal(allTables(t, left))
	expected, _ := json.Marshal(allTables(t, right))
	if !bytes.Equal(actual, expected) {
		t.Fatalf("all table mismatch\nGo %s\nPython %s", actual, expected)
	}
}
