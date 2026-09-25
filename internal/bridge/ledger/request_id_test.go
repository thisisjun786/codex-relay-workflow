package ledger

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// pythonRequestIDError is ledger.py:40's message (with an en dash), from a live Python run.
const pythonRequestIDError = "request_id must contain 1\u2013128 characters"

func TestRequestID_whenLengthIsCountedInCharacters(t *testing.T) {
	// Given: ledger.py:39 bounds len(request_id), which counts characters: Python's
	// Ledger.begin accepts 100 x "é" (200 UTF-8 bytes) and refuses "" and 129 x "é".
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	// When: Begin runs with a 100-character, 200-byte id. Then: it is accepted as fresh.
	fresh, _, err := l.Begin(ctx, strings.Repeat("é", 100), "m", map[string]any{}, nil)
	if err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	for _, id := range []string{"", strings.Repeat("é", 129)} {
		// When: Begin and Lookup run with an id outside the bound.
		_, _, beginErr := l.Begin(ctx, id, "m", map[string]any{}, nil)
		_, lookupErr := l.Lookup(ctx, id, "m", map[string]any{}, nil)
		// Then: both refuse with Python's exact message, as Python lookup does via _fingerprint.
		for name, err := range map[string]error{"Begin": beginErr, "Lookup": lookupErr} {
			if err == nil || err.Error() != pythonRequestIDError {
				t.Errorf("%s(%d chars): %v", name, len([]rune(id)), err)
			}
		}
	}
	// And: a 128-character id that was never stored is simply unknown to Lookup, as in Python.
	if receipt, err := l.Lookup(ctx, strings.Repeat("é", 128), "m", map[string]any{}, nil); err != nil || receipt != nil {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}
