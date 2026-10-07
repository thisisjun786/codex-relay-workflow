package supervisor

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The omission observer is the daemon's own observation of the store (CRW-848): a reading that is
// unmeasured because the store could not be read carries the failure's text, and a text of the
// corrupting class leaves the halt marker. The 522 and the extended corruption codes are here
// because a temporary store cannot be made to fail with them on demand, which is why the
// observation path classifies the text rather than re-reading the store.
func TestHaltOnUnreadableStoreMarksTheObservation(t *testing.T) {
	t.Parallel()
	for _, one := range []struct {
		detail string
		code   int
		halts  bool
	}{
		{"store_unreadable: disk I/O error (522)", 522, true},
		{"store_unreadable: database disk image is malformed (779)", 779, true},
		{"store_unreadable: database disk image is malformed (11)", 11, true},
		{"store_unreadable: file is not a database (26)", 26, true},
		{"store_unreadable: no such table: journal (1)", 0, false},
		{"registration_unresolved", 0, false},
	} {
		selection := store.StateSelection{Path: t.TempDir()}
		reading := delivery.Obj{{Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: one.detail}}
		haltOnUnreadableStore(context.Background(), selection, reading)
		state := store.HaltStateAt(selection.DBPath())
		if state.Present != one.halts {
			t.Errorf("%q: marker present %v, want %v (%+v)", one.detail, state.Present, one.halts, state)
			continue
		}
		if one.halts && (state.Marker.Code != one.code || state.Marker.Site != store.HaltSiteObservation) {
			t.Errorf("%q: marker %+v", one.detail, state.Marker)
		}
	}
}

// TestHaltOnUnreadableStoreLeavesOtherReadingsAlone: a reading that is not the store-unreadable shape
// changes nothing, whatever it says.
func TestHaltOnUnreadableStoreLeavesOtherReadingsAlone(t *testing.T) {
	t.Parallel()
	selection := store.StateSelection{Path: t.TempDir()}
	for _, reading := range []delivery.Obj{
		{{Key: "reportingState", Value: "unreported"}, {Key: "reason", Value: "store_unreadable: disk I/O error (522)"}},
		{{Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: "dispatch_uncorrelated"}},
		{{Key: "reportingState", Value: "unmeasured"}},
	} {
		haltOnUnreadableStore(context.Background(), selection, reading)
		if state := store.HaltStateAt(selection.DBPath()); state.Present {
			t.Fatalf("%v left a marker: %+v", reading, state)
		}
	}
}
