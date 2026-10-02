package managed

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The routing text managed-start sends its child ends the routing record with the instruction a
// continuation claim has to follow. These tests hold that instruction to one rule: it names the anchor
// of the generation the text is for. Generation 1's anchor is the standby turn the record carries as
// standbyTurnId; a later generation is anchored to the turn its revision request arrived in, so its
// text names that turn and not standbyTurnId.
//
// managed-start renders the text once, in generation 1: registration records generation 1
// (registry.attachManagedRegistration), and the final guard of the send and the replay check both
// refuse a relationship that has moved on (stale_generation). The later-generation wording is
// therefore proved on the formatter, with the standby turn and the generation's anchor as separate
// inputs, and TestRoutingText_ALaterGenerationIsNeverSent shows the guard that keeps it from being sent.

// generationOneRoutingText is what Start.packet rendered for the row built by routingRow before the text
// became a function of the generation. It is a frozen literal on purpose: the bridge ledger fingerprints
// the message of a send under its request id, so a request first sent before an upgrade and retried after
// it must render the same bytes, or the retry ends in "request_id already belongs to different arguments".
const generationOneRoutingText = "Managed assignment routing record:\n" +
	"{\"assignmentId\":\"assignment-1\",\"dispatchRequestId\":\"managed-business-d\",\"executionGeneration\":1,\"markerRoot\":\"/marker\",\"relationshipId\":\"rel-1\",\"socket\":\"/sock\",\"standbyTurnId\":\"turn-a\",\"state\":\"/state\",\"taskId\":\"child-1\",\"workspace\":\"/ws\"}\n" +
	"First publish your own intent-claim using this task, assignment and dispatch request. Publish your own per-turn disposition; continuation claims must name standbyTurnId. Do not fabricate completion, ACK or verification. Report through the registered relay. The following is the authorized business assignment:\n\n" +
	"do the work"

// instructsStandbyTurn is the instruction that is only true in generation 1.
const instructsStandbyTurn = "continuation claims must name standbyTurnId"

func routingRow(generation int64) store.ManagedStartRequestsRow {
	return store.ManagedStartRequestsRow{
		ChildTaskID:         sql.NullString{String: "child-1", Valid: true},
		StandbyTurnID:       sql.NullString{String: "turn-a", Valid: true},
		RelationshipID:      sql.NullString{String: "rel-1", Valid: true},
		ExecutionGeneration: sql.NullInt64{Int64: generation, Valid: true},
	}
}

func TestRoutingText_GenerationOneIsTheBaselineText(t *testing.T) {
	m := &Start{Store: &store.Store{Path: "/state/relay.sqlite3"}, Socket: "/sock"}
	id := Identity{DispatchRequestID: "managed-business-d", Workspace: "/ws", MarkerRoot: "/marker"}
	got := m.packet(id, routingRow(1), map[string]any{"prompt": "do the work"}, "assignment-1")
	if got != generationOneRoutingText {
		t.Fatalf("generation 1 text changed:\n got: %q\nwant: %q", got, generationOneRoutingText)
	}
}

func TestRoutingText_EachGenerationNamesItsOwnAnchor(t *testing.T) {
	const record = "{\"standbyTurnId\":\"turn-a\"}"
	anchors := []string{"turn-a", "turn-b", "turn-c"}
	for generation, anchor := range anchors {
		number := int64(generation + 1)
		t.Run(fmt.Sprintf("generation %d", number), func(t *testing.T) {
			clause := continuationClaim(number, anchor)
			text := routingText(record, number, anchor, "do the work")
			if !strings.Contains(text, "; "+clause+". Do not fabricate") {
				t.Fatalf("the text does not carry the clause %q: %q", clause, text)
			}
			if number == 1 {
				if clause != instructsStandbyTurn {
					t.Fatalf("generation 1 clause = %q, want %q", clause, instructsStandbyTurn)
				}
				return
			}
			want := "continuation claims must name this generation's anchor turn " + anchor
			if !strings.Contains(clause, want) {
				t.Errorf("generation %d clause %q does not name its anchor (%q)", number, clause, want)
			}
			if !strings.Contains(clause, fmt.Sprintf("executionGeneration %d", number)) {
				t.Errorf("generation %d clause %q does not say which generation it is for", number, clause)
			}
			if strings.Contains(text, instructsStandbyTurn) {
				t.Errorf("generation %d text still instructs naming the standby turn: %q", number, text)
			}
			for _, other := range anchors {
				if other != anchor && strings.Contains(clause, other) {
					t.Errorf("generation %d clause %q names the anchor %s of another generation", number, clause, other)
				}
			}
		})
	}
}

// routingRun is one managed start over a fresh store, the fake host recording what it was asked to send.
type routingRun struct {
	t        *testing.T
	ctx      context.Context
	store    *store.Store
	host     *managedFake
	start    *Start
	raw      []byte
	issueKey string
}

func newRoutingRun(t *testing.T) *routingRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	return &routingRun{t: t, ctx: ctx, store: s, host: host, raw: raw, issueKey: str(req["issueKey"]),
		start: &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}}
}

func (x *routingRun) run() map[string]any {
	x.t.Helper()
	result, err := x.start.Run(x.ctx, x.raw)
	if err != nil {
		x.t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	return got
}

// A retry of a request whose business send was not attempted carries the same request id and has to
// carry the same message: the bridge ledger fingerprints it, and a different text is a conflict.
func TestRoutingText_RetryWithTheSameRequestIDSendsTheSameBytes(t *testing.T) {
	x := newRoutingRun(t)
	var sent []string
	x.host.beforeSend = func(in SendRequest) { sent = append(sent, in.RequestID+"\n"+in.Message) }
	x.host.sendStatus = "not_attempted"
	first := x.run()
	if first["state"] != "incomplete" || first["reason"] != "business_not_attempted" {
		t.Fatalf("first start: %v", first)
	}
	x.host.sendStatus = ""
	second := x.run()
	if second["state"] != "admitted" || second["businessTurnId"] != "business" || x.host.created != 1 || x.host.sent != 2 {
		t.Fatalf("retry: %v; effects %d created, %d sends", second, x.host.created, x.host.sent)
	}
	if len(sent) != 2 || sent[0] != sent[1] {
		t.Fatalf("the retry changed the message or its request id:\n first: %q\nsecond: %q", sent[0], sent[len(sent)-1])
	}
	if !strings.Contains(sent[0], "\"executionGeneration\":1,") || !strings.Contains(sent[0], "; "+instructsStandbyTurn+". Do not fabricate") {
		t.Fatalf("the generation 1 text lost its instruction: %q", sent[0])
	}
}

// A relationship that has moved to generation 2 before the business message goes is refused at the final
// guard, so managed-start never sends the first generation's text as a later generation's.
func TestRoutingText_ALaterGenerationIsNeverSent(t *testing.T) {
	x := newRoutingRun(t)
	var verdict string
	x.host.beforeSend = func(in SendRequest) {
		if _, err := x.store.DB.ExecContext(x.ctx, "UPDATE relationships SET execution_generation=2 WHERE issue_key=?", x.issueKey); err != nil {
			t.Fatal(err)
		}
		refused, err := in.BeforeStart(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		verdict = str(refused["code"])
	}
	got := x.run()
	if verdict != "stale_generation" || got["state"] == "admitted" || x.host.sent != 0 {
		t.Fatalf("a later generation reached the host: verdict=%q result=%v sends=%d", verdict, got, x.host.sent)
	}
}
