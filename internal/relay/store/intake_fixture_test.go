package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Fixture identities match Python's tests/support.py, so a foreign thread or unassigned turn
// is exactly what the intake checks are there to catch.
const (
	fixtureParent = "01parent-task"
	fixtureChild  = "01child-task"
	fixtureTurn   = "turn-dispatch-1"
	fixtureNow    = "2023-11-14T22:13:20.000000+00:00"
)

type intakeFixture struct {
	t            *testing.T
	store        *Store
	intake       ReceiptIntake
	root         string
	tmp          string
	relationship Relationship
}

type registerOptions struct {
	issue        string
	dispatchTurn sql.NullString
}

func newIntakeFixture(t *testing.T) *intakeFixture {
	t.Helper()
	s := recordStore(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &intakeFixture{t: t, store: s, root: root, tmp: tmp, intake: ReceiptIntake{Store: s, Now: func() string { return fixtureNow }, Minimum: BestEffortDetection}}
	f.relationship = f.register(registerOptions{issue: "REL-1", dispatchTurn: sql.NullString{String: fixtureTurn, Valid: true}})
	return f
}

func (f *intakeFixture) register(options registerOptions) Relationship {
	f.t.Helper()
	id, err := RelationshipID(fixtureParent, fixtureChild, options.issue)
	if err != nil {
		f.t.Fatal(err)
	}
	roots, err := json.Marshal([]string{f.root})
	if err != nil {
		f.t.Fatal(err)
	}
	relationship := Relationship{ID: id, IssueKey: options.issue, Status: StatusActive, ParentTaskID: fixtureParent, ChildTaskID: fixtureChild, Generation: 1, ArtifactRoots: string(roots), AllowedRecipients: `["` + fixtureParent + `"]`, CreatedAt: fixtureNow, UpdatedAt: fixtureNow}
	generation := Generation{RelationshipID: id, Number: 1, DispatchRequestID: "dispatch-" + options.issue, AnchorState: AnchorPending, DispatchTurnID: options.dispatchTurn, OpenedAt: fixtureNow}
	if options.dispatchTurn.Valid {
		generation.AnchorState = AnchorBound
		generation.BoundAt = sql.NullString{String: fixtureNow, Valid: true}
	}
	if err := f.store.RecordRelationship(context.Background(), relationship, generation, "host-a", "host-a"); err != nil {
		f.t.Fatal(err)
	}
	return relationship
}

func (f *intakeFixture) artifact(name, text string) string {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func assignedTurn(status string) TurnReference {
	return TurnReference{ThreadID: fixtureChild, TurnID: fixtureTurn, Status: status}
}

// receiptPayload is a completion receipt in Python's insertion order (support.ready_payload).
type receiptPayload struct {
	EventID        string          `json:"eventId"`
	RelationshipID string          `json:"relationshipId"`
	Generation     any             `json:"executionGeneration"`
	Attempt        any             `json:"attempt"`
	RevisionHash   string          `json:"revisionHash"`
	Outcome        string          `json:"outcome"`
	Producer       string          `json:"producer"`
	TurnRef        TurnReference   `json:"turnRef"`
	Manifest       []ManifestEntry `json:"manifest"`
	EmittedAt      string          `json:"emittedAt"`
	ManifestRef    *string         `json:"manifestRef,omitempty"`
}

func (f *intakeFixture) readyPayload(relationship Relationship, paths []string, attempt int, turn TurnReference) receiptPayload {
	f.t.Helper()
	entries, err := BuildManifest(paths, []string{f.root})
	if err != nil {
		f.t.Fatal(err)
	}
	revision, err := ManifestRevision(entries)
	if err != nil {
		f.t.Fatal(err)
	}
	event, err := EventID(relationship.ID, int(relationship.Generation), revision, string(ReadyForReview), turn.TurnID, &attempt)
	if err != nil {
		f.t.Fatal(err)
	}
	return receiptPayload{EventID: event, RelationshipID: relationship.ID, Generation: relationship.Generation, Attempt: attempt, RevisionHash: revision, Outcome: string(ReadyForReview), Producer: ProducerChild, TurnRef: turn, Manifest: entries, EmittedAt: fixtureNow}
}

func (f *intakeFixture) executionPayload(relationship Relationship, outcome string, turn TurnReference) receiptPayload {
	f.t.Helper()
	attempt := 1
	event, err := EventID(relationship.ID, int(relationship.Generation), NoDeliverable, outcome, turn.TurnID, &attempt)
	if err != nil {
		f.t.Fatal(err)
	}
	return receiptPayload{EventID: event, RelationshipID: relationship.ID, Generation: relationship.Generation, Attempt: attempt, RevisionHash: NoDeliverable, Outcome: outcome, Producer: ProducerChild, TurnRef: turn, EmittedAt: fixtureNow}
}

// rederive recomputes eventId from the payload's current fields, as the Python tests do after
// forging one field, so only the forged field can cause the refusal.
func (f *intakeFixture) rederive(p *receiptPayload) {
	f.t.Helper()
	var attempt *int
	if value, ok := p.Attempt.(int); ok {
		attempt = &value
	}
	event, err := EventID(p.RelationshipID, int(p.Generation.(int64)), p.RevisionHash, p.Outcome, p.TurnRef.TurnID, attempt)
	if err != nil {
		f.t.Fatal(err)
	}
	p.EventID = event
}

func (p receiptPayload) bytes(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// withFields re-encodes the payload after an arbitrary mutation of its object form.
func (p receiptPayload) withFields(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(p.bytes(t), &fields); err != nil {
		t.Fatal(err)
	}
	mutate(fields)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *intakeFixture) accept(payload []byte, observation TurnReference) (StoredReceipt, error) {
	return f.intake.AcceptChildReceipt(context.Background(), payload, observation)
}

func (f *intakeFixture) acceptPayload(p receiptPayload) (StoredReceipt, error) {
	return f.accept(p.bytes(f.t), p.TurnRef)
}

func (f *intakeFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.DB.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
