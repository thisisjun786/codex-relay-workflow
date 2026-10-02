package hook

import (
	"context"
	"path/filepath"
	"testing"
)

// The Stop hook's lookup is the shared stored-receipt lookup (delivery.LookupStoredReceiptAt) with
// the hook's own answers kept: a store that answers is read, a string the driver cannot bind is a
// store that could not be read (the lookup returns that exception to the omission reader, which
// reads it as evidence_unreadable, but the hook has always held, never faulted, on it), and an
// identity that is not named is a readable answer without a store.
func TestLookupReceiptAnswersAsTheStopHookAlwaysHas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := fixtureStore(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	receipt, readable, err := LookupReceipt(ctx, path, nil, "rel-none", "session", "turn", nil, nil)
	if err != nil || !readable || get(receipt, "evidence") != "relationship_absent" || get(receipt, "atCurrentHead") != false {
		t.Errorf("a relationship the store does not hold: %v readable=%v err=%v", receipt, readable, err)
	}
	receipt, readable, err = LookupReceipt(ctx, path, nil, "rel-\xff", "session", "turn", nil, nil)
	if receipt != nil || readable || err != nil {
		t.Errorf("an identity the driver cannot bind: %v readable=%v err=%v, want not readable and no error", receipt, readable, err)
	}
	asked := false
	resolver := func() (string, error) { asked = true; return path, nil }
	receipt, readable, err = LookupReceipt(ctx, "", resolver, "rel-none", "", "turn", nil, nil)
	if receipt != nil || !readable || err != nil || asked {
		t.Errorf("an unnamed session: %v readable=%v err=%v asked=%v, want readable, no answer and no store asked for", receipt, readable, err, asked)
	}
	receipt, readable, err = LookupReceipt(ctx, "", resolver, "rel-none", "session", "turn", nil, nil)
	if err != nil || !readable || get(receipt, "evidence") != "relationship_absent" || !asked {
		t.Errorf("the resolver's store: %v readable=%v err=%v asked=%v", receipt, readable, err, asked)
	}
	receipt, readable, err = LookupReceipt(ctx, "", nil, "rel-none", "session", "turn", nil, nil)
	if receipt != nil || readable || err != nil {
		t.Errorf("no store named: %v readable=%v err=%v, want not readable", receipt, readable, err)
	}
}
