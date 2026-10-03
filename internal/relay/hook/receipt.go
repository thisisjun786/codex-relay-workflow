package hook

import (
	"context"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// stopReceiptTimeout is how long the Stop hook waits for a lock on the store: its wall-clock budget
// is short, and ctx's deadline shortens it further.
const stopReceiptTimeout = 250 * time.Millisecond

// LookupReceipt is the Stop hook's guard.lookup_receipt: delivery.LookupStoredReceiptAt, the one
// reading of a stored receipt the omission reader shares, over the store at path (or, when path is
// empty, the one fallback names). A string the store's driver cannot bind is, as it always was
// here, a store that could not be read; only the deliverable judgment's exception and the
// live-state refusal leave as errors.
func LookupReceipt(ctx context.Context, path string, fallback func() (string, error), relationship, session, turn, generation, dispatch any) (Object, bool, error) {
	receipt, readable, err := delivery.LookupStoredReceiptAt(ctx, path, fallback, stopReceiptTimeout, delivery.ReceiptQuery{Relationship: relationship, Session: session, Turn: turn, Generation: generation, Dispatch: dispatch})
	if store.EncodeError(err) != nil {
		return nil, false, nil
	}
	return receipt, readable, err
}

// DeliverableState is guard.deliverable_state: store.DeliverableState, which the omission reader
// calls as well, so a stored receipt is judged alike by both. The store reads the values
// DecodeRecord makes of a stored receipt; this exported entry has always also accepted values a
// caller built without decoding (an object as a map, a manifest or the roots as a list of strings
// or maps), so those are normalized here to the ordered objects and lists the store reads.
// That includes the records inside the manifest: a record given as a map is read as the ordered
// object the store subscripts, and any other value is left for the store to refuse as before.
func DeliverableState(ctx context.Context, payload any, reference string, rootsValue any) (string, string, string, error) {
	if o, ok := evidence.Object(payload); ok {
		normalized := append(Object(nil), o...)
		if manifest, ok := evidence.List(o.Get("manifest")); ok {
			normalized = normalized.Set("manifest", any(orderedRecords(manifest)))
		}
		payload = normalized
	}
	if roots, ok := evidence.List(rootsValue); ok {
		rootsValue = roots
	}
	return store.DeliverableState(ctx, payload, reference, rootsValue)
}

// orderedRecords is records with every record that is a map made the ordered object
// FrozenManifestEntries subscripts; any other value is kept. It builds a new list, so neither
// the caller's list nor its maps are touched.
func orderedRecords(records []any) []any {
	out := make([]any, len(records))
	for i, record := range records {
		if o, ok := evidence.Object(record); ok {
			record = o
		}
		out[i] = record
	}
	return out
}
