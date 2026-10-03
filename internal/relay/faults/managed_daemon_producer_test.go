package faults

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A turn that ended failed or interrupted, for which the daemon wrote its own observation event, has
// been reported: the managed observer answers reported / daemon_execution_report. The event is
// written by the writer the daemon uses, so the producer the observer looks for is the one stored;
// it never matched while the query spelled it as a literal that is not ProducerDaemon.
func TestCRW263ManagedObserverAnswersReportedForADaemonObservation(t *testing.T) {
	for _, c := range []struct {
		name, status, event string // event is whose observation the daemon wrote: own, other or none
		state, reason       string
	}{
		{"an interrupted turn with the daemon's event", "interrupted", "own", "reported", "daemon_execution_report"},
		{"a failed turn with the daemon's event", "failed", "own", "reported", "daemon_execution_report"},
		{"control: an interrupted turn with no event", "interrupted", "none", "unreported", "terminal_without_report"},
		{"control: the event is another turn's", "failed", "other", "unreported", "terminal_without_report"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, gd := f1ReplayStores(t)
			root, work := filepath.Dir(gd)+"/markers", filepath.Dir(gd)+"/workspace"
			f1Seed(t, ctx, gd, []string{f1Relationship,
				fmt.Sprintf("UPDATE relationships SET child_cwd='%s'", work),
				"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel',1,'dispatch-1','bound','turn-9','stamp')",
				fmt.Sprintf("INSERT INTO assignment_settlements VALUES('rel','child','turn-9','%s','stamp')", c.status),
				// turn-10 is admitted to the generation, so the daemon may observe it.
				"INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('rel',1,'turn-10','explicit_admission_bound:turn-9','child','admitted','stamp')"})
			workspaceHash, dispatchHash := sha256.Sum256([]byte(work)), sha256.Sum256([]byte("dispatch-1"))
			assignment := fmt.Sprintf("%x", dispatchHash)
			dir := filepath.Join(root, fmt.Sprintf("%x", workspaceHash), assignment)
			fcMarker(t, dir, "intent.json", map[string]any{"dispatchRequestIdHash": assignment, "workspace": work, "dbPath": gd + "/relay.sqlite3", "issueKey": "ISSUE"})
			fcMarker(t, dir, "claims/child/claim.json", map[string]any{"sessionId": "child", "dispatchRequestId": "dispatch-1"})
			fcMarker(t, dir, "bound.json", map[string]any{"sessionId": "child", "taskId": "child"})
			fcMarker(t, dir, "relationship.json", map[string]any{"relationshipId": "rel", "executionGeneration": 1})
			fcMarker(t, dir, "hook/child/turn-9/1.json", map[string]any{"sessionId": "child", "turnId": "turn-9", "observation": "undeclared_turn_end", "decisionState": "unresolved_handoff", "at": "1970-01-02T03:46:40+00:00"})

			s := fcOpen(t, ctx, gd)
			clock := &testClock{now: 100000}
			intake := store.ReceiptIntake{Store: s, Now: clock.ISO}
			observed := map[string]string{"own": "turn-9", "other": "turn-10"}[c.event]
			if observed != "" {
				if _, err := intake.DaemonObservation(ctx, "rel", store.TurnReference{ThreadID: "child", TurnID: observed, Status: c.status}); err != nil {
					t.Fatalf("the daemon's observation was not written: %v", err)
				}
				rows, err := s.All(ctx, "SELECT producer,stage,outcome,turn_status FROM events WHERE relationship_id='rel' AND turn_id=?", observed)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Text("producer") != store.ProducerDaemon || rows[0].Text("stage") != "final" || rows[0].Text("outcome") != c.status || rows[0].Text("turn_status") != c.status {
					t.Fatalf("the stored event is not the daemon's final observation of the turn: %v", rows)
				}
			}
			reading, err := realObserver(t).Observe(ctx, ManagedReadingRequest{Selection: store.StateSelection{Path: gd}, Root: root, Workspace: work, Assignment: assignment, Session: "child", Turn: "turn-9", Now: clock.ISO()})
			if err != nil {
				t.Fatal(err)
			}
			answer, _ := reading.(map[string]any)
			if answer["reportingState"] != c.state || answer["reason"] != c.reason {
				t.Fatalf("the observation reads %v / %v, want %s / %s", answer["reportingState"], answer["reason"], c.state, c.reason)
			}
		})
	}
}
