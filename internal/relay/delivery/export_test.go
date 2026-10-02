package delivery

import (
	"context"
	"testing"
)

// ReceiptStage is a registered relationship whose head event stands for a ready receipt over one
// artifact, in a store of its own. The external tests (omitted_values_test.go) read it with
// OmissionReceipt and with the Stop hook's reader, and replace what the store holds for the
// receipt and the artifact roots, as a hand-edited store would.
type ReceiptStage struct {
	f *fixture
	// Path is the store file; the rest name the stored head event.
	Path, Relationship, Session, Turn, Dispatch, Event, ManifestRef, Artifact string
	Generation                                                                int64
}

// StageReceipt registers a relationship and accepts a ready receipt over one artifact.
func StageReceipt(t *testing.T) *ReceiptStage {
	t.Helper()
	f := newFixture(t, "")
	event := f.readyEvent(regOpts{})
	row := f.one("SELECT manifest_ref FROM events WHERE event_id = ?", event)
	return &ReceiptStage{f: f, Path: f.store.Path, Relationship: f.rid, Session: child, Turn: dispatchTurn, Dispatch: "dispatch-1", Event: event,
		ManifestRef: row.S("manifest_ref"), Artifact: f.root + "/out.txt", Generation: 1}
}

// Receipt is the stored receipt text of the head event.
func (s *ReceiptStage) Receipt() string {
	s.f.t.Helper()
	return s.f.one("SELECT receipt FROM events WHERE event_id = ?", s.Event).S("receipt")
}

// Roots is the relationship's stored artifact_roots text.
func (s *ReceiptStage) Roots() string {
	s.f.t.Helper()
	return s.f.one("SELECT artifact_roots FROM relationships WHERE relationship_id = ?", s.Relationship).S("artifact_roots")
}

// SetReceipt replaces the stored receipt text of the head event.
func (s *ReceiptStage) SetReceipt(text string) {
	s.f.t.Helper()
	n, err := execSQL(s.f.ctx, s.f.store, "UPDATE events SET receipt = ? WHERE event_id = ?", text, s.Event)
	mustDo(s.f.t, err)
	if n != 1 {
		s.f.t.Fatalf("replaced %d receipts", n)
	}
}

// SetRoots replaces the stored artifact_roots text.
func (s *ReceiptStage) SetRoots(text string) {
	s.f.t.Helper()
	n, err := execSQL(s.f.ctx, s.f.store, "UPDATE relationships SET artifact_roots = ? WHERE relationship_id = ?", text, s.Relationship)
	mustDo(s.f.t, err)
	if n != 1 {
		s.f.t.Fatalf("replaced %d roots", n)
	}
}

// OmissionReceipt is the omission reader's lookup of the stage's head event.
func (s *ReceiptStage) OmissionReceipt(ctx context.Context) (Obj, error) {
	return omissionReceipt(ctx, s.Path, s.Relationship, s.Session, s.Turn, s.Generation, s.Dispatch)
}

// OmissionReceiptFor is OmissionReceipt with the generation the assignment registered given as the
// marker would hold it, whatever its type.
func (s *ReceiptStage) OmissionReceiptFor(ctx context.Context, generation any) (Obj, error) {
	return omissionReceipt(ctx, s.Path, s.Relationship, s.Session, s.Turn, generation, s.Dispatch)
}

// OmissionReceiptFailure is the reason the omission is left unmeasured with when that lookup fails.
func OmissionReceiptFailure(err error) string { return omissionReceiptFailure(err) }
