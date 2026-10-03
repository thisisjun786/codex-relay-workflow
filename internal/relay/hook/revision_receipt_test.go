package hook

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestRevisionTurnStagedReceiptReleasesTheStopGuard(t *testing.T) {
	// Keep the original fixture's path: its manifest revision includes artifact paths.
	root := canonicalRootNamed(t, "Test33GuardBinaryPython")
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	for _, mode := range []string{Observe, Hold} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			base := filepath.Join(root, "ready_receipted")
			if err := os.MkdirAll(base, 0o700); err != nil {
				t.Fatal(err)
			}
			layFixture(t, "guard-ready_receipted", base)
			path := filepath.Join(base, "state", "relay.sqlite3")
			s, err := fixtureStore(ctx, path, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			raw, err := os.ReadFile(filepath.Join(base, "stop.json"))
			if err != nil {
				t.Fatal(err)
			}
			stop, err := decodeObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			options := GuardOptions{Root: filepath.Join(base, "markers"), DBPath: path, Mode: mode, Now: "2026-01-01T00:06:00Z"}
			first, err := Evaluate(ctx, stop, options)
			if err != nil || first.Get("state") != "declared_ready_receipted" {
				t.Fatalf("generation 1: %v, %v", first, err)
			}
			directory, marker, unreadable, err := delivery.SelectAssignment(ctx, options.Root, stop.Get("cwd").(string), "child")
			if err != nil || len(unreadable) != 0 {
				t.Fatalf("marker: %v, %v", unreadable, err)
			}
			registration := filepath.Join(directory, "relationship.json")
			registered, err := os.ReadFile(registration)
			if err != nil {
				t.Fatal(err)
			}
			rid := object(marker.Get("relationship")).Get("relationshipId").(string)
			var receiptText string
			if err := s.DB.QueryRowContext(ctx, "SELECT receipt FROM events WHERE relationship_id = ? AND producer = 'child'", rid).Scan(&receiptText); err != nil {
				t.Fatal(err)
			}
			payload, err := decodeObject([]byte(receiptText))
			if err != nil {
				t.Fatal(err)
			}
			initial := payload.Get("eventId").(string)
			// Seed the prior accepted acknowledgement; the correction itself uses real APIs.
			if _, err := s.DB.ExecContext(ctx, "UPDATE relationships SET allowed_recipients = '[\"parent\",\"child\"]' WHERE relationship_id = ?", rid); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, "INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) VALUES (?, '{}', 'review-turn', 1, 'verified', ?)", initial, options.Now); err != nil {
				t.Fatal(err)
			}
			clock := delivery.NewFakeClock()
			ack := delivery.NewAck(delivery.NewService(s, clock))
			if _, err := ack.RecordVerdict(ctx, initial, "needs_changes", "review-turn", nil, nil, "correct the result", nil); err != nil {
				t.Fatal(err)
			}
			turn := "revision-turn-2"
			if _, err := delivery.BindAnchor(ctx, s, clock, rid, 2, turn); err != nil {
				t.Fatal(err)
			}
			attempt := 1
			event, err := store.EventID(rid, 2, payload.Get("revisionHash").(string), "ready_for_review", turn, &attempt)
			if err != nil {
				t.Fatal(err)
			}
			payload = payload.Set("eventId", event).Set("executionGeneration", int64(2)).Set("turnRef", Object{{Key: "threadId", Value: "child"}, {Key: "turnId", Value: turn}, {Key: "turnStatus", Value: "inProgress"}})
			intake := store.ReceiptIntake{Store: s, Now: clock.ISO, Minimum: store.BestEffortDetection}
			staged, err := intake.AcceptChildReceiptWith(ctx, []byte(pyjson.Dumps(payload, pyjson.Options{})), store.TurnReference{ThreadID: "child", TurnID: turn, Status: "inProgress"}, store.AcceptOptions{})
			if err != nil || staged.Stage != store.StageStaged {
				t.Fatalf("staging: %+v, %v", staged, err)
			}
			assignment := filepath.Base(directory)
			if _, err := delivery.PublishDisposition(ctx, options.Root, stop.Get("cwd").(string), assignment, "child", turn, "ready_for_review", options.Now); err != nil {
				t.Fatal(err)
			}
			stop = stop.Set("turn_id", turn)
			var dispatch string
			if err := s.DB.QueryRowContext(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", rid).Scan(&dispatch); err != nil {
				t.Fatal(err)
			}
			current, readable, err := LookupReceipt(ctx, path, nil, rid, "child", turn, int64(2), dispatch)
			if err != nil || !readable || current.Get("atCurrentHead") != true {
				t.Fatalf("current generation control: %v, %v, %v", current, readable, err)
			}
			disposition, _ := delivery.ReadDisposition(ctx, directory, "child", turn)
			if ClassifyDeclaration(Observation{Stop: stop, Marker: marker, Disposition: disposition, Receipt: current}) != "declared_ready_receipted" {
				t.Fatal("matched receipt and disposition did not declare readiness")
			}
			t.Log("generation 1 unchanged; current-generation head and declaration controls pass")
			before := storeRows(t, path)
			verdict, err := Evaluate(ctx, stop, options)
			if err != nil || verdict.Get("state") != "declared_ready_receipted" || verdict.Get("decision") != "release" || verdict.Get("receiptEvidence") != "at_head" {
				t.Fatalf("generation 2 with original registration: %v, %v", verdict, err)
			}
			answer, valid := VerdictOutput(verdict)
			if !valid || answer != "" || strings.TrimSpace(pyjson.Text(verdict.Get("reason"))) == "" {
				t.Fatalf("release protocol: %q, %v, %v", answer, valid, verdict)
			}
			observation, err := os.ReadFile(filepath.Join(directory, verdict.Get("recordedAs").(string)+".json"))
			if err != nil {
				t.Fatal(err)
			}
			recorded, err := decodeObject(observation)
			if err != nil || recorded.Get("observation") != "declared_ready_receipted" || recorded.Get("held") != false {
				t.Fatalf("recorded Stop: %v, %v", recorded, err)
			}
			after, err := os.ReadFile(registration)
			if err != nil || string(after) != string(registered) || !slices.Equal(before, storeRows(t, path)) {
				t.Fatal("evaluation changed registration or relay facts", err)
			}
			if _, err := s.DB.ExecContext(ctx, "UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", event); err != nil {
				t.Fatal(err)
			}
			missing, err := Evaluate(ctx, stop, options)
			if err != nil || missing.Get("observation") != "receipt_missing" || strings.TrimSpace(pyjson.Text(missing.Get("reason"))) == "" {
				t.Fatalf("missing receipt reason: %v, %v", missing, err)
			}
		})
	}
}
