package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fault sweep reads a reading as plain maps: its selectors, relationship and owed field. The observer
// hands it delivery's ordered answer converted whole, nested objects included.
func TestOmissionObserverAnswersPlainMaps(t *testing.T) {
	dir := t.TempDir()
	request := faults.ManagedReadingRequest{
		Selection:  store.StateSelection{Path: filepath.Join(dir, "state")},
		Root:       filepath.Join(dir, "markers"),
		Workspace:  filepath.Join(dir, "work"),
		Assignment: delivery.AssignmentID("dispatch-1"),
		Session:    "child",
		Turn:       "turn-9",
		Now:        "2026-10-03T00:00:00.000000+00:00",
	}
	answer, err := OmissionObserver{}.Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	reading, ok := answer.(map[string]any)
	if !ok {
		t.Fatalf("the observer answered %T, want a map", answer)
	}
	// No marker was ever written for this assignment: delivery reads it as unmanaged, and owes nothing.
	if reading["schema"] != "reporting-observation/1" || reading["reportingState"] != "unmanaged" || reading["reason"] != "marker_absent" || reading["owed"] != false {
		t.Fatalf("an assignment with no marker reads %v", reading)
	}
	selectors, ok := reading["selectors"].(map[string]any)
	if !ok || selectors["turn"] != "turn-9" || selectors["session"] != "child" {
		t.Fatalf("selectors read %#v, want a map naming the session and the turn", reading["selectors"])
	}
}

// The selection is the store.StateSelection the sweep was given; anything else is an error the sweep turns
// into a gap, as the reader this one replaced did.
func TestOmissionObserverNeedsAStoreSelection(t *testing.T) {
	_, err := OmissionObserver{}.Observe(context.Background(), faults.ManagedReadingRequest{Selection: "the-selection"})
	if err == nil || !strings.HasPrefix(err.Error(), "TypeError: ") {
		t.Fatalf("a selection that is not a store selection is answered with %v", err)
	}
}
